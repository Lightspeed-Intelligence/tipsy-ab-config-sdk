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
    make_exp_result,
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
    ab_servicer.set_response("ns1", make_exp_result({"k1": 2, "k2": 2}))

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
    ab_servicer.set_response("ns1", make_exp_result({"k": 99}))
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
