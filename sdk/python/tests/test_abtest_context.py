"""AbtestContext per-(uid,ns) at-most-once memoise tests (design 04 §B.3)."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Config, init
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_per_group_result,
    make_snapshot,
)


async def test_one_compute_per_ns_per_request(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {
                "k1": (1, {1: "a", 2: "b"}),
                "k2": (1, {1: "c", 2: "d"}),
            },
        )
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k1": 2, "k2": 2}))

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
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        abctx = cli.new_abtest_context("u1")
        v1 = await cli.get_config(abctx, "ns1", "k1", "")
        v2 = await cli.get_config(abctx, "ns1", "k2", "")
        assert v1 == "b" and v2 == "d"
        after = ab_servicer.calls_by_ns.get("ns1", 0)
        # Exactly one GetExperimentResult even though two get_config calls
        # happened (per-ns memoise, at-most-once per request link).
        assert after - before == 1, (
            f"expected 1 GetExperimentResult, got {after - before}"
        )
    finally:
        await cli.aclose()


async def test_empty_abtest_context_does_not_compute(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
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
        before = ab_servicer.calls
        abctx = cli.empty_abtest_context()
        val = await cli.get_config(abctx, "ns1", "k", "")
        assert val == "full"
        assert ab_servicer.calls == before
    finally:
        await cli.aclose()


async def test_mock_abtest_context_resolves(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 9: "ab9"})})
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
        abctx = cli.mock_abtest_context("u1", {"ns1": {"k": 9}})
        val = await cli.get_config(abctx, "ns1", "k", "")
        assert val == "ab9"
    finally:
        await cli.aclose()


async def test_abtest_timeout_degrades_silently(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 99}))
    ab_servicer.delay = 0.5  # 500ms — well past timeout

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            abtest_timeout=0.05,  # 50ms
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full"
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_abtest_scope_sets_contextvar(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    from tipsy_ab_config import abtest_ctx_var

    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
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
        async with cli.abtest_scope("u1", {"country": "US"}):
            ctx = abtest_ctx_var.get()
            assert ctx is not None
            assert ctx.user_id == "u1"
            val = await cli.get_config(None, "ns1", "k", "def")
            assert val == "full"
        # After scope exits the contextvar resets.
        assert abtest_ctx_var.get() is None
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# uid ""/"0" shortcut (design §1): no-user uid ⇒ zero GetExperimentResult RPCs
# across get_config / prefetch / wait_for_abtest; pure full-release resolution.
# The shortcut lives in the lazy-fetch layer (_ensure_fetch), so it covers every
# path that flows through it — mirrors EmptyAbtestContext, but keyed on uid.
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("uid", ["", "0"])
async def test_get_config_no_user_uid_skips_abtest(
    uid, cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    # Arm an ab hit that WOULD win if the RPC fired — the shortcut must skip it.
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
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
        abctx = cli.new_abtest_context(uid)
        val = await cli.get_config(abctx, "ns1", "k", "def")
        # Pure static resolution: the full-release value, never the ab version.
        assert val == "full"
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
        # A proactive shortcut, not a degradation ⇒ no fallback metric.
        assert cli.metrics.abtest_fallback_total("ns1") == 0
    finally:
        await cli.aclose()


@pytest.mark.parametrize("uid", ["", "0"])
async def test_get_config_no_user_uid_no_full_returns_default(
    uid, cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    # No full release for the key ⇒ single-key get_config returns the default.
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (None, {2: "ab-only"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
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
        abctx = cli.new_abtest_context(uid)
        val = await cli.get_config(abctx, "ns1", "k", "the-default")
        assert val == "the-default"
        assert ab_servicer.calls == 0
    finally:
        await cli.aclose()


async def test_get_config_normal_uid_still_calls_abtest(
    cfg_servicer, ab_servicer, running_servers
):
    """Regression: uid="1" is a real identity ⇒ RPC fires, ab hit wins."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
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
        abctx = cli.new_abtest_context("1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "ab-v2"
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
    finally:
        await cli.aclose()


@pytest.mark.parametrize("uid", ["", "0"])
async def test_prefetch_no_user_uid_zero_rpc(
    uid, cfg_servicer, ab_servicer, running_servers
):
    """prefetch flows through _ensure_fetch ⇒ no-user uid issues no RPC."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
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
        abctx = cli.new_abtest_context(uid)
        abctx.prefetch_config_version_flat_kv_for_namespace("ns1")
        # wait_for_abtest returns the empty result without ever fetching.
        result = await abctx.wait_for_abtest("ns1")
        assert result.key_versions == {}
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
    finally:
        await cli.aclose()


async def test_abtest_context_none_user_id_normalises_and_shortcuts(
    cfg_servicer, ab_servicer, running_servers
):
    """AbtestContext(user_id=None) normalises to "" and takes the shortcut.

    Guards the design §1 note: a None uid must normalise to the empty string
    (so proto encoding never sees None) AND fall into the no-user shortcut.
    """
    from tipsy_ab_config import AbtestContext

    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
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
        abctx = AbtestContext(user_id=None, owner=cli)
        assert abctx.user_id == ""
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full"
        assert ab_servicer.calls == 0
    finally:
        await cli.aclose()


async def test_encode_value_types():
    from tipsy_ab_config.client import _encode_value, _encode_user_attrs

    assert _encode_value("hi") is not None
    assert _encode_value(42) is not None
    assert _encode_value(1.5) is not None
    assert _encode_value(True) is not None
    # bool must be detected before int because isinstance(True, int) == True.
    assert _encode_value(True).b is True
    # Unsupported types dropped.
    assert _encode_value([1, 2]) is None
    assert _encode_value({"x": 1}) is None
    assert _encode_value(None) is None

    out = _encode_user_attrs({"s": "x", "i": 1, "bad": object()})
    assert "s" in out
    assert "i" in out
    assert "bad" not in out
