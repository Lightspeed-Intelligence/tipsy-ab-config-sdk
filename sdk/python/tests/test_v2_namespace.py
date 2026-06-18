"""v2 namespace + userInfo semantics tests for the Python SDK.

Mirrors the Go SDK's ``v2_namespace_test.go`` 1:1 (design 04 §B.1–§B.6,
decision A-3):

- env / Config default-ns resolution + NamespaceRequired
- eager pre-request fires ONLY for the prefetch ns (config_version + flat_kv)
- no default ns ⇒ no eager RPC
- per-ns at-most-once: concurrent get_config on the same ctx ⇒ exactly 1 RPC
- ns-optional get_config preserves the full-release fallback (M6)
- user_info accessor carries uid + attrs
- get_experiment_result client forwards all params
- resolved-but-unsubscribed ns ⇒ NamespaceNotSubscribed
"""

from __future__ import annotations

import asyncio

import pytest

from tipsy_ab_config import (
    Config,
    NamespaceNotSubscribed,
    NamespaceRequired,
    UserInfo,
    init,
)
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


async def _wait_for(predicate, timeout: float = 2.0) -> bool:
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(0.01)
    return predicate()


async def test_get_config_default_namespace_required(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """No explicit ns AND no default ns ⇒ NamespaceRequired (design 04 §B.1)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})})
    )
    cli = await init(
        Config(
            namespaces=["ns1"],  # no default_namespace, env unset
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        abctx = cli.new_abtest_context("u1")
        with pytest.raises(NamespaceRequired):
            await cli.get_config_default(abctx, "k", "def")
        # Explicit-ns get_config with empty ns also errors.
        with pytest.raises(NamespaceRequired):
            await cli.get_config(abctx, "", "k", "def")
        with pytest.raises(NamespaceRequired):
            await cli.get_config(abctx, None, "k", "def")
    finally:
        await cli.aclose()


async def test_default_namespace_from_config(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Configured default ns drives ns-optional get_config + eager pre-request."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full-v1", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            default_namespace="ns1",
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        assert cli.default_namespace == "ns1"
        abctx = cli.new_abtest_context("u1")
        # Eager pre-request fires for the default ns; get_config_default hits it.
        val = await cli.get_config_default(abctx, "k", "def")
        assert val == "ab-v2"
    finally:
        await cli.aclose()


async def test_default_namespace_from_env(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    monkeypatch,
):
    """The SDK reads the PROJECT_DEFAULT_NAMESPACE env var once at init."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full-v1", 2: "ab-v2"})})
    )
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    monkeypatch.setenv("PROJECT_DEFAULT_NAMESPACE", "ns1")
    cli = await init(
        Config(
            namespaces=["ns1"],  # no default_namespace override → falls to env
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        assert cli.default_namespace == "ns1"
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config_default(abctx, "k", "def")
        assert val == "ab-v2"
    finally:
        await cli.aclose()


async def test_new_abtest_context_eager_prefetch_shape(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Eager pre-request targets ONLY the default ns + config_version + flat_kv."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cfg_servicer.set_pull_snapshot(make_snapshot("ns2", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1", "ns2"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            default_namespace="ns1",
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        before = ab_servicer.calls
        _ = cli.new_abtest_context("u1")
        assert await _wait_for(lambda: ab_servicer.calls_by_ns.get("ns1", 0) > 0), (
            "expected eager pre-request for default ns1"
        )
        # Only the default ns is pre-fetched; ns2 must NOT be requested eagerly.
        assert ab_servicer.calls_by_ns.get("ns2", 0) == 0
        assert ab_servicer.calls - before == 1
        req = ab_servicer.last_req
        assert req.namespace == "ns1"
        assert (
            req.experiment_type
            == abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION
        )
        assert (
            req.display_type
            == abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_FLAT_KV
        )
    finally:
        await cli.aclose()


async def test_new_abtest_context_no_default_no_eager_rpc(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """With no default ns the constructor fires no eager RPC (design 04 §B.2)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],  # no default ns
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        before = ab_servicer.calls
        _ = cli.new_abtest_context("u1")
        # Give any (erroneous) eager task a moment.
        await asyncio.sleep(0.05)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_result_for_concurrent_at_most_once(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Feedback-point-3: concurrent get_config on the SAME not-yet-fetched ns
    ⇒ exactly ONE GetExperimentResult RPC (the rest await the shared task)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    # Add latency so concurrent first-accessors genuinely race in-flight.
    ab_servicer.delay = 0.08
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            abtest_timeout=2.0,
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        # No default ns ⇒ no eager pre-request; the lazy path is exercised.
        abctx = cli.new_abtest_context("u1")

        async def one():
            return await cli.get_config(abctx, "ns1", "k", "def")

        vals = await asyncio.gather(*[one() for _ in range(16)])
        after = ab_servicer.calls_by_ns.get("ns1", 0)
        assert after - before == 1, (
            f"expected exactly 1 RPC across racing tasks, got {after - before}"
        )
        assert all(v == "ab" for v in vals)
    finally:
        await cli.aclose()


async def test_get_config_full_fallback_preserved_for_unhit_key(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """M6: a key NOT in config_flat_kv resolves to the full-release version
    (not the default), even though the abtest result was fetched for the ns."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1,
            {
                "hit": (1, {1: "full-hit", 2: "ab-hit"}),
                "unhit": (5, {5: "full-unhit"}),
            },
        )
    )
    # Experiment only hits "hit"; "unhit" is absent from config_flat_kv.
    ab_servicer.set_response("ns1", make_exp_result({"hit": 2}))
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
        # hit -> ab version
        assert await cli.get_config(abctx, "ns1", "hit", "def") == "ab-hit"
        # unhit -> full release (NOT default), reusing the same memoised result.
        assert await cli.get_config(abctx, "ns1", "unhit", "def") == "full-unhit"
        # missing key -> default.
        assert await cli.get_config(abctx, "ns1", "missing", "def") == "def"
    finally:
        await cli.aclose()


async def test_user_info_accessor(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """user_info accessor carries uid + attrs (design 04 §B.4)."""
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
    try:
        attrs = {"country": "US", "tier": 3}
        abctx = cli.new_abtest_context("u42", attrs)
        ui = abctx.user_info
        assert ui.uid == "u42"
        assert ui.attrs["country"] == "US"
        assert ui.attrs["tier"] == 3
        # user_id back-compat still works.
        assert abctx.user_id == "u42"
    finally:
        await cli.aclose()


async def test_get_experiment_result_client(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """The thin get_experiment_result client forwards all params (design 04 §B.6)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 7}))
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
        resp = await cli.get_experiment_result(
            "ns1",
            user_info=UserInfo(uid="u1", attrs={"country": "US"}),
            layer_ids=["L1", "L2"],
            experiment_type=abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CUSTOM_PARAMS,
            display_type=abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP,
        )
        assert resp.config_flat_kv["k"] == 7
        req = ab_servicer.last_req
        assert req.namespace == "ns1"
        assert req.user_id == "u1"
        assert list(req.layer_ids) == ["L1", "L2"]
        assert (
            req.experiment_type
            == abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CUSTOM_PARAMS
        )
        assert (
            req.display_type
            == abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP
        )
        assert req.user_attrs["country"].s == "US"

        # ns-optional + no default ⇒ NamespaceRequired.
        with pytest.raises(NamespaceRequired):
            await cli.get_experiment_result(None)
    finally:
        await cli.aclose()


async def test_resolve_namespace_not_subscribed(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """A resolved-but-unsubscribed ns is rejected (design 04 §B.1 validation)."""
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
    try:
        abctx = cli.new_abtest_context("u1")
        with pytest.raises(NamespaceNotSubscribed):
            await cli.get_config(abctx, "ns-not-subscribed", "k", "def")
    finally:
        await cli.aclose()
