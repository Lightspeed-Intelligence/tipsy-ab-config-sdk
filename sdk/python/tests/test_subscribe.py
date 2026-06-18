"""Subscribe stream + reconnect tests for the Python SDK."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Config, init

from .conftest import FakeAbtestServicer, FakeConfigServicer, issue_test_token, make_snapshot


async def _wait_until(predicate, timeout=2.0, step=0.05):
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


async def test_subscribe_applies_pushed_snapshot(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})})
    )
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        assert ok, "Subscribe never attached"

        cfg_servicer.push_snapshot(
            make_snapshot("ns1", 5, 5, {"k": (2, {2: "v2"})})
        )
        ok = await _wait_until(
            lambda: cli.cache.full_release_version("ns1", "k") == 2
        )
        assert ok, "cache did not reflect pushed snapshot"
        assert cli.metrics.subscribe_event_received_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_subscribe_known_seqs_sent(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 3, 4))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        ok = await _wait_until(lambda: len(cfg_servicer.subscribe_reqs) >= 1)
        assert ok
        req = cfg_servicer.subscribe_reqs[-1]
        seqs = req.known_seqs["ns1"]
        assert seqs.business_snapshot_seq == 3
        assert seqs.experiment_snapshot_seq == 4
    finally:
        await cli.aclose()


async def test_subscribe_reconnect_after_error(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cfg_servicer.fail_after_first_push = True
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        # Wait for first Subscribe.
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        assert ok
        # Push triggers the error.
        cfg_servicer.push_snapshot(make_snapshot("ns1", 2, 2))
        # Subscribe must reconnect; backoff starts at 1s so allow ~3s.
        ok = await _wait_until(
            lambda: cfg_servicer.subscribe_calls >= 2, timeout=5.0
        )
        assert ok, f"reconnect did not happen; calls={cfg_servicer.subscribe_calls}"
        assert cli.metrics.subscribe_disconnect_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_aclose_drains_quickly(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
    # aclose should complete within a short window (no deadlock).
    await asyncio.wait_for(cli.aclose(), timeout=5.0)
