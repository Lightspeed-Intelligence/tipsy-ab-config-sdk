"""get_all_configs / get_all_configs_default + uid-shortcut get-all tests.

Written against the design contract in
``.zyz-worker/tasks/abconfig-sdk-get-all-configs/design.md`` (Proposed Design
§2 + semantics table + Testing Plan 1-10). The get-all path is pure-client:
one per-(request, ns) memoised ``GetExperimentResult`` (the SAME one the single
key ``get_config`` uses), then a per-key walk of the SAME captured snapshot —
ab hit (whitelist/experiment) > full-release fallback > key dropped.

RPC-count assertion mechanism mirrors test_get_config.py: the FakeAbtestServicer
increments ``calls`` (total) and ``calls_by_ns[ns]`` on every
GetExperimentResult, so "zero RPC" is ``ab_servicer.calls == 0`` AND
``calls_by_ns.get(ns, 0) == 0``, and "exactly one" is a before/after delta.
"""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Config, init
from tipsy_ab_config.exceptions import (
    AbtestContextMissing,
    NamespaceNotSubscribed,
    NamespaceRequired,
    SDKClosed,
)

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_per_group_result,
    make_snapshot,
)


async def _init(
    cfg_addr: str,
    ab_addr: str,
    *,
    namespaces=("ns1",),
    default_namespace: str = "",
):
    return await init(
        Config(
            namespaces=list(namespaces),
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
            default_namespace=default_namespace,
        )
    )


def _matrix_snapshot():
    """One ns exercising every resolution branch at once (plan 1).

    ``ab-hit``        full v1="full-ab", ab v2="ab-v2"  → ab wins.
    ``ab-miss-cache`` full v1="full-only"; ab points at v99 (not cached) → WARN
                      + abtest_fallback + falls to full "full-only".
    ``full-only``     no ab entry → full "full5".
    ``neither``       no full release, no versions → DROPPED from the map.
    ``empty``         full v7="" → present as the empty string (valid value).
    """
    return make_snapshot(
        "ns1",
        1,
        1,
        {
            "ab-hit": (1, {1: "full-ab", 2: "ab-v2"}),
            "ab-miss-cache": (1, {1: "full-only"}),
            "full-only": (5, {5: "full5"}),
            "neither": (None, {}),
            "empty": (7, {7: ""}),
        },
    )


# ---------------------------------------------------------------------------
# Plan 1 — resolution matrix in a single namespace.
# ---------------------------------------------------------------------------


async def test_get_all_configs_resolution_matrix(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(_matrix_snapshot())
    # ab-hit → v2 (cached); ab-miss-cache → v99 (NOT cached, forces ab→full).
    ab_servicer.set_response(
        "ns1", make_per_group_result({"ab-hit": 2, "ab-miss-cache": 99})
    )
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1", {"country": "US"})
        got = await cli.get_all_configs(abctx, "ns1")
        assert got == {
            "ab-hit": "ab-v2",       # experiment version wins
            "ab-miss-cache": "full-only",  # ab version missing → full fallback
            "full-only": "full5",    # no ab entry → full release
            "empty": "",             # empty string is a valid value, kept
            # "neither" DROPPED: no ab hit and no full release.
        }
        assert "neither" not in got
        # ab→full fallback for ab-miss-cache bumped the per-ns counter.
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
        # A single at-most-once GetExperimentResult served the whole map.
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 2 — get_all_configs_default (project default namespace).
# ---------------------------------------------------------------------------


async def test_get_all_configs_default_uses_default_namespace(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab2"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
    cli = await _init(cfg_addr, ab_addr, default_namespace="ns1")
    try:
        abctx = cli.new_abtest_context("u1")
        got = await cli.get_all_configs_default(abctx)
        assert got == {"k": "ab2"}
    finally:
        await cli.aclose()


async def test_get_all_configs_default_no_default_ns_raises(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
    )
    # No default_namespace configured ⇒ NamespaceRequired family.
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        with pytest.raises(NamespaceRequired):
            await cli.get_all_configs_default(abctx)
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 3 — error paths.
# ---------------------------------------------------------------------------


async def test_get_all_configs_unsubscribed_ns_raises(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})})
    )
    cli = await _init(cfg_addr, ab_addr, namespaces=("ns1",))
    try:
        abctx = cli.new_abtest_context("u1")
        with pytest.raises(NamespaceNotSubscribed):
            await cli.get_all_configs(abctx, "ns2")
    finally:
        await cli.aclose()


async def test_get_all_configs_missing_ctx_no_contextvar_raises(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})})
    )
    cli = await _init(cfg_addr, ab_addr)
    try:
        # ctx=None AND no abtest_ctx_var set ⇒ AbtestContextMissing (same as
        # get_config; the None→contextvar fallback finds nothing).
        with pytest.raises(AbtestContextMissing):
            await cli.get_all_configs(None, "ns1")
    finally:
        await cli.aclose()


async def test_get_all_configs_after_close_raises(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})})
    )
    cli = await _init(cfg_addr, ab_addr)
    abctx = cli.new_abtest_context("u1")
    await cli.aclose()
    with pytest.raises(SDKClosed):
        await cli.get_all_configs(abctx, "ns1")


async def test_get_all_configs_cancellation_propagates(
    cfg_servicer, ab_servicer, running_servers
):
    """A cancelled asyncio task awaiting get_all_configs re-raises CancelledError."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab2"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
    ab_servicer.delay = 5.0  # keep the in-flight RPC pending so we can cancel
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        task = asyncio.ensure_future(cli.get_all_configs(abctx, "ns1"))
        await asyncio.sleep(0.05)  # let the coroutine reach the abtest await
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 4 — subscribed but no snapshot ⇒ empty map, zero RPC.
# ---------------------------------------------------------------------------


async def test_get_all_configs_no_snapshot_returns_empty(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    # Subscribed to ns1 but the config server has NO snapshot to hand back, so
    # the cache stays empty for ns1 after startup PullAll.
    cli = await _init(cfg_addr, ab_addr, namespaces=("ns1",))
    try:
        abctx = cli.new_abtest_context("u1")
        got = await cli.get_all_configs(abctx, "ns1")
        assert got == {}
        # No snapshot ⇒ the get-all path returns early WITHOUT issuing an RPC.
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 5 — at-most-once RPC across get_config + get_all_configs on one ctx.
# ---------------------------------------------------------------------------


async def test_get_config_then_get_all_configs_shares_one_rpc(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {
                "k1": (1, {1: "f1", 2: "ab2"}),
                "k2": (1, {1: "g1"}),
            },
        )
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k1": 2}))
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        # First a single-key get_config (fires the one per-ns RPC + memoises).
        v = await cli.get_config(abctx, "ns1", "k1", "")
        assert v == "ab2"
        # Then get_all_configs on the SAME ctx+ns reuses the memoised result.
        got = await cli.get_all_configs(abctx, "ns1")
        assert got == {"k1": "ab2", "k2": "g1"}
        assert ab_servicer.calls_by_ns.get("ns1", 0) - before == 1
    finally:
        await cli.aclose()


async def test_get_all_configs_all_false_flags_zero_rpc(
    cfg_servicer, ab_servicer, running_servers
):
    """Every key has_dynamic_resolution explicitly False ⇒ ns fast-path, 0 RPC."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {"a": (1, {1: "av"}), "b": (2, {2: "bv"})},
            has_dynamic_resolution={"a": False, "b": False},
        )
    )
    # Arm a response that WOULD differ from full so a wrongly-issued RPC is loud.
    ab_servicer.set_response("ns1", make_per_group_result({"a": 1, "b": 2}))
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        got = await cli.get_all_configs(abctx, "ns1")
        assert got == {"a": "av", "b": "bv"}
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 6 — uid "" / "0" shortcut for get_all_configs (pure full-release).
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("uid", ["", "0"])
async def test_get_all_configs_no_user_uid_pure_full_release(
    uid, cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(_matrix_snapshot())
    # Arm a response whose ab values DIFFER from full release; the shortcut must
    # mean these are never applied (ab-hit would be "ab-v2" if the RPC fired).
    ab_servicer.set_response(
        "ns1", make_per_group_result({"ab-hit": 2, "ab-miss-cache": 99})
    )
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context(uid)
        got = await cli.get_all_configs(abctx, "ns1")
        # Pure full-release resolution for every key.
        assert got == {
            "ab-hit": "full-ab",
            "ab-miss-cache": "full-only",
            "full-only": "full5",
            "empty": "",
        }
        assert "neither" not in got
        # Shortcut = no RPC and NOT a degradation, so no fallback metric.
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
        assert cli.metrics.abtest_fallback_total("ns1") == 0
    finally:
        await cli.aclose()


async def test_get_all_configs_normal_uid_still_issues_rpc(
    cfg_servicer, ab_servicer, running_servers
):
    """Regression: a real uid ("1") still fires the RPC and applies ab hits."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(_matrix_snapshot())
    ab_servicer.set_response("ns1", make_per_group_result({"ab-hit": 2}))
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("1")
        got = await cli.get_all_configs(abctx, "ns1")
        assert got["ab-hit"] == "ab-v2"  # experiment version applied
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
    finally:
        await cli.aclose()


async def test_get_all_configs_mock_ctx_empty_uid_seeded_wins(
    cfg_servicer, ab_servicer, running_servers
):
    """mock_abtest_context(uid="") pre-seeded per-ns result still resolves.

    The uid shortcut only affects the *unresolved* lazy path; a seeded ns slot
    is used as-is (no RPC either, since mock ctx never fetches).
    """
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 9: "ab9"})})
    )
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.mock_abtest_context("", {"ns1": {"k": 9}})
        got = await cli.get_all_configs(abctx, "ns1")
        assert got == {"k": "ab9"}
        assert ab_servicer.calls == 0
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 7 — returned dict is a fresh, caller-owned copy.
# ---------------------------------------------------------------------------


async def test_get_all_configs_returned_map_is_independent(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "orig"})})
    )
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        first = await cli.get_all_configs(abctx, "ns1")
        assert first == {"k": "orig"}
        # Mutate the returned map; it must not leak into the cache/next call.
        first["k"] = "mutated"
        first["injected"] = "x"
        second = await cli.get_all_configs(abctx, "ns1")
        assert second == {"k": "orig"}
        assert "injected" not in second
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 8 — mixed fast-path within a namespace.
# ---------------------------------------------------------------------------


async def test_get_all_configs_mixed_fast_path_one_rpc(
    cfg_servicer, ab_servicer, running_servers
):
    """At least one non-False key ⇒ abtest consulted exactly once for the ns."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {
                "pureF": (1, {1: "f1"}),
                "dyn": (2, {2: "f2", 3: "ab3"}),
            },
            has_dynamic_resolution={"pureF": False, "dyn": True},
        )
    )
    ab_servicer.set_response("ns1", make_per_group_result({"dyn": 3}))
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        got = await cli.get_all_configs(abctx, "ns1")
        # pureF has no ab entry ⇒ full; dyn hits the experiment version.
        assert got == {"pureF": "f1", "dyn": "ab3"}
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 9 — merged abtest version == 0 is not a hit (no WARN / no fallback).
# ---------------------------------------------------------------------------


async def test_get_all_configs_zero_version_uses_full(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (3, {3: "full3"})})
    )
    # A merged version of 0 means "no experiment hit" — must resolve to full and
    # must NOT bump the ab→full fallback metric.
    ab_servicer.set_response("ns1", make_per_group_result({"k": 0}))
    cli = await _init(cfg_addr, ab_addr)
    try:
        abctx = cli.new_abtest_context("u1")
        got = await cli.get_all_configs(abctx, "ns1")
        assert got == {"k": "full3"}
        assert cli.metrics.abtest_fallback_total("ns1") == 0
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Plan 10 — ctx=None resolves via abtest_ctx_var contextvar fallback.
# ---------------------------------------------------------------------------


async def test_get_all_configs_none_ctx_uses_contextvar(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab2"})})
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 2}))
    cli = await _init(cfg_addr, ab_addr)
    try:
        # abtest_scope stashes the ctx in abtest_ctx_var for the block.
        async with cli.abtest_scope("u1", {"country": "US"}):
            got = await cli.get_all_configs(None, "ns1")
            assert got == {"k": "ab2"}
    finally:
        await cli.aclose()
