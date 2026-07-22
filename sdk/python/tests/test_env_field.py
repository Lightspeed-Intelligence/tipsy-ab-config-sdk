"""Config.env stamping onto every outgoing request (design D11 / ST6).

The SDK carries a single-value ``env`` environment identifier on every request
it emits so the server can filter experiment admission by environment. Contract
under test:

- ``Config.env`` defaults to ``""`` ("unspecified"). With the default, the
  outgoing proto's ``env`` field is the zero value, and — because both
  transports serialise via protojson (``MessageToJson`` on the HTTP path) which
  omits zero-value scalars — the HTTP request body never contains an ``"env"``
  key. This is the旧 SDK / backward-compat wire shape.
- A non-empty ``Config.env`` (e.g. ``"prod"``) is stamped verbatim onto all
  three request kinds the SDK actually emits: ``GetExperimentResult`` (public
  method + the ``get_config`` config_version fast path), ``PullAll`` (startup +
  periodic), and ``Subscribe``. It travels on both the gRPC and HTTP transports.

We lean on the existing capture seams:
- gRPC abtest: ``conftest.FakeAbtestServicer.last_req``.
- gRPC subscribe: ``conftest.FakeConfigServicer.subscribe_reqs``.
- gRPC PullAll: base ``FakeConfigServicer`` does not keep PullAll requests, so
  we subclass it locally (same approach as ``test_pull_subscribe_traceid.py``).
- HTTP: a local ``httpx.MockTransport`` recorder that keeps both the decoded
  proto AND the raw JSON body (so the "env omitted when empty" assertion can
  inspect the actual bytes on the wire).
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import socket
from typing import Dict, List, Optional

import grpc
import grpc.aio
import pytest

from google.protobuf import json_format

from tipsy_ab_config import Config, UserInfo, init
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2, abtest_pb2_grpc
from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2, config_pb2_grpc

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


def _free_port() -> int:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


async def _wait_until(predicate, timeout=2.0, step=0.05):
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


# ===========================================================================
# gRPC transport
# ===========================================================================


class CapturingConfigServicer(FakeConfigServicer):
    """Adds ``pull_reqs`` capture on top of the shared FakeConfigServicer."""

    def __init__(self) -> None:
        super().__init__()
        self.pull_reqs: List[config_pb2.PullAllRequest] = []

    async def PullAll(self, request, context):
        kept = config_pb2.PullAllRequest()
        kept.CopyFrom(request)
        self.pull_reqs.append(kept)
        return await super().PullAll(request, context)


@pytest.fixture
async def grpc_servers():
    """Start a capturing ConfigService + a vanilla AbtestService.

    Yields ``(cfg_servicer, ab_servicer, cfg_addr, ab_addr)`` so a test can
    drive all three request kinds and inspect ``cfg_servicer.pull_reqs`` /
    ``.subscribe_reqs`` / ``ab_servicer.last_req``.
    """
    cfg_servicer = CapturingConfigServicer()
    ab_servicer = FakeAbtestServicer()

    cfg_server = grpc.aio.server()
    config_pb2_grpc.add_ConfigServiceServicer_to_server(cfg_servicer, cfg_server)
    cfg_addr = f"127.0.0.1:{_free_port()}"
    cfg_server.add_insecure_port(cfg_addr)
    await cfg_server.start()

    ab_server = grpc.aio.server()
    abtest_pb2_grpc.add_AbtestServiceServicer_to_server(ab_servicer, ab_server)
    ab_addr = f"127.0.0.1:{_free_port()}"
    ab_server.add_insecure_port(ab_addr)
    await ab_server.start()

    try:
        yield cfg_servicer, ab_servicer, cfg_addr, ab_addr
    finally:
        with contextlib.suppress(Exception):
            await cfg_server.stop(grace=None)
        with contextlib.suppress(Exception):
            await ab_server.stop(grace=None)


async def test_grpc_default_env_is_empty_on_all_requests(grpc_servers):
    """No ``Config.env`` set ⇒ every request carries env=='' (zero value)."""
    cfg_servicer, ab_servicer, cfg_addr, ab_addr = grpc_servers
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
            # env intentionally left at its default "".
        )
    )
    try:
        await cli.get_experiment_result("ns1", user_info=UserInfo(uid="u1"))
        assert ab_servicer.last_req is not None
        assert ab_servicer.last_req.env == "", "default env must be empty on abtest"

        assert len(cfg_servicer.pull_reqs) >= 1, "startup PullAll must run"
        assert cfg_servicer.pull_reqs[0].env == "", "default env must be empty on PullAll"

        ok = await _wait_until(lambda: len(cfg_servicer.subscribe_reqs) >= 1)
        assert ok, "Subscribe never attached"
        assert cfg_servicer.subscribe_reqs[-1].env == "", (
            "default env must be empty on Subscribe"
        )
    finally:
        await cli.aclose()


async def test_grpc_env_stamped_on_all_requests(grpc_servers):
    """``Config.env='prod'`` ⇒ env=='prod' on abtest + PullAll + Subscribe."""
    cfg_servicer, ab_servicer, cfg_addr, ab_addr = grpc_servers
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
            env="prod",
        )
    )
    try:
        await cli.get_experiment_result("ns1", user_info=UserInfo(uid="u1"))
        assert ab_servicer.last_req is not None
        assert ab_servicer.last_req.env == "prod"

        assert len(cfg_servicer.pull_reqs) >= 1
        assert cfg_servicer.pull_reqs[0].env == "prod"

        ok = await _wait_until(lambda: len(cfg_servicer.subscribe_reqs) >= 1)
        assert ok, "Subscribe never attached"
        assert cfg_servicer.subscribe_reqs[-1].env == "prod"
    finally:
        await cli.aclose()


async def test_grpc_env_stamped_on_get_config_fast_path(grpc_servers):
    """The ``get_config`` config_version fast path also stamps env.

    ``get_config`` drives ``_fetch_config_version_flat_kv_for_ns`` — a distinct
    construction point from the public ``get_experiment_result`` — so it needs
    its own assertion.
    """
    cfg_servicer, ab_servicer, cfg_addr, ab_addr = grpc_servers
    # A key whose snapshot has dynamic resolution forces the abtest fast path
    # (the config_version flat_kv RPC that get_config awaits).
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})},
            has_dynamic_resolution={"k": True},
        )
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
            env="prod",
        )
    )
    try:
        abctx = cli.new_abtest_context("u1", {"country": "US"})
        await cli.get_config(abctx, "ns1", "k", "def")
        assert ab_servicer.last_req is not None
        assert ab_servicer.last_req.env == "prod", (
            "config_version fast path must stamp env too"
        )
    finally:
        await cli.aclose()


# ===========================================================================
# HTTP transport
# ===========================================================================
#
# The HTTP path serialises requests with ``json_format.MessageToJson`` (see
# ``_http_transport.py``), which OMITS zero-value scalars. So env=="" must be
# absent from the JSON body entirely, while a non-empty env must appear. We
# keep the raw body bytes (not just the decoded proto) so the "omitted when
# empty" assertion inspects exactly what went over the wire.

httpx = pytest.importorskip("httpx", reason="httpx required for HTTP transport tests")

CONFIG_PULL_PATH = "/api/v1/config/pull_all"
ABTEST_RESULT_PATH = "/api/v1/abtest/experiment_result"


class HttpEnvRecorder:
    """httpx.MockTransport handler that keeps decoded protos AND raw bodies."""

    def __init__(self) -> None:
        self.pull_responses: Dict[str, config_pb2.NamespaceSnapshot] = {}
        self.abtest_response: Optional[abtest_pb2.GetExperimentResultResponse] = None

        self.last_pull_req: Optional[config_pb2.PullAllRequest] = None
        self.last_abtest_req: Optional[abtest_pb2.GetExperimentResultRequest] = None
        self.last_pull_body: Optional[dict] = None
        self.last_abtest_body: Optional[dict] = None
        self.pull_calls: int = 0
        self.abtest_calls: int = 0

    def set_pull_snapshot(self, snap: config_pb2.NamespaceSnapshot) -> None:
        self.pull_responses[snap.namespace] = snap

    def set_abtest_response(
        self, resp: abtest_pb2.GetExperimentResultResponse
    ) -> None:
        self.abtest_response = resp

    def handler(self, request: "httpx.Request") -> "httpx.Response":
        path = request.url.path
        body = request.content.decode("utf-8") if request.content else ""
        parsed_json = json.loads(body) if body.strip() else {}
        if path == CONFIG_PULL_PATH:
            self.pull_calls += 1
            req = config_pb2.PullAllRequest()
            if body.strip():
                json_format.Parse(body, req, ignore_unknown_fields=True)
            self.last_pull_req = req
            self.last_pull_body = parsed_json
            out = config_pb2.PullAllResponse()
            for ns in req.namespaces:
                if ns in self.pull_responses:
                    out.snapshots.append(self.pull_responses[ns])
            return httpx.Response(
                200,
                content=json_format.MessageToJson(out).encode("utf-8"),
                headers={"content-type": "application/json"},
            )
        if path == ABTEST_RESULT_PATH:
            self.abtest_calls += 1
            req = abtest_pb2.GetExperimentResultRequest()
            if body.strip():
                json_format.Parse(body, req, ignore_unknown_fields=True)
            self.last_abtest_req = req
            self.last_abtest_body = parsed_json
            resp = self.abtest_response or abtest_pb2.GetExperimentResultResponse()
            return httpx.Response(
                200,
                content=json_format.MessageToJson(resp).encode("utf-8"),
                headers={"content-type": "application/json"},
            )
        return httpx.Response(404, json={"error": f"unexpected path {path}"})


def _http_client(recorder: HttpEnvRecorder) -> "httpx.AsyncClient":
    return httpx.AsyncClient(transport=httpx.MockTransport(recorder.handler))


async def test_http_default_env_omitted_from_bodies():
    """Default env=="" ⇒ ``"env"`` key absent from every HTTP request body."""
    recorder = HttpEnvRecorder()
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    recorder.set_abtest_response(make_exp_result({"k": 1}))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr="http://lb.internal:8080",
            abtest_service_addr="http://lb.internal:8080",
            transport="http",
            http_client=_http_client(recorder),
            token=issue_test_token(),
            pull_interval=10.0,
            pull_timeout=2.0,
            pull_retries=1,
            abtest_timeout=2.0,
            # env left at default "".
        )
    )
    try:
        await cli.get_experiment_result("ns1", user_info=UserInfo(uid="u1"))
        assert recorder.abtest_calls >= 1
        assert recorder.last_abtest_body is not None
        assert "env" not in recorder.last_abtest_body, (
            "protojson must omit zero-value env from the abtest body"
        )

        assert recorder.pull_calls >= 1
        assert recorder.last_pull_body is not None
        assert "env" not in recorder.last_pull_body, (
            "protojson must omit zero-value env from the PullAll body"
        )
    finally:
        await cli.aclose()


async def test_http_env_stamped_on_bodies():
    """env=='prod' ⇒ present on both the abtest and PullAll HTTP bodies."""
    recorder = HttpEnvRecorder()
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    recorder.set_abtest_response(make_exp_result({"k": 1}))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr="http://lb.internal:8080",
            abtest_service_addr="http://lb.internal:8080",
            transport="http",
            http_client=_http_client(recorder),
            token=issue_test_token(),
            pull_interval=10.0,
            pull_timeout=2.0,
            pull_retries=1,
            abtest_timeout=2.0,
            env="prod",
        )
    )
    try:
        await cli.get_experiment_result("ns1", user_info=UserInfo(uid="u1"))
        assert recorder.last_abtest_req is not None
        assert recorder.last_abtest_req.env == "prod"
        assert recorder.last_abtest_body is not None
        assert recorder.last_abtest_body.get("env") == "prod", (
            "non-empty env must appear on the HTTP abtest body"
        )

        assert recorder.pull_calls >= 1
        assert recorder.last_pull_req is not None
        assert recorder.last_pull_req.env == "prod"
        assert recorder.last_pull_body is not None
        assert recorder.last_pull_body.get("env") == "prod", (
            "non-empty env must appear on the HTTP PullAll body"
        )
    finally:
        await cli.aclose()
