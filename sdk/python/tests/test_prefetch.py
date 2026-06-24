"""Explicit prefetch API + construction-RPC=0 tests (design G1 / G2 / D2-PY).

Covers the prefetch decouple slice for the Python SDK:

- Construction (`new_abtest_context`) issues ZERO GetExperimentResult RPC
  before any dynamic `get_config`.
- The explicit `AbtestContext.prefetch_config_version_flat_kv_for_namespace`
  warm-up fires EXACTLY ONE RPC and a subsequent `get_config` for that ns
  REUSES it (at-most-once: total stays 1).
- Prefetch is synchronous + non-blocking: it returns immediately and the
  in-flight result is resolved later by the awaiting `get_config`.
- Prefetch is idempotent: two prefetch calls for the same ns => still 1 RPC.
- Empty / mock ctx and an unsubscribed ns short-circuit inside
  `_ensure_fetch` without issuing any RPC.
- The prefetch RPC carries the AbtestContext's `trace_id`.
- Concurrency: many tasks racing first-access (mix of prefetch + get_config)
  on the same not-yet-fetched ns => exactly 1 RPC (shared `_NsResult` slot).

Prefetch requires a running event loop (`asyncio.ensure_future`); every test
here is async (pytest-asyncio), matching the in-memory grpc/capture harness in
``conftest.py``.
"""

from __future__ import annotations

import asyncio

import pytest

from tipsy_ab_config import Config, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


async def _init_client(
    cfg_servicer: FakeConfigServicer,
    cfg_addr: str,
    ab_addr: str,
    *,
    namespaces=("ns1",),
    default_namespace: str = "",
    abtest_timeout: float = 1.5,
):
    return await init(
        Config(
            namespaces=list(namespaces),
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            default_namespace=default_namespace,
            abtest_timeout=abtest_timeout,
            pull_interval=10.0,
            pull_retries=1,
        )
    )


async def test_construction_issues_zero_rpc_before_get_config(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """new_abtest_context(uid, attrs) issues no RPC before any get_config (G1)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await _init_client(cfg_servicer, cfg_addr, ab_addr, default_namespace="ns1")
    try:
        before = ab_servicer.calls
        ctx = cli.new_abtest_context("u1", {"country": "US"})
        # No RPC at construction; give a stray task a window to disprove it.
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
        # First get_config pays the single lazy RPC.
        assert await cli.get_config(ctx, "ns1", "k", "def") == "ab"
        assert ab_servicer.calls - before == 1
    finally:
        await cli.aclose()


async def test_prefetch_then_get_config_reuses_single_rpc(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Explicit prefetch fires 1 RPC; a later get_config reuses it (total 1)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    # Add latency so the prefetch genuinely outlives the synchronous call and
    # the awaiting get_config resolves the same in-flight task.
    ab_servicer.delay = 0.05
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await _init_client(
        cfg_servicer, cfg_addr, ab_addr, abtest_timeout=2.0
    )
    try:
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        ctx = cli.new_abtest_context("u1")

        # Prefetch is synchronous + non-blocking: it returns immediately
        # (its return value is None) and does NOT await the in-flight RPC.
        ret = ctx.prefetch_config_version_flat_kv_for_namespace("ns1")
        assert ret is None

        # The later get_config awaits and resolves the SAME task: at-most-once.
        assert await cli.get_config(ctx, "ns1", "k", "def") == "ab"
        after = ab_servicer.calls_by_ns.get("ns1", 0)
        assert after - before == 1, (
            f"prefetch + get_config must total exactly 1 RPC, got {after - before}"
        )
    finally:
        await cli.aclose()


async def test_prefetch_idempotent_same_ns_one_rpc(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Two prefetch calls for the same ns => still exactly 1 RPC (idempotent)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    ab_servicer.delay = 0.05
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await _init_client(cfg_servicer, cfg_addr, ab_addr, abtest_timeout=2.0)
    try:
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        ctx = cli.new_abtest_context("u1")
        ctx.prefetch_config_version_flat_kv_for_namespace("ns1")
        ctx.prefetch_config_version_flat_kv_for_namespace("ns1")
        # Let the single in-flight task settle, then prove only one RPC issued.
        assert await cli.get_config(ctx, "ns1", "k", "def") == "ab"
        after = ab_servicer.calls_by_ns.get("ns1", 0)
        assert after - before == 1, (
            f"idempotent prefetch must issue exactly 1 RPC, got {after - before}"
        )
    finally:
        await cli.aclose()


async def test_prefetch_empty_ctx_issues_zero_rpc(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Prefetch on an empty (identity-less) ctx short-circuits — no RPC."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await _init_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        before = ab_servicer.calls
        ctx = cli.empty_abtest_context()
        ctx.prefetch_config_version_flat_kv_for_namespace("ns1")
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_prefetch_unsubscribed_ns_issues_zero_rpc(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Prefetch on an unsubscribed ns short-circuits inside _ensure_fetch — no RPC."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await _init_client(cfg_servicer, cfg_addr, ab_addr, namespaces=("ns1",))
    try:
        before = ab_servicer.calls
        ctx = cli.new_abtest_context("u1")
        # "ns-other" is NOT in the subscription set: prefetch must not raise and
        # must not issue any RPC (mirrors resultFor short-circuit).
        ctx.prefetch_config_version_flat_kv_for_namespace("ns-other")
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_prefetch_rpc_carries_ctx_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """The prefetch RPC forwards the AbtestContext's trace_id verbatim."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await _init_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        ctx = cli.new_abtest_context("u1", trace_id="prefetch-trace-xyz")
        ctx.prefetch_config_version_flat_kv_for_namespace("ns1")
        # Await the warmed result through get_config so the in-flight RPC has
        # definitely landed at the servicer before we inspect last_req.
        assert await cli.get_config(ctx, "ns1", "k", "def") == "ab"
        assert ab_servicer.last_req is not None
        assert ab_servicer.last_req.trace_id == "prefetch-trace-xyz"
        assert ab_servicer.last_req.namespace == "ns1"
    finally:
        await cli.aclose()


async def test_prefetch_and_get_config_race_one_rpc(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Many tasks racing first-access (prefetch + get_config) same ns => 1 RPC."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    # Latency makes the concurrent first-accessors genuinely race in-flight.
    ab_servicer.delay = 0.08
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await _init_client(cfg_servicer, cfg_addr, ab_addr, abtest_timeout=2.0)
    try:
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        ctx = cli.new_abtest_context("u1")

        async def via_get_config():
            return await cli.get_config(ctx, "ns1", "k", "def")

        async def via_prefetch():
            # Sync, non-blocking trigger; resolve through a follow-up await.
            ctx.prefetch_config_version_flat_kv_for_namespace("ns1")
            return await cli.get_config(ctx, "ns1", "k", "def")

        coros = [via_get_config() for _ in range(8)] + [
            via_prefetch() for _ in range(8)
        ]
        vals = await asyncio.gather(*coros)
        after = ab_servicer.calls_by_ns.get("ns1", 0)
        assert after - before == 1, (
            f"racing prefetch + get_config must issue exactly 1 RPC, "
            f"got {after - before}"
        )
        assert all(v == "ab" for v in vals)
    finally:
        await cli.aclose()
