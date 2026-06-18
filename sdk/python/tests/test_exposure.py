"""Async exposure + 5min dedup tests for the Python SDK."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config.exposure import ExposureEmitter, ExposureEvent
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2


class CollectingSink:
    def __init__(self) -> None:
        self.events: list[ExposureEvent] = []

    def __call__(self, ev: ExposureEvent) -> None:
        self.events.append(ev)


def _exposure(key: str, version: int, source: str = "experiment_group", **kw) -> abtest_pb2.Exposure:
    e = abtest_pb2.Exposure(key=key, version=version, source=source)
    for k, v in kw.items():
        setattr(e, k, v)
    return e


async def _drain(emitter: ExposureEmitter, sink: CollectingSink, count: int, timeout: float = 1.5):
    end = asyncio.get_event_loop().time() + timeout
    while len(sink.events) < count and asyncio.get_event_loop().time() < end:
        await asyncio.sleep(0.02)


async def test_basic_emit():
    sink = CollectingSink()
    e = ExposureEmitter(sink=sink, ttl_seconds=60)
    await e.start()
    try:
        e.emit("u1", "ns1", "k", 10, [_exposure("k", 10, experiment_id="100", group_id="200")])
        await _drain(e, sink, 1)
        assert len(sink.events) == 1
        ev = sink.events[0]
        assert ev.user_id == "u1"
        assert ev.namespace == "ns1"
        assert ev.key == "k"
        assert ev.version == 10
        assert ev.source == "experiment_group"
        assert ev.experiment_id == "100"
        assert ev.group_id == "200"
    finally:
        await e.aclose()


async def test_dedup_within_window():
    sink = CollectingSink()
    now = [1000.0]

    def clock() -> float:
        return now[0]

    e = ExposureEmitter(sink=sink, ttl_seconds=300, clock=clock)
    await e.start()
    try:
        e.emit("u1", "ns1", "k", 10, [])
        e.emit("u1", "ns1", "k", 10, [])
        e.emit("u1", "ns1", "k", 10, [])
        await _drain(e, sink, 1)
        await asyncio.sleep(0.05)
        # Only the first emission survives dedup.
        assert len(sink.events) == 1

        # Advance past TTL → next emit goes through.
        now[0] += 301.0
        e.emit("u1", "ns1", "k", 10, [])
        await _drain(e, sink, 2)
        assert len(sink.events) == 2
    finally:
        await e.aclose()


async def test_dedup_distinguishes_dimensions():
    sink = CollectingSink()
    e = ExposureEmitter(sink=sink, ttl_seconds=60)
    await e.start()
    try:
        e.emit("u1", "ns1", "k", 10, [])
        e.emit("u1", "ns1", "k", 11, [])      # different version
        e.emit("u1", "ns1", "other", 10, [])  # different key
        e.emit("u2", "ns1", "k", 10, [])      # different uid
        await _drain(e, sink, 4, timeout=2.0)
        assert len(sink.events) == 4
    finally:
        await e.aclose()


async def test_pick_exposure_for_key_default_source_when_no_match():
    sink = CollectingSink()
    e = ExposureEmitter(sink=sink, ttl_seconds=60)
    await e.start()
    try:
        # abExposures doesn't contain (k, 10), default attribution kicks in.
        e.emit("u1", "ns1", "k", 10, [_exposure("other", 99, source="whitelist")])
        await _drain(e, sink, 1)
        assert sink.events[0].source == "experiment_group"
        # Without ids set, they remain empty (v2 TEXT ids).
        assert sink.events[0].experiment_id == ""
    finally:
        await e.aclose()


async def test_whitelist_source_attribution():
    sink = CollectingSink()
    e = ExposureEmitter(sink=sink, ttl_seconds=60)
    await e.start()
    try:
        e.emit("u1", "ns1", "k", 10, [_exposure("k", 10, source="whitelist", release_id=77)])
        await _drain(e, sink, 1)
        assert sink.events[0].source == "whitelist"
        assert sink.events[0].release_id == 77
    finally:
        await e.aclose()


async def test_queue_overflow_drops_silently():
    # blocking sink so events accumulate in queue
    block = asyncio.Event()

    async def slow(ev: ExposureEvent) -> None:
        await block.wait()

    e = ExposureEmitter(sink=slow, ttl_seconds=60, queue_size=4)
    await e.start()
    try:
        for i in range(64):
            # unique versions to defeat dedup
            e.emit("u", "ns", "k", i, [])
        # No crash — drops happen silently.
        block.set()
        await asyncio.sleep(0.1)
    finally:
        await e.aclose()


async def test_sink_exception_recovered():
    calls = {"n": 0}

    def boom(ev: ExposureEvent) -> None:
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("boom")

    e = ExposureEmitter(sink=boom, ttl_seconds=60)
    await e.start()
    try:
        e.emit("u1", "ns", "k", 1, [])
        e.emit("u1", "ns", "k", 2, [])
        for _ in range(50):
            await asyncio.sleep(0.02)
            if calls["n"] >= 2:
                break
        assert calls["n"] >= 2
    finally:
        await e.aclose()


async def test_async_sink_supported():
    seen: list[ExposureEvent] = []

    async def async_sink(ev: ExposureEvent) -> None:
        seen.append(ev)

    e = ExposureEmitter(sink=async_sink, ttl_seconds=60)
    await e.start()
    try:
        e.emit("u1", "ns", "k", 1, [])
        for _ in range(50):
            await asyncio.sleep(0.02)
            if seen:
                break
        assert len(seen) == 1
    finally:
        await e.aclose()


async def test_close_idempotent():
    e = ExposureEmitter(ttl_seconds=60)
    await e.start()
    await e.aclose()
    await e.aclose()
