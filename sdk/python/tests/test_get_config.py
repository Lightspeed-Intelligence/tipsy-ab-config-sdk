"""get_config_static + get_config + ab->full fallback tests."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Config, init
from tipsy_ab_config.exceptions import AbtestContextMissing
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


async def test_get_config_static_hit(cfg_servicer, ab_servicer, running_servers):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {
                "hit": (10, {10: "value10"}),
                "empty": (11, {11: ""}),
            },
        )
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
        assert cli.get_config_static("ns1", "hit", "def") == "value10"
        # Empty string is valid value; must NOT fall through to default.
        assert cli.get_config_static("ns1", "empty", "def") == ""
        # Miss returns default.
        assert cli.get_config_static("ns1", "missing", "def") == "def"
        assert cli.get_config_static("other-ns", "hit", "def") == "def"
    finally:
        await cli.aclose()


async def test_get_config_abtest_hit(cfg_servicer, ab_servicer, running_servers):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    # D3: SDK no longer emits ExposureEvent; we only check that the
    # abtest-resolved version reaches GetConfig. The deprecated `exposures`
    # field is retained on the wire (D1) but server永不再填充, so the
    # response carries config_flat_kv only.
    resp = make_exp_result({"k": 2})
    ab_servicer.set_response("ns1", resp)

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
        abctx = cli.new_abtest_context("u1", {"country": "US"})
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "ab-v2"
    finally:
        await cli.aclose()


async def test_get_config_full_fallback_when_abtest_unavailable(
    cfg_servicer, ab_servicer, running_servers
):
    import grpc

    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
    )
    ab_servicer.set_error("ns1", grpc.StatusCode.UNAVAILABLE)
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
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full"
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_get_config_ab_version_missing_in_cache_falls_back(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full-only"})})
    )
    # GetExperimentResult says version 99 but the cache doesn't have it.
    ab_servicer.set_response("ns1", make_exp_result({"k": 99}))

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
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full-only"
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_get_config_raises_without_abtest_context(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})})
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
        with pytest.raises(AbtestContextMissing):
            # No ctx passed and no contextvar set -> raises.
            await cli.get_config(None, "ns1", "k", "def")
    finally:
        await cli.aclose()


async def test_get_config_static_after_close_returns_default(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})})
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
    await cli.aclose()
    assert cli.get_config_static("ns1", "k", "def") == "def"


# ---- has_dynamic_resolution fast path (ST4) ----
#
# RPC-count assertion mechanism: the FakeAbtestServicer (conftest) increments
# ``calls`` (total) and ``calls_by_ns[ns]`` on every GetExperimentResult. The
# fast path's contract is "ZERO GetExperimentResult RPCs", which we assert as
# ``ab_servicer.calls == 0`` AND ``ab_servicer.calls_by_ns.get(ns, 0) == 0``.
#
# Memoization false-green avoidance: ``wait_for_abtest`` memoises the result
# per-ns into the AbtestContext, so the FIRST get_config in a link is the only
# one that can fire the RPC; a later call in the SAME ctx is satisfied from the
# memo and would fire nothing even WITHOUT the fast path. To prove the fast
# path itself skips the RPC we therefore:
#   1. use a FRESH abctx whose memo is empty,
#   2. query ONLY the explicit-False key in that ns on that ctx (no other
#      true/absent key in the same link that would have triggered the per-ns
#      RPC and left a green memo behind),
#   3. assert the per-ns counter is exactly 0.


async def test_get_config_fast_path_false_skips_abtest_rpc(
    cfg_servicer, ab_servicer, running_servers
):
    """Explicit has_dynamic_resolution=False ⇒ ZERO GetExperimentResult RPCs."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {"pure": (10, {10: "full10"})},
            has_dynamic_resolution={"pure": False},
        )
    )
    # Arm a response so that IF the SDK wrongly issued the RPC and somehow hit,
    # the value would differ from the full release — making a regression loud.
    ab_servicer.set_response("ns1", make_exp_result({"pure": 10}))
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
        # FRESH ctx, and we query ONLY the false key in this ns on this link.
        abctx = cli.new_abtest_context("u-fast")
        val = await cli.get_config(abctx, "ns1", "pure", "def")
        assert val == "full10"
        # The whole point: not a single GetExperimentResult was issued.
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
    finally:
        await cli.aclose()


async def test_get_config_fast_path_false_no_full_returns_default(
    cfg_servicer, ab_servicer, running_servers
):
    """False flag + no full release ⇒ default, still ZERO RPCs."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {"pure": (None, {})},  # no full_release_version, no versions
            has_dynamic_resolution={"pure": False},
        )
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
        abctx = cli.new_abtest_context("u-fast2")
        val = await cli.get_config(abctx, "ns1", "pure", "the-default")
        assert val == "the-default"
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
    finally:
        await cli.aclose()


async def test_get_config_true_flag_still_awaits_abtest(
    cfg_servicer, ab_servicer, running_servers
):
    """has_dynamic_resolution=True ⇒ slow path: RPC issued, ab value wins."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {"k": (1, {1: "full", 2: "ab-v2"})},
            has_dynamic_resolution={"k": True},
        )
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
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
        abctx = cli.new_abtest_context("u-slow", {"country": "US"})
        val = await cli.get_config(abctx, "ns1", "k", "def")
        # ab hit resolves to the experiment version, not the full release.
        assert val == "ab-v2"
        # The RPC MUST have been issued (no regression of the slow path).
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
    finally:
        await cli.aclose()


async def test_get_config_absent_flag_still_awaits_abtest(
    cfg_servicer, ab_servicer, running_servers
):
    """Field ABSENT (old server) ⇒ safe default: RPC still issued, ab value wins."""
    cfg_addr, ab_addr = running_servers
    # No has_dynamic_resolution arg at all ⇒ field UNSET on the wire (None).
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
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
        abctx = cli.new_abtest_context("u-absent", {"country": "US"})
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "ab-v2"
        # Safe default: an absent flag must NOT skip abtest.
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
    finally:
        await cli.aclose()
