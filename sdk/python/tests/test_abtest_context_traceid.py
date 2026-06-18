"""AbtestContext.trace_id tests (design §5, SubTask C).

Covers the per-AbtestContext trace_id field that codingAgent is adding to
:class:`tipsy_ab_config.abtest_context.AbtestContext` and to the
``Client.new_abtest_context`` / ``Client.abtest_scope`` factories.

Contract under test (design §5 / decision #2 / decision #4):

- Explicit ``trace_id="<value>"`` is preserved verbatim on the context.
- Missing / empty trace_id (``None`` or ``""``) triggers SDK-side
  ``str(uuid.uuid4())`` so ``ctx.trace_id`` is the canonical 36-char dashed
  UUID v4 string and never empty.
- The trace_id stored on the context is the value sent on every subsequent
  ``GetExperimentResult`` for the request link (eager prefetch + lazy
  per-ns).
"""

from __future__ import annotations

import pytest

from tipsy_ab_config import Config, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


def _looks_like_uuid_v4(s: str) -> bool:
    """Return True when ``s`` is the canonical 36-char dashed UUID format.

    Matches design decision #4: ``uuid.New().String()`` / ``str(uuid.uuid4())``
    output — 8-4-4-4-12 hex groups separated by dashes at positions
    8 / 13 / 18 / 23. We don't strictly validate the version nibble because
    the design only mandates "UUID format", not v4 specifically (Go / Python
    happen to both use v4).
    """
    if not isinstance(s, str) or len(s) != 36:
        return False
    if s[8] != "-" or s[13] != "-" or s[18] != "-" or s[23] != "-":
        return False
    hex_chars = s.replace("-", "")
    if len(hex_chars) != 32:
        return False
    try:
        int(hex_chars, 16)
    except ValueError:
        return False
    return True


async def test_explicit_trace_id_preserved(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Caller-supplied trace_id is held verbatim on the AbtestContext."""
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
        ctx = cli.new_abtest_context("u1", trace_id="explicit-trace-id")
        assert ctx.trace_id == "explicit-trace-id"
    finally:
        await cli.aclose()


async def test_missing_trace_id_generates_uuid(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``trace_id=None`` triggers SDK-side UUID generation (non-empty, 36 chars)."""
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
        ctx = cli.new_abtest_context("u1")  # trace_id omitted ⇒ default None
        assert ctx.trace_id, "trace_id must not be empty when caller did not supply one"
        assert _looks_like_uuid_v4(ctx.trace_id), (
            f"expected canonical 36-char dashed UUID, got {ctx.trace_id!r}"
        )
    finally:
        await cli.aclose()


async def test_empty_trace_id_generates_uuid(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``trace_id=""`` (empty string) is equivalent to missing — UUID is generated."""
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
        ctx = cli.new_abtest_context("u1", trace_id="")
        assert ctx.trace_id, "empty string trace_id must be replaced by SDK-side UUID"
        assert _looks_like_uuid_v4(ctx.trace_id)
    finally:
        await cli.aclose()


async def test_each_context_has_unique_uuid(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Two contexts created back-to-back must get distinct generated UUIDs.

    Sanity check: a stale module-level UUID would make every request share
    the same trace_id and defeat the whole feature.
    """
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
        ctx_a = cli.new_abtest_context("u1")
        ctx_b = cli.new_abtest_context("u2")
        assert ctx_a.trace_id != ctx_b.trace_id
        assert _looks_like_uuid_v4(ctx_a.trace_id)
        assert _looks_like_uuid_v4(ctx_b.trace_id)
    finally:
        await cli.aclose()


async def test_stored_trace_id_flows_to_experiment_result_fetch(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """The trace_id stashed on AbtestContext is the one sent on Compute.

    Drives the lazy per-ns fetch path (``_get_experiment_result_for_ns``)
    via a normal ``get_config`` so the eager-prefetch / lazy split doesn't
    matter — both sites must read the AbtestContext's trace_id.
    """
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
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
        ctx = cli.new_abtest_context("u1", trace_id="ctx-trace-xyz")
        val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "ab"
        assert ab_servicer.last_req is not None
        assert ab_servicer.last_req.trace_id == "ctx-trace-xyz", (
            "AbtestContext.trace_id must be forwarded as proto req.trace_id"
        )
    finally:
        await cli.aclose()
