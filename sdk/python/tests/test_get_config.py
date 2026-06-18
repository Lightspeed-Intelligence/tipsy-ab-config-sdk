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
