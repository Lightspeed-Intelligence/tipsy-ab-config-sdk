"""new_abtest_context / abtest_scope trace_id kwarg (design §5, SubTask C).

Pins the public-API contract:

- ``Client.new_abtest_context(uid, ..., trace_id="caller-id")`` ⇒
  ``ctx.trace_id == "caller-id"`` (no normalization, no rewriting).
- ``Client.new_abtest_context(uid)`` (no kwarg) ⇒ ``ctx.trace_id`` is a
  SDK-generated, canonical 36-char dashed UUID.
- ``Client.abtest_scope`` mirrors the same surface; the yielded context's
  ``trace_id`` follows the same rules.
- ``empty_abtest_context`` and ``mock_abtest_context`` still produce a
  non-empty trace_id (every request link must carry one, even off-user
  paths and tests).
"""

from __future__ import annotations

import pytest

from tipsy_ab_config import Config, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_snapshot,
)


def _looks_like_uuid_v4(s: str) -> bool:
    if not isinstance(s, str) or len(s) != 36:
        return False
    if s[8] != "-" or s[13] != "-" or s[18] != "-" or s[23] != "-":
        return False
    try:
        int(s.replace("-", ""), 16)
    except ValueError:
        return False
    return True


async def test_new_abtest_context_explicit_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
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
        ctx = cli.new_abtest_context(
            "u1", {"country": "US"}, trace_id="caller-id"
        )
        assert ctx.trace_id == "caller-id"
    finally:
        await cli.aclose()


async def test_new_abtest_context_auto_generates_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
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
        ctx = cli.new_abtest_context("u1")
        assert _looks_like_uuid_v4(ctx.trace_id)
    finally:
        await cli.aclose()


async def test_abtest_scope_explicit_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``async with cli.abtest_scope(uid, ..., trace_id=...)`` honours explicit id."""
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
        async with cli.abtest_scope(
            "u1", {"country": "US"}, trace_id="scope-trace"
        ) as ctx:
            assert ctx.trace_id == "scope-trace"
    finally:
        await cli.aclose()


async def test_abtest_scope_auto_generates_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``abtest_scope`` without trace_id ⇒ canonical UUID."""
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
        async with cli.abtest_scope("u1") as ctx:
            assert _looks_like_uuid_v4(ctx.trace_id)
    finally:
        await cli.aclose()


async def test_empty_abtest_context_has_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Even identity-less ctxs carry a non-empty trace_id.

    The design says every request link has one trace_id so logs always
    have a stitch point. The empty ctx still represents *some* call site;
    leaving it empty would create a class of requests that can never be
    grepped by trace_id.
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
        ctx = cli.empty_abtest_context()
        assert _looks_like_uuid_v4(ctx.trace_id)
    finally:
        await cli.aclose()


async def test_mock_abtest_context_has_trace_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``mock_abtest_context`` test helper also carries a trace_id."""
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
        ctx = cli.mock_abtest_context("u1", {"ns1": {"k": 1}})
        # Mock contexts don't issue RPCs, but a non-empty trace_id is still
        # required so the design invariant "every ctx has one" holds.
        assert ctx.trace_id and _looks_like_uuid_v4(ctx.trace_id)
    finally:
        await cli.aclose()
