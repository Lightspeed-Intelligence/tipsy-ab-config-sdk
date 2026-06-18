"""Background PullAll / Subscribe trace_id generation (design §5, decision #1).

Background SDK loops aren't tied to any inbound request, so they generate a
fresh trace_id per RPC. Contract under test:

- ``Client._pull_once`` (the worker behind startup + 10s periodic PullAll)
  populates ``PullAllRequest.trace_id`` with a non-empty, SDK-generated
  canonical 36-char dashed UUID.
- ``Client._subscribe_once`` populates ``SubscribeRequest.trace_id`` with
  the same kind of UUID.

We can't read the trace_id off the existing ``FakeConfigServicer`` from
``conftest.py`` because it only records ``subscribe_reqs`` — PullAll
requests aren't kept. Rather than mutate the shared conftest fake (and risk
collateral on other tests), we subclass it locally and add a
``pull_reqs`` list, then wire two private servers and a ``Client`` against
them. Subscribe coverage piggybacks on the existing ``subscribe_reqs``
capture seam.
"""

from __future__ import annotations

import asyncio
import contextlib
import socket
from typing import List, Optional

import grpc
import grpc.aio
import pytest

from tipsy_ab_config import Config, init
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2_grpc
from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2, config_pb2_grpc

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


def _free_port() -> int:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class CapturingConfigServicer(FakeConfigServicer):
    """Adds ``pull_reqs`` capture on top of the shared FakeConfigServicer.

    We can't add this to ``conftest.FakeConfigServicer`` without churning
    every other test file — subclass scope keeps the change local.
    """

    def __init__(self) -> None:
        super().__init__()
        self.pull_reqs: List[config_pb2.PullAllRequest] = []

    async def PullAll(self, request, context):
        # Defensive copy so the SDK can't mutate the captured value out from
        # under us later.
        kept = config_pb2.PullAllRequest()
        kept.CopyFrom(request)
        self.pull_reqs.append(kept)
        return await super().PullAll(request, context)


@pytest.fixture
async def capturing_servers():
    """Start a capturing ConfigService + a vanilla AbtestService.

    Yields ``(cfg_servicer, cfg_addr, ab_addr)`` so tests can drive both
    the PullAll and Subscribe loops and inspect captured requests on
    ``cfg_servicer.pull_reqs`` / ``.subscribe_reqs``.
    """
    cfg_servicer = CapturingConfigServicer()
    ab_servicer = FakeAbtestServicer()

    cfg_server = grpc.aio.server()
    config_pb2_grpc.add_ConfigServiceServicer_to_server(cfg_servicer, cfg_server)
    cfg_port = _free_port()
    cfg_addr = f"127.0.0.1:{cfg_port}"
    cfg_server.add_insecure_port(cfg_addr)
    await cfg_server.start()

    ab_server = grpc.aio.server()
    abtest_pb2_grpc.add_AbtestServiceServicer_to_server(ab_servicer, ab_server)
    ab_port = _free_port()
    ab_addr = f"127.0.0.1:{ab_port}"
    ab_server.add_insecure_port(ab_addr)
    await ab_server.start()

    try:
        yield cfg_servicer, cfg_addr, ab_addr
    finally:
        with contextlib.suppress(Exception):
            await cfg_server.stop(grace=None)
        with contextlib.suppress(Exception):
            await ab_server.stop(grace=None)


async def _wait_until(predicate, timeout=2.0, step=0.05):
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


async def test_pull_all_request_carries_uuid_trace_id(capturing_servers):
    """Startup PullAll fills ``PullAllRequest.trace_id`` with a fresh UUID."""
    cfg_servicer, cfg_addr, ab_addr = capturing_servers
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
        # init() runs the startup PullAll synchronously, so at least one
        # captured request is guaranteed by the time init() returns.
        assert len(cfg_servicer.pull_reqs) >= 1, "startup PullAll must run"
        req = cfg_servicer.pull_reqs[0]
        assert req.trace_id, "background PullAll must populate trace_id"
        assert _looks_like_uuid_v4(req.trace_id), (
            f"expected canonical 36-char dashed UUID, got {req.trace_id!r}"
        )
    finally:
        await cli.aclose()


async def test_pull_all_each_invocation_gets_fresh_trace_id(capturing_servers):
    """Each periodic PullAll generates an independent trace_id (no caching)."""
    cfg_servicer, cfg_addr, ab_addr = capturing_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=0.05,  # tight cadence so the fallback loop fires
            pull_retries=1,
        )
    )
    try:
        # Wait for at least two PullAll requests to land (startup + ≥1 loop).
        ok = await _wait_until(lambda: len(cfg_servicer.pull_reqs) >= 2)
        assert ok, (
            f"expected >=2 PullAll requests; "
            f"got {len(cfg_servicer.pull_reqs)}"
        )
        trace_ids = [r.trace_id for r in cfg_servicer.pull_reqs[:2]]
        for tid in trace_ids:
            assert tid and _looks_like_uuid_v4(tid)
        assert trace_ids[0] != trace_ids[1], (
            "each periodic PullAll must generate a fresh trace_id"
        )
    finally:
        await cli.aclose()


async def test_subscribe_request_carries_uuid_trace_id(capturing_servers):
    """The Subscribe stream's request carries a non-empty UUID trace_id."""
    cfg_servicer, cfg_addr, ab_addr = capturing_servers
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
        ok = await _wait_until(lambda: len(cfg_servicer.subscribe_reqs) >= 1)
        assert ok, "Subscribe never attached"
        req = cfg_servicer.subscribe_reqs[-1]
        assert req.trace_id, "Subscribe must populate trace_id"
        assert _looks_like_uuid_v4(req.trace_id), (
            f"expected canonical 36-char dashed UUID, got {req.trace_id!r}"
        )
    finally:
        await cli.aclose()


async def test_subscribe_and_pull_use_independent_trace_ids(capturing_servers):
    """Subscribe and the parallel startup PullAll get distinct trace_ids.

    They're independent RPCs initiated by separate code paths; no design
    requirement to share an id, and conflating them would make it
    impossible to grep "which PullAll" vs "which Subscribe" attached when.
    """
    cfg_servicer, cfg_addr, ab_addr = capturing_servers
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
        ok = await _wait_until(lambda: len(cfg_servicer.subscribe_reqs) >= 1)
        assert ok
        sub_id = cfg_servicer.subscribe_reqs[-1].trace_id
        pull_id = cfg_servicer.pull_reqs[0].trace_id
        assert sub_id and pull_id
        assert sub_id != pull_id
    finally:
        await cli.aclose()
