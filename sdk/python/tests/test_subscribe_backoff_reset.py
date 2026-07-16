"""Wiring test for the Subscribe backoff-reset (ST4 / S4).

The pure-function coverage lives in ``test_backoff_reset.py``. This file
proves the reset is actually *wired into* ``_run_subscribe`` — i.e. a
long-lived-then-dropped connection genuinely resets the reconnect backoff to
~1s instead of letting it escalate. It guards against a silent regression
where ``_reset_backoff_if_stable`` exists but is never called (or is called
with the wrong uptime / threshold).

Mechanism
---------
We inject a tiny stability threshold (``_stable_reset_threshold_s = 0.05``)
and use a fake ConfigService whose ``Subscribe`` keeps each stream alive
~0.06s (just over the threshold) and then aborts. Every drop therefore
counts as "stable", so the backoff resets to 1.0s before every sleep and the
inter-reconnect gap stays flat at ~1s.

    with reset (correct):  gaps ≈ 1.06, 1.06, 1.06, ...
    without reset (bug):   gaps ≈ 1.06, 2.06, 4.06, ...  (exponential)

The discriminating signal is the SECOND observed gap (attach#2 → attach#3):
~1s if the reset is wired, ~2s if it is not.

Flake tradeoff (S4)
-------------------
This is an asyncio-timing test, so it is inherently softer than the pure
unit test. We keep it robust by:
  - leaning on the hard 1.0s ``asyncio.sleep`` floor (deterministic), and
  - asserting the gap against a wide 0.5..1.5s window that cleanly separates
    the reset case (~1.06s) from the escalate case (~2.06s) with ~0.5s of
    slack on each side.
If it ever proves flaky on CI it may be marked ``skip`` with this rationale;
the pure-function test remains the authoritative reset coverage. As written
it is intended to RUN.
"""

from __future__ import annotations

import asyncio
import contextlib
import socket

import grpc
import grpc.aio
import pytest

from tipsy_ab_config import Config, init
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2_grpc
from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2_grpc

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_snapshot,
)


def _free_port() -> int:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class ShortLivedThenErrorConfigServicer(FakeConfigServicer):
    """Subscribe stays alive ``live_seconds`` then aborts (a real error).

    Records the loop-clock time of every (re)attach so the test can measure
    the gap between successive reconnects. PullAll is inherited so startup
    seeding works.
    """

    def __init__(self, live_seconds: float = 0.06) -> None:
        super().__init__()
        self.live_seconds = live_seconds
        self.attach_times: list[float] = []

    async def Subscribe(self, request, context):
        self.subscribe_calls += 1
        self.subscribe_reqs.append(request)
        self.attach_times.append(asyncio.get_event_loop().time())
        # Keep the stream open just past the injected stability threshold so
        # the client measures uptime >= threshold, then drop it as a real error.
        await asyncio.sleep(self.live_seconds)
        await context.abort(
            grpc.StatusCode.UNAVAILABLE, "forced drop after live window"
        )
        yield  # unreachable; makes this a server-streaming (generator) handler


@pytest.fixture
async def short_lived_servers():
    """Start the short-lived ConfigService + a vanilla AbtestService.

    Yields ``(cfg_servicer, cfg_addr, ab_addr)``.
    """
    cfg_servicer = ShortLivedThenErrorConfigServicer(live_seconds=0.06)
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


async def _wait_until(predicate, timeout=8.0, step=0.05):
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


async def test_stable_connection_resets_backoff_between_reconnects(
    short_lived_servers,
):
    cfg_servicer, cfg_addr, ab_addr = short_lived_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=100.0,  # keep the pull loop out of the way
            pull_retries=1,
        )
    )
    # Inject the tiny stability threshold. There is no ``await`` between
    # init() returning and this line, so the subscribe task cannot process an
    # error before the attribute is set for every gap we assert on.
    cli._stable_reset_threshold_s = 0.05
    try:
        # Need three attaches to observe two reconnect gaps; the second gap is
        # the discriminator (reset ~1s vs escalate ~2s).
        ok = await _wait_until(
            lambda: len(cfg_servicer.attach_times) >= 3, timeout=8.0
        )
        assert ok, (
            f"expected >=3 Subscribe attaches; "
            f"got {len(cfg_servicer.attach_times)}"
        )

        gap_2 = cfg_servicer.attach_times[2] - cfg_servicer.attach_times[1]
        # Reset wired  -> backoff resets to 1.0 every drop -> gap ~1.06s.
        # Reset absent -> backoff escalates 1->2 -> this gap ~2.06s.
        assert 0.5 < gap_2 < 1.5, (
            "second reconnect gap should reflect a backoff reset to ~1s, not "
            f"an escalated ~2s wait; measured {gap_2:.3f}s "
            f"(attach_times={cfg_servicer.attach_times})"
        )
        # Sanity: we really did go through the real-error reconnect path.
        assert cli.metrics.subscribe_disconnect_total("ns1") >= 2
    finally:
        await cli.aclose()
