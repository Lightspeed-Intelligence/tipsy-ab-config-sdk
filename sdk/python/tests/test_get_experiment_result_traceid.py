"""Client.get_experiment_result trace_id passthrough (design §5, SubTask C).

These tests pin down the contract that the *wire-level* exported
``Client.get_experiment_result`` method takes a ``trace_id`` keyword arg
(design §5) and threads it onto ``GetExperimentResultRequest.trace_id`` on
the outgoing proto. SDK-side normalization rule (design decision #4 +
§4 "SDK 端就地生成"):

- ``trace_id="caller-id"`` ⇒ proto ``req.trace_id == "caller-id"`` verbatim.
- ``trace_id=None`` (or empty) ⇒ SDK generates a fresh UUID, proto
  ``req.trace_id`` is the canonical 36-char dashed format and never empty.

We deliberately use the existing ``ab_servicer.last_req`` capture seam from
``conftest.FakeAbtestServicer`` rather than stubbing the transport directly:
the seam already exists, exercises the real grpc.aio path, and is what the
existing ``test_abtest_context.py`` / ``test_get_config.py`` files lean on.
"""

from __future__ import annotations

import pytest

from tipsy_ab_config import Config, UserInfo, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
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


async def test_get_experiment_result_explicit_trace_id_sent(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Caller-supplied trace_id appears verbatim on the outgoing proto."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 1}))

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
            trace_id="caller-id",
        )
        assert resp is not None
        assert ab_servicer.last_req is not None
        assert ab_servicer.last_req.trace_id == "caller-id"
    finally:
        await cli.aclose()


async def test_get_experiment_result_missing_trace_id_generates_uuid(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``trace_id=None`` ⇒ SDK auto-fills a canonical 36-char UUID on the proto."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 1}))

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
        await cli.get_experiment_result(
            "ns1",
            user_info=UserInfo(uid="u1"),
            trace_id=None,
        )
        assert ab_servicer.last_req is not None
        sent = ab_servicer.last_req.trace_id
        assert sent, "auto-filled trace_id must not be empty"
        assert _looks_like_uuid_v4(sent), (
            f"expected canonical 36-char dashed UUID, got {sent!r}"
        )
    finally:
        await cli.aclose()


async def test_get_experiment_result_empty_string_trace_id_generates_uuid(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Empty string ``trace_id=""`` ⇒ SDK treats it as missing, fills UUID."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 1}))

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
        await cli.get_experiment_result(
            "ns1",
            user_info=UserInfo(uid="u1"),
            trace_id="",
        )
        assert ab_servicer.last_req is not None
        sent = ab_servicer.last_req.trace_id
        assert sent, "empty trace_id must be replaced by SDK-side UUID"
        assert _looks_like_uuid_v4(sent)
    finally:
        await cli.aclose()


async def test_get_experiment_result_default_omitted_kwarg(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Backward compat: callers that never pass trace_id still get a non-empty UUID.

    This pins the "trace_id is optional with a sane default" contract; old
    callers (no kwarg at all) MUST keep working and the resulting proto must
    still carry a non-empty trace_id (so server-side logs always have one).
    """
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    ab_servicer.set_response("ns1", make_exp_result({"k": 1}))

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
        # No trace_id kwarg at all — mirrors any pre-existing caller.
        await cli.get_experiment_result("ns1", user_info=UserInfo(uid="u1"))
        assert ab_servicer.last_req is not None
        sent = ab_servicer.last_req.trace_id
        assert sent and _looks_like_uuid_v4(sent)
    finally:
        await cli.aclose()
