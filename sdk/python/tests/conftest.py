"""Shared test fixtures + helpers for the Python SDK tests.

We use the ``Config.channel_factory`` seam wherever possible: it lets us
inject in-memory channels without spinning up TCP. For Subscribe-stream
tests that exercise reconnect logic we use a real ``grpc.aio.server``
bound to ``127.0.0.1:0`` because ``grpc.aio`` has no in-memory pipe.

All proto generated bindings live under
``tipsy_ab_config._proto.tipsy.*``.
"""

from __future__ import annotations

import asyncio
import contextlib
import socket
from typing import Any, AsyncIterator, Dict, List, Optional, Tuple

import grpc
import grpc.aio
import pytest

from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2
from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2_grpc
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2_grpc


def _free_port() -> int:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class FakeConfigServicer(config_pb2_grpc.ConfigServiceServicer):
    """Programmable in-process ConfigService."""

    def __init__(self) -> None:
        self.pull_responses: Dict[str, config_pb2.NamespaceSnapshot] = {}
        self.pull_calls: int = 0
        self.pull_error: Optional[grpc.StatusCode] = None

        self.subscribe_calls: int = 0
        self.subscribe_reqs: List[config_pb2.SubscribeRequest] = []
        # Lazily-built; created inside the first running event loop so it
        # binds to the correct loop on every Python version.
        self._push_q: Optional[
            "asyncio.Queue[Optional[config_pb2.NamespaceSnapshot]]"
        ] = None
        # When True, Subscribe raises after the first push and resets.
        self.fail_after_first_push: bool = False
        self._fail_counter: int = 0

    def _ensure_queue(self) -> "asyncio.Queue[Optional[config_pb2.NamespaceSnapshot]]":
        if self._push_q is None:
            self._push_q = asyncio.Queue()
        return self._push_q

    def set_pull_snapshot(self, snap: config_pb2.NamespaceSnapshot) -> None:
        self.pull_responses[snap.namespace] = snap

    def push_snapshot(self, snap: config_pb2.NamespaceSnapshot) -> None:
        self._ensure_queue().put_nowait(snap)

    async def PullAll(
        self,
        request: config_pb2.PullAllRequest,
        context: grpc.aio.ServicerContext,
    ) -> config_pb2.PullAllResponse:
        self.pull_calls += 1
        if self.pull_error is not None:
            await context.abort(self.pull_error, "fake pull error")
        out = config_pb2.PullAllResponse()
        for ns in request.namespaces:
            if ns in self.pull_responses:
                out.snapshots.append(self.pull_responses[ns])
        return out

    async def Subscribe(
        self,
        request: config_pb2.SubscribeRequest,
        context: grpc.aio.ServicerContext,
    ) -> AsyncIterator[config_pb2.ConfigUpdateEvent]:
        self.subscribe_calls += 1
        self.subscribe_reqs.append(request)
        q = self._ensure_queue()
        while True:
            try:
                snap = await q.get()
            except asyncio.CancelledError:
                return
            if snap is None:
                return
            ev = config_pb2.ConfigUpdateEvent()
            ev.snapshot.CopyFrom(snap)
            yield ev
            if self.fail_after_first_push:
                self._fail_counter += 1
                if self._fail_counter == 1:
                    await context.abort(
                        grpc.StatusCode.UNAVAILABLE, "fail after first push"
                    )


class FakeAbtestServicer(abtest_pb2_grpc.AbtestServiceServicer):
    """Programmable in-process AbtestService.

    Mirrors the Go test harness's abServer: per-ns canned
    ``GetExperimentResultResponse``, per-ns error injection, call counters
    (total + per-ns), an artificial delay (to make concurrent first-accessors
    genuinely race in-flight), and the last received request.
    """

    def __init__(self) -> None:
        self.responses: Dict[str, abtest_pb2.GetExperimentResultResponse] = {}
        self.errors: Dict[str, grpc.StatusCode] = {}
        self.calls: int = 0
        self.calls_by_ns: Dict[str, int] = {}
        self.delay: float = 0.0
        self.last_req: Optional[abtest_pb2.GetExperimentResultRequest] = None

    def set_response(
        self,
        ns: str,
        resp: abtest_pb2.GetExperimentResultResponse,
    ) -> None:
        self.responses[ns] = resp

    def set_error(self, ns: str, code: grpc.StatusCode) -> None:
        self.errors[ns] = code

    async def GetExperimentResult(
        self,
        request: abtest_pb2.GetExperimentResultRequest,
        context: grpc.aio.ServicerContext,
    ) -> abtest_pb2.GetExperimentResultResponse:
        self.calls += 1
        self.calls_by_ns[request.namespace] = (
            self.calls_by_ns.get(request.namespace, 0) + 1
        )
        self.last_req = request
        if self.delay:
            await asyncio.sleep(self.delay)
        err = self.errors.get(request.namespace)
        if err is not None:
            await context.abort(err, "fake abtest error")
        return self.responses.get(
            request.namespace, abtest_pb2.GetExperimentResultResponse()
        )

    async def GetPossibleVersionsAndExperimentSnapshotSeq(
        self, request, context
    ):  # not exercised by SDK tests
        return abtest_pb2.GetPossibleVersionsAndExperimentSnapshotSeqResponse()


async def start_server(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: Optional[FakeAbtestServicer] = None,
) -> Tuple[grpc.aio.Server, str]:
    server = grpc.aio.server()
    config_pb2_grpc.add_ConfigServiceServicer_to_server(cfg_servicer, server)
    if ab_servicer is not None:
        abtest_pb2_grpc.add_AbtestServiceServicer_to_server(ab_servicer, server)
    port = _free_port()
    addr = f"127.0.0.1:{port}"
    server.add_insecure_port(addr)
    await server.start()
    return server, addr


def make_snapshot(
    ns: str,
    biz: int,
    exp: int,
    keys: Optional[Dict[str, Tuple[Optional[int], Dict[int, str]]]] = None,
    has_dynamic_resolution: Optional[Dict[str, Optional[bool]]] = None,
) -> config_pb2.NamespaceSnapshot:
    """Build a NamespaceSnapshot proto.

    ``keys`` maps configKey → ``(full_release_version_or_None, {version_id:value})``.

    ``has_dynamic_resolution`` optionally controls the per-key
    ``optional bool has_dynamic_resolution`` proto field in three states:

    - ``True``  → ``ks.has_dynamic_resolution = True`` (key carries gray/exp).
    - ``False`` → ``ks.has_dynamic_resolution = False`` (explicitly pure-full).
    - ``None``  → field left UNSET on the wire, exercising the ``HasField``
      False branch (an old server that predates the field). This is also the
      default for any key not listed in the dict, so existing callers that pass
      no ``has_dynamic_resolution`` keep building snapshots whose keys leave the
      field absent — i.e. identical to before this field existed.
    """
    hdr = has_dynamic_resolution or {}
    snap = config_pb2.NamespaceSnapshot(
        namespace=ns,
        business_snapshot_seq=biz,
        experiment_snapshot_seq=exp,
    )
    for k, (full, versions) in (keys or {}).items():
        ks = config_pb2.KeyState(key=k)
        if full is not None:
            ks.full_release_version = full
        for vid, val in versions.items():
            ks.versions[vid] = val
        # Only set the optional field when the caller asked for an explicit
        # True/False. A missing entry (or an explicit None) leaves the proto
        # field UNSET so ``KeyState.HasField("has_dynamic_resolution")`` is
        # False on the wire — the old-server / absent state.
        flag = hdr.get(k)
        if flag is not None:
            ks.has_dynamic_resolution = flag
        snap.keys.append(ks)
    return snap


def make_exp_result(
    config_flat_kv: Optional[Dict[str, int]] = None,
) -> abtest_pb2.GetExperimentResultResponse:
    """Build a GetExperimentResultResponse with a config_flat_kv map.

    Mirrors the Go harness's ``&abtestv1.GetExperimentResultResponse{
    ConfigFlatKv: ...}`` literal used across v2_namespace_test.go.
    """
    resp = abtest_pb2.GetExperimentResultResponse()
    for k, v in (config_flat_kv or {}).items():
        resp.config_flat_kv[k] = v
    return resp


@pytest.fixture
def cfg_servicer() -> FakeConfigServicer:
    return FakeConfigServicer()


@pytest.fixture
def ab_servicer() -> FakeAbtestServicer:
    return FakeAbtestServicer()


@pytest.fixture
async def running_servers(
    cfg_servicer: FakeConfigServicer, ab_servicer: FakeAbtestServicer
):
    """Start fake config + abtest gRPC servers, yield (cfg_addr, ab_addr)."""
    # We start two separate servers to mirror Go SDK's two channels — that
    # lets the test sever them independently when it needs to.
    cfg_server, cfg_addr = await start_server(cfg_servicer)
    ab_only = grpc.aio.server()
    abtest_pb2_grpc.add_AbtestServiceServicer_to_server(ab_servicer, ab_only)
    ab_port = _free_port()
    ab_addr = f"127.0.0.1:{ab_port}"
    ab_only.add_insecure_port(ab_addr)
    await ab_only.start()

    try:
        yield cfg_addr, ab_addr
    finally:
        with contextlib.suppress(Exception):
            await cfg_server.stop(grace=None)
        with contextlib.suppress(Exception):
            await ab_only.stop(grace=None)


# --------- token construction ----------

import time
import hmac
import base64
import json
import hashlib

SECRET = "tipsyabconfig-sdk-test-secret"


def _b64url(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode("ascii")


def issue_test_token(
    *,
    subject: str = "tipsy-internal-service",
    role: str = "internal_service",
    namespaces: Optional[List[str]] = None,
    ttl_seconds: int = 600,
    secret: str = SECRET,
) -> str:
    """Issue an HS256 JWT matching ``internal/auth/jwt.go`` expectations.

    The Python SDK tests don't talk to the real server-side auth interceptor
    because the in-process gRPC fakes don't install one. The token is still
    useful for ``token_provider`` rotation tests.
    """
    header = {"alg": "HS256", "typ": "JWT"}
    now = int(time.time())
    payload = {
        "sub": subject,
        "role": role,
        "namespaces": namespaces or ["*"],
        "exp": now + ttl_seconds,
        "iat": now,
    }
    header_b64 = _b64url(json.dumps(header, separators=(",", ":")).encode())
    payload_b64 = _b64url(json.dumps(payload, separators=(",", ":")).encode())
    signing_input = f"{header_b64}.{payload_b64}".encode("ascii")
    sig = hmac.new(secret.encode("utf-8"), signing_input, hashlib.sha256).digest()
    return f"{header_b64}.{payload_b64}.{_b64url(sig)}"
