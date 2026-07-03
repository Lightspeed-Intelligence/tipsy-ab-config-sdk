"""Per-call Debug timing logs at the two GetExperimentResult sites (ST3).

Mirrors the Go SDK's按次计时日志: both call sites emit exactly one
``logging.DEBUG`` record after the RPC completes (success or failure),
carrying ``ns`` / ``trace_id`` / ``duration_ms`` — dual-written into BOTH the
message string (纯文本 grep-able, symmetric with Java) AND ``extra={}`` (so
``caplog`` exposes them as record attributes). ``err`` is附带 on failure only.

Both sites are driven through the existing ``running_servers`` /
``FakeAbtestServicer`` seam (real grpc.aio round-trip, so ``duration_ms`` is a
genuine non-zero float) rather than stubbing the transport:

- public ``Client.get_experiment_result`` — the ``asyncio.wait_for`` site;
- lazy ``_fetch_config_version_flat_kv_for_ns`` — reached via
  ``Client.get_config`` → ``AbtestContext.wait_for_abtest`` (the per-ns
  memoised fetch the config fast path uses).

The logger is ``logging.getLogger("tipsy_ab_config")`` (module-level
``logger`` in ``client.py``); ``caplog.at_level(..., logger="tipsy_ab_config")``
scopes capture to it.
"""

from __future__ import annotations

import logging

import grpc
import pytest

from tipsy_ab_config import Config, UserInfo, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)

_RPC_MSG = "GetExperimentResult rpc"


def _timing_records(caplog: pytest.LogCaptureFixture) -> list[logging.LogRecord]:
    """All captured records whose rendered message is the RPC timing line."""
    return [r for r in caplog.records if _RPC_MSG in r.getMessage()]


async def _make_client(cfg_addr: str, ab_addr: str):
    return await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )


async def test_public_get_experiment_result_success_emits_one_timing_debug(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    caplog: pytest.LogCaptureFixture,
):
    """Success on the public path ⇒ exactly one DEBUG record with the timing
    fields dual-written (message + extra), duration_ms a float ≥ 0, and NO
    ``err`` attribute."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 1}))

    cli = await _make_client(cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.DEBUG, logger="tipsy_ab_config"):
            resp = await cli.get_experiment_result(
                "ns1",
                user_info=UserInfo(uid="u1", attrs={"country": "US"}),
                trace_id="trace-success",
            )
        assert resp is not None

        recs = _timing_records(caplog)
        assert len(recs) == 1, (
            f"expected exactly one timing DEBUG record, got {len(recs)}"
        )
        rec = recs[0]

        # message string carries the fields (纯文本 grep-able).
        msg = rec.getMessage()
        assert "duration_ms=" in msg
        assert "ns=" in msg
        assert "trace-success" in msg

        # extra={} exposes them as record attributes for structured consumers.
        assert rec.ns == "ns1"
        assert rec.trace_id == "trace-success"
        assert isinstance(rec.duration_ms, float)
        assert rec.duration_ms >= 0.0

        # success ⇒ no err attribute at all.
        assert not hasattr(rec, "err")
    finally:
        await cli.aclose()


async def test_public_get_experiment_result_failure_emits_err_debug_and_propagates(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    caplog: pytest.LogCaptureFixture,
):
    """Failure on the public path ⇒ a DEBUG record carrying err info, and the
    original ``AioRpcError`` still propagates (纯增日志: exception type &
    propagation unchanged)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_error("ns1", grpc.StatusCode.UNAVAILABLE)

    cli = await _make_client(cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.DEBUG, logger="tipsy_ab_config"):
            with pytest.raises(grpc.aio.AioRpcError):
                await cli.get_experiment_result(
                    "ns1",
                    user_info=UserInfo(uid="u1"),
                    trace_id="trace-fail",
                )

        recs = _timing_records(caplog)
        assert len(recs) == 1, (
            f"expected exactly one timing DEBUG record, got {len(recs)}"
        )
        rec = recs[0]

        assert rec.ns == "ns1"
        assert rec.trace_id == "trace-fail"
        assert isinstance(rec.duration_ms, float)
        assert rec.duration_ms >= 0.0

        # err is present on failure — either as an extra attr or in the message.
        has_err_attr = getattr(rec, "err", None) not in (None, "")
        assert has_err_attr or "err" in rec.getMessage().lower(), (
            "failure record must carry err info (extra attr or message)"
        )
    finally:
        await cli.aclose()


async def test_lazy_fetch_path_emits_timing_debug(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    caplog: pytest.LogCaptureFixture,
):
    """The per-ns lazy fetch (_fetch_config_version_flat_kv_for_ns), reached via
    ``get_config`` → ``wait_for_abtest``, emits its own timing DEBUG with
    ns + trace_id + duration_ms."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))

    cli = await _make_client(cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.DEBUG, logger="tipsy_ab_config"):
            abctx = cli.new_abtest_context("u1", {"country": "US"}, trace_id="trace-lazy")
            val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "ab-v2"

        recs = _timing_records(caplog)
        # The lazy fetch is memoised at-most-once per link, so exactly one
        # timing line for this ns.
        assert len(recs) == 1, (
            f"expected one lazy-path timing DEBUG record, got {len(recs)}"
        )
        rec = recs[0]

        assert rec.ns == "ns1"
        assert rec.trace_id == "trace-lazy"
        assert isinstance(rec.duration_ms, float)
        assert rec.duration_ms >= 0.0
        assert not hasattr(rec, "err")
    finally:
        await cli.aclose()


async def test_lazy_fetch_failure_emits_err_debug_and_degrades_silently(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    caplog: pytest.LogCaptureFixture,
):
    """Lazy-path RPC failure ⇒ a timing DEBUG carrying err, AND the call
    degrades to full release (does NOT propagate — the existing swallow-and-
    fallback contract is preserved; only the debug line is新增)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
    )
    ab_servicer.set_error("ns1", grpc.StatusCode.UNAVAILABLE)

    cli = await _make_client(cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.DEBUG, logger="tipsy_ab_config"):
            abctx = cli.new_abtest_context("u1", trace_id="trace-lazy-fail")
            # Falls back to full release; must not raise.
            val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full"

        recs = _timing_records(caplog)
        assert len(recs) == 1, (
            f"expected one lazy-path timing DEBUG record, got {len(recs)}"
        )
        rec = recs[0]

        assert rec.ns == "ns1"
        assert rec.trace_id == "trace-lazy-fail"
        assert isinstance(rec.duration_ms, float)

        has_err_attr = getattr(rec, "err", None) not in (None, "")
        assert has_err_attr or "err" in rec.getMessage().lower(), (
            "lazy failure record must carry err info (extra attr or message)"
        )
    finally:
        await cli.aclose()


async def test_no_timing_records_at_info_level(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    caplog: pytest.LogCaptureFixture,
):
    """At INFO the timing line is silent — the DEBUG-level requirement (Info 级
    零新增噪音) holds."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 1}))

    cli = await _make_client(cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.INFO, logger="tipsy_ab_config"):
            resp = await cli.get_experiment_result(
                "ns1",
                user_info=UserInfo(uid="u1"),
                trace_id="trace-info",
            )
        assert resp is not None
        assert _timing_records(caplog) == [], (
            "no GetExperimentResult timing line may appear at INFO level"
        )
    finally:
        await cli.aclose()
