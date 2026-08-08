"""Bearer-auth-on-the-wire tests for every gRPC method kind (issue #8).

The defect these lock down: ``_AuthInterceptor`` used to be ONE class deriving
from both ``grpc.aio.UnaryUnaryClientInterceptor`` and
``grpc.aio.UnaryStreamClientInterceptor``. grpcio sorts interceptors into
per-method-kind lists with an **if/elif chain** (``grpc/aio/_channel.py``,
``Channel.__init__``), so that single instance only ever landed in the
unary-unary list — the unary-stream list stayed empty and
``ConfigService.Subscribe`` went out with **no ``authorization`` header**. A real
server answered ``Unauthenticated: missing authorization metadata`` and the SDK
reconnect-looped forever, silently degrading push to ``pull_interval`` polling.

The tests assert on metadata **as the server received it**, per method kind.
Asserting that ``intercept_unary_stream`` merely exists would have passed while
the bug was live, so that is deliberately not what is checked here.
"""

from __future__ import annotations

import asyncio

import pytest

from tipsy_ab_config import Config, init

from .conftest import FakeAbtestServicer, FakeConfigServicer, issue_test_token, make_snapshot

grpc = pytest.importorskip("grpc", reason="grpcio required for the interceptor wiring tests")

from tipsy_ab_config.client import (  # noqa: E402  (after importorskip)
    _AuthUnaryStreamInterceptor,
    _AuthUnaryUnaryInterceptor,
    _build_channel,
    _TokenCache,
)


async def _wait_until(predicate, timeout=2.0, step=0.05):
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


# ===========================================================================
# 1. End-to-end: the header the SERVER actually receives, per method kind.
# ===========================================================================


async def test_subscribe_and_pull_both_carry_authorization_header(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Server-streaming Subscribe carries ``authorization: Bearer <token>``,
    exactly like unary PullAll.

    This is the direct regression test for issue #8: pre-fix, ``pull_metadata``
    held the header and ``subscribe_metadata`` did not.
    """
    cfg_addr, ab_addr = running_servers
    token = issue_test_token()
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=token,
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        assert ok, "Subscribe never attached"

        assert cfg_servicer.pull_metadata, "PullAll was never called"
        assert cfg_servicer.pull_metadata[0].get("authorization") == "Bearer " + token, (
            "unary PullAll must carry the bearer header"
        )

        # The actual issue-#8 assertion.
        assert cfg_servicer.subscribe_metadata[0].get("authorization") == "Bearer " + token, (
            "server-streaming Subscribe must carry the bearer header; a missing "
            "one is what made the server reply 'missing authorization metadata' "
            "and put the SDK in an endless reconnect loop (issue #8)"
        )
    finally:
        await cli.aclose()


async def test_subscribe_reconnect_reads_token_per_attempt(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Every Subscribe attempt — including reconnects — carries the bearer
    header, resolved from the token cache at call time.

    Two things are locked in: a reconnect is not a hole in auth coverage, and
    the interceptor reads ``_TokenCache.current()`` per call rather than
    capturing the value once, so a token refreshed mid-flight lands on the next
    attempt instead of a stale one being pinned.
    """
    cfg_addr, ab_addr = running_servers
    first, rotated = issue_test_token(subject="first"), issue_test_token(subject="rotated")

    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))
    cfg_servicer.fail_after_first_push = True

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=first,
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        assert ok, "Subscribe never attached"
        assert cfg_servicer.subscribe_metadata[0].get("authorization") == "Bearer " + first

        # Rotate the cached token, then force a stream error → reconnect.
        cli._auth._cached = rotated
        cfg_servicer.push_snapshot(make_snapshot("ns1", 2, 2, {"k": (2, {2: "v2"})}))
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 2, timeout=5.0)
        assert ok, "Subscribe never reconnected"

        for i, md in enumerate(cfg_servicer.subscribe_metadata):
            got = md.get("authorization")
            assert got is not None and got.startswith("Bearer ") and got != "Bearer ", (
                f"Subscribe attempt #{i} reached the server without a usable "
                f"bearer header (got {got!r})"
            )
        assert cfg_servicer.subscribe_metadata[-1].get("authorization") == "Bearer " + rotated, (
            "the reconnect must use the token current at call time, not the one "
            "cached when the interceptor was constructed"
        )
    finally:
        await cli.aclose()


# ===========================================================================
# 2. Channel wiring: grpcio's per-kind interceptor lists are both populated.
# ===========================================================================


async def test_build_channel_registers_one_interceptor_per_method_kind():
    """Both of grpcio's per-kind interceptor lists get an auth interceptor.

    Guards the root cause directly: with one multi-inheritance instance,
    ``_unary_stream_interceptors`` came back empty because grpcio's registration
    if/elif stops at the first matching branch. No server is dialled — building
    the channel is enough to inspect how grpcio sorted the chain.
    """
    cfg = Config(namespaces=["ns1"], config_service_addr="dns:///example:18081", token="tok")
    ch = _build_channel(cfg, cfg.config_service_addr, _TokenCache("tok", None))
    try:
        assert any(
            isinstance(i, _AuthUnaryUnaryInterceptor) for i in ch._unary_unary_interceptors
        ), "no auth interceptor registered for unary-unary calls"
        assert any(
            isinstance(i, _AuthUnaryStreamInterceptor) for i in ch._unary_stream_interceptors
        ), (
            "no auth interceptor registered for unary-stream calls — Subscribe "
            "would go out unauthenticated (issue #8)"
        )
    finally:
        await ch.close()


def test_auth_interceptor_classes_are_single_purpose():
    """Neither auth interceptor class inherits more than one interceptor ABC.

    Re-merging them into one multi-inheritance class would silently reintroduce
    issue #8, since grpcio would register the instance under one kind only. This
    fails fast at the class level if someone does.
    """
    abcs = (
        grpc.aio.UnaryUnaryClientInterceptor,
        grpc.aio.UnaryStreamClientInterceptor,
        grpc.aio.StreamUnaryClientInterceptor,
        grpc.aio.StreamStreamClientInterceptor,
    )
    for cls in (_AuthUnaryUnaryInterceptor, _AuthUnaryStreamInterceptor):
        implemented = [abc for abc in abcs if issubclass(cls, abc)]
        assert len(implemented) == 1, (
            f"{cls.__name__} implements {[a.__name__ for a in implemented]}; grpcio "
            f"registers an interceptor under only ONE method kind, so each class "
            f"must implement exactly one interceptor ABC"
        )
