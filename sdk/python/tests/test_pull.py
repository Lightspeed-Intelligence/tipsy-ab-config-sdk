"""PullAll + 10s fallback PullAll tests for the Python SDK."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Client, Config, StartupPullFailed, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_snapshot,
)


async def test_pull_all_happy_path(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (10, {10: "v10"})})
    )
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=0.05,
            pull_timeout=2.0,
            pull_retries=1,
        )
    )
    try:
        assert cli.cache.full_release_version("ns1", "k") == 10
        assert cli.cache.value_of("ns1", "k", 10) == "v10"
    finally:
        await cli.aclose()


async def test_pull_all_multi_namespace(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    cfg_addr, ab_addr = running_servers
    for ns in ("nsA", "nsB", "nsC"):
        cfg_servicer.set_pull_snapshot(
            make_snapshot(ns, 1, 1, {"k": (1, {1: "v"})})
        )
    cli = await init(
        Config(
            namespaces=["nsB", "nsA", "nsC"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_timeout=2.0,
            pull_retries=1,
        )
    )
    try:
        for ns in ("nsA", "nsB", "nsC"):
            assert cli.cache.full_release_version(ns, "k") == 1
        assert cli.namespaces == ["nsA", "nsB", "nsC"]
    finally:
        await cli.aclose()


async def test_pull_all_fail_close(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    import grpc

    cfg_addr, ab_addr = running_servers
    cfg_servicer.pull_error = grpc.StatusCode.UNAVAILABLE
    with pytest.raises(StartupPullFailed):
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr=cfg_addr,
                abtest_service_addr=ab_addr,
                token=issue_test_token(),
                pull_retries=1,
                pull_timeout=1.0,
                startup_fail_open=False,
            )
        )


async def test_pull_all_fail_open_metric(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    import grpc

    cfg_addr, ab_addr = running_servers
    cfg_servicer.pull_error = grpc.StatusCode.UNAVAILABLE
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_retries=1,
            pull_timeout=1.0,
            startup_fail_open=True,
            pull_interval=10.0,
        )
    )
    try:
        assert cli.metrics.cache_empty_total() == 1
        assert cli.metrics.pull_failure_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_fallback_pull_loop_refreshes_cache(
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
            pull_interval=0.05,  # compressed
            pull_timeout=2.0,
            pull_retries=1,
        )
    )
    try:
        # Update the fake to advance seq.
        cfg_servicer.set_pull_snapshot(
            make_snapshot("ns1", 2, 2, {"k": (2, {2: "v2"})})
        )
        # Wait for the fallback timer to refresh.
        for _ in range(40):
            await asyncio.sleep(0.05)
            if cli.cache.full_release_version("ns1", "k") == 2:
                break
        assert cli.cache.full_release_version("ns1", "k") == 2
    finally:
        await cli.aclose()


async def test_init_namespace_validation():
    with pytest.raises(ValueError):
        await init(Config(namespaces=[], config_service_addr="x", token="t"))


async def test_init_token_validation(
    cfg_servicer, ab_servicer, running_servers
):
    cfg_addr, _ = running_servers
    with pytest.raises(ValueError):
        await init(Config(namespaces=["ns1"], config_service_addr=cfg_addr))
