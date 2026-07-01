"""HTTP transport-mode tests for the Python SDK (SubTask ST3).

These tests exercise the optional ``transport="http"`` path added by the
gRPC/HTTP dual-transport work (design.md §Proposed Design 2/3/5/6,
§Testing Plan "Python SDK (ST3)").

Injection strategy
------------------
Rather than spin up a real HTTP server, we inject an ``httpx.AsyncClient``
backed by ``httpx.MockTransport`` through the new ``Config.http_client`` seam.
The MockTransport handler decodes the protojson request body with
``json_format.Parse`` and encodes the protojson response with
``MessageToJson`` configured to mirror the *server* output options:

    preserving_proto_field_name=True              (≡ UseProtoNames: snake_case)
    always_print_fields_with_no_presence=True     (≡ EmitUnpopulated: zero
                                                   values emitted; the protobuf
                                                   6.x kwarg — the old
                                                   ``including_default_value_fields``
                                                   was removed)

This guarantees the SDK's own ``json_format.Parse(..., ignore_unknown_fields=
True)`` decode is tested against the exact wire format the Go server emits
(snake_case field names, int64 as quoted strings — see the Go
``publicread_test.go`` TestPullAll_JSONOutputFormat assertions).

Assumptions about the in-progress implementation (codingAgent contract)
----------------------------------------------------------------------
* ``Config.transport: str = ""`` and ``Config.http_client: Optional[
  httpx.AsyncClient] = None`` fields exist.
* Module-level constant ``tipsy_ab_config.client.TRANSPORT_ENV_VAR ==
  "TIPSY_SDK_TRANSPORT"``.
* HTTP base URLs append fixed paths:
    config_service_addr + "/api/v1/config/pull_all"
    abtest_service_addr + "/api/v1/abtest/experiment_result"
* HTTP mode does NOT start a Subscribe task; only the pull loop.
* ``Authorization: Bearer <token>`` is taken from ``_TokenCache.current()``.

Where the contract is uncertain (e.g. exact transport-parse helper name) the
tests assert via observable ``init()`` behaviour instead of calling internal
functions directly, and a couple of helper-call tests are guarded with
``getattr`` + ``pytest.skip`` so they light up once the symbol lands without
hard-failing the suite before then.
"""

from __future__ import annotations

import asyncio
import sys
from typing import Callable, Dict, List, Optional

import pytest

from google.protobuf import json_format

from tipsy_ab_config import Config, StartupPullFailed, UserInfo, init
from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

from .conftest import (
    issue_test_token,
    make_exp_result,
    make_snapshot,
)

# httpx is an optional dependency (pyproject [http] extra + dev). All the
# end-to-end tests need it; skip the whole module cleanly if it is absent so a
# bare-grpc dev environment doesn't see spurious failures. The dedicated
# "missing httpx" test (test_http_mode_without_httpx_raises) does NOT depend on
# httpx being installed and lives outside this skip via its own import guard.
httpx = pytest.importorskip("httpx", reason="httpx required for HTTP transport tests")


CONFIG_PULL_PATH = "/api/v1/config/pull_all"
ABTEST_RESULT_PATH = "/api/v1/abtest/experiment_result"


# ---------------------------------------------------------------------------
# protojson encode/decode mirroring the server's publicread output options.
# ---------------------------------------------------------------------------


def _server_marshal(msg) -> str:
    """Encode ``msg`` the way the Go publicread endpoints do.

    UseProtoNames (snake_case) + EmitUnpopulated (zero values present). The
    protobuf python binding spells UseProtoNames ``preserving_proto_field_name``.
    EmitUnpopulated changed kwarg names across protobuf releases:

    * protobuf >= 5.x (incl. the 6.x pinned in .venv): the supported kwarg is
      ``always_print_fields_with_no_presence`` — ``including_default_value_fields``
      was REMOVED and now raises ``TypeError``.
    * protobuf < 5.x: only ``including_default_value_fields`` exists.

    We therefore try the NEW kwarg FIRST (it is what the installed protobuf
    6.33 actually honours), fall back to the legacy kwarg for old protobuf, and
    only as a last resort drop emit-defaults entirely. Crucially we catch ONLY
    ``TypeError`` on the probes — an unsupported kwarg raises ``TypeError``,
    whereas a genuine encoding fault raises ``ValueError`` and must surface
    rather than silently degrade the handler's fidelity to the Go server.
    """
    try:
        # protobuf >= 5.x (matches the pinned 6.33 in sdk/python/.venv).
        return json_format.MessageToJson(
            msg,
            preserving_proto_field_name=True,
            always_print_fields_with_no_presence=True,
        )
    except TypeError:
        pass
    try:
        # Legacy protobuf < 5.x.
        return json_format.MessageToJson(
            msg,
            preserving_proto_field_name=True,
            including_default_value_fields=True,
        )
    except TypeError:
        # Neither emit-defaults kwarg is accepted: encode without it. This is a
        # last-resort path that should not be reached on any supported
        # protobuf; tests asserting zero-value fields would catch a regression
        # here.
        return json_format.MessageToJson(msg, preserving_proto_field_name=True)


# ---------------------------------------------------------------------------
# _server_marshal fidelity to the Go publicread output (F2).
#
# The mock handler is only a faithful stand-in for the real Go server if it
# reproduces the server's protojson options: UseProtoNames (snake_case) +
# EmitUnpopulated (zero values present) + int64 string encoding. This locks
# that in directly, so a regression to the no-emit fallback (e.g. a future
# protobuf renaming the kwarg again) fails loudly here rather than silently
# weakening the end-to-end decode tests. Mirrors the Go
# publicread_test.py::TestPullAll_JSONOutputFormat assertions.
# ---------------------------------------------------------------------------


def test_server_marshal_matches_go_publicread_output() -> None:
    import json as _json

    # business_snapshot_seq > 2^53 exercises int64 string encoding;
    # experiment_snapshot_seq = 0 exercises EmitUnpopulated.
    snap = make_snapshot("shop", biz=1234567890123456789, exp=0)
    out = config_pb2.PullAllResponse(snapshots=[snap])

    decoded = _json.loads(_server_marshal(out))
    snaps = decoded["snapshots"]
    assert len(snaps) == 1
    s = snaps[0]

    # UseProtoNames: snake_case present, camelCase absent.
    assert "business_snapshot_seq" in s, s.keys()
    assert "businessSnapshotSeq" not in s, s.keys()

    # int64 encoded as a quoted string (precision correctness).
    assert s["business_snapshot_seq"] == "1234567890123456789"
    assert isinstance(s["business_snapshot_seq"], str)

    # EmitUnpopulated: the zero-value int64 field is still emitted (as "0").
    assert "experiment_snapshot_seq" in s, (
        "EmitUnpopulated fidelity lost: zero-value field dropped — "
        "_server_marshal fell back to the no-emit path"
    )
    assert s["experiment_snapshot_seq"] == "0"


# ---------------------------------------------------------------------------
# Recording mock transport handler.
# ---------------------------------------------------------------------------


class HttpRecorder:
    """Programmable httpx.MockTransport handler + per-path call recorder.

    Mirrors conftest's Fake*Servicer ergonomics but over HTTP:

    * ``set_pull_snapshot`` / ``set_pull_error`` configure the pull_all path.
    * ``set_abtest_response`` / ``set_abtest_error`` configure the
      experiment_result path.
    * ``pull_calls`` / ``abtest_calls`` count requests; ``last_pull_req`` /
      ``last_abtest_req`` capture the decoded proto; ``auth_headers`` records
      every inbound Authorization header value (in order) so token assertions
      can inspect exactly what the SDK sent.
    """

    def __init__(self) -> None:
        self.pull_responses: Dict[str, config_pb2.NamespaceSnapshot] = {}
        self.pull_status: int = 200
        self.pull_calls: int = 0
        self.last_pull_req: Optional[config_pb2.PullAllRequest] = None

        self.abtest_response: Optional[abtest_pb2.GetExperimentResultResponse] = None
        self.abtest_status: int = 200
        self.abtest_calls: int = 0
        self.last_abtest_req: Optional[abtest_pb2.GetExperimentResultRequest] = None

        self.auth_headers: List[Optional[str]] = []
        self.paths_seen: List[str] = []

    # ---- programming ----

    def set_pull_snapshot(self, snap: config_pb2.NamespaceSnapshot) -> None:
        self.pull_responses[snap.namespace] = snap

    def set_pull_status(self, status: int) -> None:
        self.pull_status = status

    def set_abtest_response(
        self, resp: abtest_pb2.GetExperimentResultResponse
    ) -> None:
        self.abtest_response = resp

    def set_abtest_status(self, status: int) -> None:
        self.abtest_status = status

    # ---- the httpx.MockTransport handler ----

    def handler(self, request: "httpx.Request") -> "httpx.Response":
        path = request.url.path
        self.paths_seen.append(path)
        self.auth_headers.append(request.headers.get("authorization"))

        if path == CONFIG_PULL_PATH:
            return self._handle_pull(request)
        if path == ABTEST_RESULT_PATH:
            return self._handle_abtest(request)
        # Any other path is a routing bug in the SDK — surface it loudly.
        return httpx.Response(404, json={"error": f"unexpected path {path}"})

    def _handle_pull(self, request: "httpx.Request") -> "httpx.Response":
        self.pull_calls += 1
        req = config_pb2.PullAllRequest()
        body = request.content.decode("utf-8") if request.content else ""
        if body.strip():
            json_format.Parse(body, req, ignore_unknown_fields=True)
        self.last_pull_req = req
        if self.pull_status != 200:
            return httpx.Response(
                self.pull_status, json={"error": "injected pull error"}
            )
        out = config_pb2.PullAllResponse()
        for ns in req.namespaces:
            if ns in self.pull_responses:
                out.snapshots.append(self.pull_responses[ns])
        return httpx.Response(
            200,
            content=_server_marshal(out),
            headers={"content-type": "application/json"},
        )

    def _handle_abtest(self, request: "httpx.Request") -> "httpx.Response":
        self.abtest_calls += 1
        req = abtest_pb2.GetExperimentResultRequest()
        body = request.content.decode("utf-8") if request.content else ""
        if body.strip():
            json_format.Parse(body, req, ignore_unknown_fields=True)
        self.last_abtest_req = req
        if self.abtest_status != 200:
            return httpx.Response(
                self.abtest_status, json={"error": "injected abtest error"}
            )
        resp = self.abtest_response or abtest_pb2.GetExperimentResultResponse()
        return httpx.Response(
            200,
            content=_server_marshal(resp),
            headers={"content-type": "application/json"},
        )


def make_http_client(recorder: HttpRecorder) -> "httpx.AsyncClient":
    """An AsyncClient routed entirely through the recorder (no real network)."""
    return httpx.AsyncClient(transport=httpx.MockTransport(recorder.handler))


@pytest.fixture
def recorder() -> HttpRecorder:
    return HttpRecorder()


def http_config(
    recorder: HttpRecorder,
    *,
    namespaces: Optional[List[str]] = None,
    base_url: str = "http://lb.internal:8080",
    abtest_base_url: Optional[str] = "http://lb.internal:8080",
    token: Optional[str] = None,
    token_provider=None,
    **overrides,
) -> Config:
    """Build a Config wired for HTTP mode with the recorder's mock client.

    Defaults compress ``pull_interval``/timeouts so periodic-loop assertions
    are fast; callers override per-test as needed.
    """
    kwargs = dict(
        namespaces=namespaces or ["ns1"],
        config_service_addr=base_url,
        abtest_service_addr=abtest_base_url if abtest_base_url is not None else "",
        transport="http",
        http_client=make_http_client(recorder),
        token=token if token is not None else issue_test_token(),
        token_provider=token_provider,
        pull_interval=0.05,
        pull_timeout=2.0,
        pull_retries=1,
        abtest_timeout=2.0,
    )
    kwargs.update(overrides)
    return Config(**kwargs)


async def _wait_until(predicate: Callable[[], bool], timeout: float = 3.0,
                      step: float = 0.02) -> bool:
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


# ===========================================================================
# 1. Transport-mode resolution (mirrors the Go matrix; design §2)
# ===========================================================================
#
# The parse logic ("cfg.transport > TIPSY_SDK_TRANSPORT > grpc", strip+lower,
# illegal -> ValueError) may be inlined in init() or extracted into a helper.
# We test it three ways:
#   (a) directly against a helper if codingAgent exported one (skip if absent);
#   (b) via init() behaviour: a Config with an HTTP-only setup (mock client +
#       http base URL) only *works* if HTTP mode was selected; if grpc were
#       selected init would try to dial "http://..." as a gRPC target;
#   (c) illegal value -> ValueError straight out of init().

TRANSPORT_ENV_VAR = "TIPSY_SDK_TRANSPORT"


def _resolve_transport_helper():
    """Return codingAgent's transport-resolution helper if it exists.

    Tries a few likely names/locations so the direct-call matrix test lights
    up regardless of the exact symbol chosen, without hard-failing if the
    logic stayed inlined in init().
    """
    from tipsy_ab_config import client as client_mod

    for name in ("_resolve_transport", "_parse_transport", "resolve_transport"):
        fn = getattr(client_mod, name, None)
        if callable(fn):
            return fn
    return None


def test_transport_env_var_constant():
    """The module exposes TRANSPORT_ENV_VAR == 'TIPSY_SDK_TRANSPORT'."""
    from tipsy_ab_config import client as client_mod

    const = getattr(client_mod, "TRANSPORT_ENV_VAR", None)
    if const is None:
        pytest.skip("TRANSPORT_ENV_VAR not yet exported by client module")
    assert const == TRANSPORT_ENV_VAR


@pytest.mark.parametrize(
    "cfg_value, env_value, expected",
    [
        # (cfg.transport, env, normalized result)
        ("", None, "grpc"),            # default: nothing set
        ("http", None, "http"),
        ("grpc", None, "grpc"),
        ("HTTP", None, "http"),        # case-insensitive
        (" Grpc ", None, "grpc"),      # strip + lower
        ("", "http", "http"),          # env drives when cfg empty
        ("", "HTTP", "http"),          # env normalized too
        ("", " grpc ", "grpc"),
        ("http", "grpc", "http"),      # cfg wins over env
        ("grpc", "http", "grpc"),      # cfg wins over env
    ],
)
def test_transport_resolution_matrix(monkeypatch, cfg_value, env_value, expected):
    """cfg.transport > TIPSY_SDK_TRANSPORT > 'grpc', strip + lower."""
    fn = _resolve_transport_helper()
    if fn is None:
        pytest.skip(
            "no exported transport-resolution helper; covered indirectly by "
            "init() behaviour tests"
        )
    if env_value is None:
        monkeypatch.delenv(TRANSPORT_ENV_VAR, raising=False)
    else:
        monkeypatch.setenv(TRANSPORT_ENV_VAR, env_value)
    # Helper signature is unknown; try (cfg) then (cfg_value).
    try:
        result = fn(Config(transport=cfg_value, namespaces=["ns1"]))
    except TypeError:
        result = fn(cfg_value)
    assert result == expected


@pytest.mark.parametrize("bad", ["rest", "GRPCS", "http2", "tcp", "htttp"])
async def test_illegal_transport_value_raises(recorder, monkeypatch, bad):
    """An unrecognized transport value -> ValueError (not StartupFailOpen)."""
    monkeypatch.delenv(TRANSPORT_ENV_VAR, raising=False)
    with pytest.raises(ValueError):
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr="http://lb.internal:8080",
                transport=bad,
                http_client=make_http_client(recorder),
                token=issue_test_token(),
            )
        )


@pytest.mark.parametrize("bad", ["websocket", "udp"])
async def test_illegal_transport_from_env_raises(recorder, monkeypatch, bad):
    """Illegal value coming from the env var also -> ValueError."""
    monkeypatch.setenv(TRANSPORT_ENV_VAR, bad)
    with pytest.raises(ValueError):
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr="http://lb.internal:8080",
                http_client=make_http_client(recorder),
                token=issue_test_token(),
            )
        )


async def test_transport_env_var_selects_http(recorder, monkeypatch):
    """TIPSY_SDK_TRANSPORT=http (no cfg.transport) selects HTTP end-to-end."""
    monkeypatch.setenv(TRANSPORT_ENV_VAR, "http")
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (10, {10: "v10"})}))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr="http://lb.internal:8080",
            abtest_service_addr="http://lb.internal:8080",
            # transport left empty on purpose: env var must drive selection.
            http_client=make_http_client(recorder),
            token=issue_test_token(),
            pull_interval=10.0,
            pull_timeout=2.0,
            pull_retries=1,
        )
    )
    try:
        assert cli.get_config_static("ns1", "k", "def") == "v10"
        # HTTP path was exercised (not gRPC): the recorder saw a pull request.
        assert recorder.pull_calls >= 1
    finally:
        await cli.aclose()


async def test_cfg_transport_overrides_env(recorder, monkeypatch):
    """cfg.transport='http' wins even when env says grpc."""
    monkeypatch.setenv(TRANSPORT_ENV_VAR, "grpc")
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (10, {10: "v10"})}))
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        assert cli.get_config_static("ns1", "k", "def") == "v10"
        assert recorder.pull_calls >= 1
    finally:
        await cli.aclose()


# ===========================================================================
# 2. Missing httpx handling (design §5, Acceptance Criteria 6)
# ===========================================================================


async def test_http_mode_without_httpx_raises(monkeypatch):
    """Choosing http mode when httpx can't be imported -> clear error.

    We force ``import httpx`` to fail by making the module unimportable, even
    in environments where httpx IS installed. The SDK should surface a clear
    ImportError/RuntimeError whose message points at the extra install
    (``tipsy-ab-config[http]`` and/or ``httpx``).

    Note: we do NOT pass ``http_client`` here — the SDK must reach the
    ``import httpx`` line during init() to construct its own client. (If the
    implementation imports httpx eagerly at module top-level rather than lazily
    inside the http branch, this test will need the import-machinery patch
    applied before the SDK module is first imported; see the reload guard.)
    """
    import builtins

    real_import = builtins.__import__

    def fake_import(name, *args, **kwargs):
        if name == "httpx" or name.startswith("httpx."):
            raise ImportError("No module named 'httpx'")
        return real_import(name, *args, **kwargs)

    # Drop any cached httpx so the patched __import__ is actually consulted.
    for mod in list(sys.modules):
        if mod == "httpx" or mod.startswith("httpx."):
            monkeypatch.delitem(sys.modules, mod, raising=False)
    monkeypatch.setattr(builtins, "__import__", fake_import)

    with pytest.raises((ImportError, RuntimeError)) as excinfo:
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr="http://lb.internal:8080",
                transport="http",
                token=issue_test_token(),
                pull_retries=1,
                pull_timeout=1.0,
            )
        )
    msg = str(excinfo.value).lower()
    assert "httpx" in msg or "tipsy-ab-config[http]" in msg, (
        f"error message should mention the missing dep / extra: {excinfo.value!r}"
    )


# ===========================================================================
# 3. HTTP end-to-end (the core; design §5/§6, Acceptance Criteria 3)
# ===========================================================================


async def test_http_startup_pull_populates_cache(recorder):
    """init() in http mode runs the startup PullAll; get_config_static hits."""
    recorder.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (10, {10: "v10"}), "empty": (11, {11: ""})})
    )
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        assert cli.get_config_static("ns1", "k", "def") == "v10"
        # Empty-string value is a valid hit, not a miss.
        assert cli.get_config_static("ns1", "empty", "def") == ""
        # Miss -> default.
        assert cli.get_config_static("ns1", "missing", "def") == "def"
        # Startup pull issued exactly one request per ns (here: one ns).
        assert recorder.pull_calls >= 1
        assert recorder.last_pull_req is not None
        assert list(recorder.last_pull_req.namespaces) == ["ns1"]
    finally:
        await cli.aclose()


async def test_http_multi_namespace_startup_pull(recorder):
    """Startup pull sweeps each subscribed ns (serial per-ns requests)."""
    for ns in ("nsA", "nsB", "nsC"):
        recorder.set_pull_snapshot(make_snapshot(ns, 1, 1, {"k": (1, {1: "v"})}))
    cli = await init(
        http_config(recorder, namespaces=["nsB", "nsA", "nsC"], pull_interval=10.0)
    )
    try:
        for ns in ("nsA", "nsB", "nsC"):
            assert cli.cache.full_release_version(ns, "k") == 1
        # One pull request per ns at startup (design §6:逐 ns 串行).
        assert recorder.pull_calls >= 3
    finally:
        await cli.aclose()


async def test_http_periodic_pull_refreshes_cache(recorder):
    """The periodic pull loop (PullInterval) refreshes the cache over HTTP."""
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))
    cli = await init(http_config(recorder, pull_interval=0.05))
    try:
        assert cli.cache.full_release_version("ns1", "k") == 1
        # Advance the handler's canned data; the loop should pick it up.
        recorder.set_pull_snapshot(make_snapshot("ns1", 2, 2, {"k": (2, {2: "v2"})}))
        ok = await _wait_until(
            lambda: cli.cache.full_release_version("ns1", "k") == 2
        )
        assert ok, "periodic HTTP pull did not refresh the cache"
    finally:
        await cli.aclose()


async def test_http_get_experiment_result(recorder):
    """get_experiment_result over HTTP returns the decoded proto response.

    The deprecated `exposures` field is retained on the wire (D1) but is
    never populated by the server again (D3); see
    test_http_get_experiment_result_exposures_round_trip below for the
    backward-compat read coverage.
    """
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})}))
    resp = make_exp_result({"k": 7})
    recorder.set_abtest_response(resp)
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        out = await cli.get_experiment_result("ns1", UserInfo(uid="u1"))
        assert out.config_flat_kv.get("k") == 7
        assert recorder.abtest_calls >= 1
        # The decoded request carried the namespace + user id over the wire.
        assert recorder.last_abtest_req is not None
        assert recorder.last_abtest_req.namespace == "ns1"
        assert recorder.last_abtest_req.user_id == "u1"
    finally:
        await cli.aclose()


async def test_http_get_experiment_result_exposures_round_trip(recorder):
    """Backward-compat: SDK still decodes a legacy server's `exposures`.

    Mirrors the Go-side
    transport_http_test.go::TestHTTP_GetExperimentResult_ExposuresRoundTrip:
    mock server塞 the deprecated `Exposures` repeated field to simulate an
    OLD server; verify the SDK still deserialises it. Future protojson
    regressions on the optional+int64 path will be caught here.
    """
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})}))
    resp = make_exp_result({"k": 7})
    ex = resp.exposures.add()
    ex.key = "k"
    ex.version = 7
    ex.source = "experiment_group"
    ex.experiment_id = "100"
    ex.group_id = "200"
    recorder.set_abtest_response(resp)
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        out = await cli.get_experiment_result("ns1", UserInfo(uid="u1"))
        # Backward-compat read: even though SDK no longer EMITS exposures,
        # the proto field bytes a legacy server might send must still decode.
        assert len(out.exposures) == 1
        assert out.exposures[0].version == 7
        assert out.exposures[0].source == "experiment_group"
    finally:
        await cli.aclose()


async def test_http_get_experiment_result_gray_hits_round_trip(recorder):
    """Grouped `gray_hits` field (D1/D2) round-trips through the HTTP transport.

    Mirrors the Go-side
    transport_http_test.go::TestHTTP_GetExperimentResult_GrayHitsRoundTrip:
    mock server塞一条 grouped GrayReleaseHit (release_id=7, key_versions={"k": 99});
    verify the SDK observes its key_versions map via response.gray_hits after
    the publicread protojson decode.
    """
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})}))
    resp = make_exp_result({"k": 99})
    resp.gray_hits.add(release_id=7, key_versions={"k": 99})
    recorder.set_abtest_response(resp)
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        out = await cli.get_experiment_result("ns1", UserInfo(uid="u-gray"))
        assert len(out.gray_hits) == 1, (
            f"expected 1 gray_hit; got {list(out.gray_hits)!r}"
        )
        assert out.gray_hits[0].release_id == 7
        assert dict(out.gray_hits[0].key_versions) == {"k": 99}
        assert out.gray_hits[0].key_versions["k"] == 99
    finally:
        await cli.aclose()


async def test_http_get_config_full_link(recorder):
    """get_config over HTTP: experiment compute + value lookup.

    D3 removed the SDK-side exposure emit pipeline; this test now only
    verifies the abtest-resolved version reaches the value lookup.
    """
    recorder.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab-v2"})})
    )
    resp = make_exp_result({"k": 2})
    recorder.set_abtest_response(resp)
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        abctx = cli.new_abtest_context("u1", {"country": "US"})
        val = await cli.get_config(abctx, "ns1", "k", "def")
        # abtest hit -> version 2 value resolved from the locally-pulled cache.
        assert val == "ab-v2"
        assert recorder.abtest_calls >= 1
    finally:
        await cli.aclose()


async def test_http_get_config_ab_fallback_to_full(recorder):
    """abtest version absent from cache -> silent full-release fallback + metric."""
    recorder.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full-only"})})
    )
    # Experiment says version 99 but the cache only has version 1.
    recorder.set_abtest_response(make_exp_result({"k": 99}))
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full-only"
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
    finally:
        await cli.aclose()


# ---- Authorization header ----


async def test_http_authorization_header_static_token(recorder):
    """Every HTTP request carries 'Authorization: Bearer <static token>'."""
    token = issue_test_token(subject="svc-static")
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    recorder.set_abtest_response(make_exp_result({"k": 1}))
    cli = await init(http_config(recorder, token=token, pull_interval=10.0))
    try:
        await cli.get_experiment_result("ns1", UserInfo(uid="u1"))
        # Pull (startup) + abtest both carried the bearer.
        assert recorder.auth_headers, "no requests captured"
        for hdr in recorder.auth_headers:
            assert hdr == f"Bearer {token}", f"bad auth header: {hdr!r}"
    finally:
        await cli.aclose()


async def test_http_authorization_header_token_provider(recorder):
    """token_provider primes _TokenCache at init; HTTP requests use that token.

    Python semantics (design §Important Details F3): the provider is awaited
    ONCE at init to prime the cache; the HTTP transport reads
    ``_TokenCache.current()`` per request (it does NOT await the provider on
    every request).
    """
    provided = issue_test_token(subject="svc-from-provider")

    calls = {"n": 0}

    async def provider() -> str:
        calls["n"] += 1
        return provided

    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    cli = await init(
        http_config(
            recorder,
            token="",            # no static token; provider supplies it
            token_provider=provider,
            pull_interval=10.0,
        )
    )
    try:
        assert recorder.auth_headers, "no requests captured"
        for hdr in recorder.auth_headers:
            assert hdr == f"Bearer {provided}", f"bad auth header: {hdr!r}"
        # Provider primed once at init, not per-request.
        assert calls["n"] == 1, (
            f"token_provider awaited {calls['n']}x; expected exactly 1 (init prime)"
        )
    finally:
        await cli.aclose()


# ---- non-2xx degradation ----


async def test_http_pull_500_increments_pull_failure(recorder):
    """A 500 from pull_all during the periodic loop -> inc_pull_failure.

    Startup uses fail-open so init succeeds with an empty cache; then the
    periodic loop keeps failing and bumps the per-ns failure counter.
    """
    recorder.set_pull_status(500)
    cli = await init(
        http_config(recorder, pull_interval=0.05, startup_fail_open=True)
    )
    try:
        ok = await _wait_until(
            lambda: cli.metrics.pull_failure_total("ns1") >= 1
        )
        assert ok, "pull failure counter never incremented on HTTP 500"
        # Empty cache: nothing was applied.
        assert cli.get_config_static("ns1", "k", "def") == "def"
    finally:
        await cli.aclose()


async def test_http_abtest_403_degrades_to_full(recorder):
    """A 403 from experiment_result -> get_config degrades to full + metric."""
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})}))
    recorder.set_abtest_status(403)
    cli = await init(http_config(recorder, pull_interval=10.0))
    try:
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full"
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
    finally:
        await cli.aclose()


# ---- StartupFailOpen ----


async def test_http_startup_fail_close_raises(recorder):
    """startup pull 500 + startup_fail_open=False -> init raises StartupPullFailed."""
    recorder.set_pull_status(500)
    with pytest.raises(StartupPullFailed):
        await init(
            http_config(
                recorder,
                pull_interval=10.0,
                pull_retries=1,
                pull_timeout=1.0,
                startup_fail_open=False,
            )
        )


async def test_http_startup_fail_open_metric(recorder):
    """startup pull 500 + startup_fail_open=True -> init OK, empty cache, metric."""
    recorder.set_pull_status(500)
    cli = await init(
        http_config(
            recorder,
            pull_interval=10.0,
            pull_retries=1,
            pull_timeout=1.0,
            startup_fail_open=True,
        )
    )
    try:
        assert cli.metrics.cache_empty_total() == 1
        assert cli.metrics.pull_failure_total("ns1") >= 1
        assert cli.get_config_static("ns1", "k", "def") == "def"
    finally:
        await cli.aclose()


# ---- no Subscribe in HTTP mode + clean shutdown ----


async def test_http_mode_does_not_subscribe(recorder):
    """HTTP mode starts NO Subscribe task; only the pull loop.

    There is no Subscribe HTTP endpoint, so the recorder must NEVER see a
    subscribe-shaped path. We also assert the SDK didn't spin up a subscribe
    coroutine by inspecting the task list (best-effort: the names of the
    running coroutines should not include the subscribe loop).
    """
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    cli = await init(http_config(recorder, pull_interval=0.05))
    try:
        # Give the loops a moment to spin.
        await asyncio.sleep(0.2)
        # Only the two known HTTP paths should ever be hit.
        assert set(recorder.paths_seen) <= {CONFIG_PULL_PATH, ABTEST_RESULT_PATH}, (
            f"unexpected paths hit in http mode: {recorder.paths_seen}"
        )
        # No subscribe coroutine running.
        task_reprs = " ".join(repr(t) for t in getattr(cli, "_tasks", []))
        assert "_run_subscribe" not in task_reprs, (
            f"subscribe task started in http mode: {task_reprs}"
        )
    finally:
        # aclose must not hang (no subscribe stream to drain).
        await asyncio.wait_for(cli.aclose(), timeout=5.0)


async def test_http_aclose_is_clean(recorder):
    """aclose() exits cleanly + promptly in HTTP mode (no deadlock)."""
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    cli = await init(http_config(recorder, pull_interval=0.05))
    await asyncio.sleep(0.1)
    await asyncio.wait_for(cli.aclose(), timeout=5.0)
    # Idempotent: a second close is a no-op.
    await cli.aclose()
    # Reads after close return the default (closed-client contract).
    assert cli.get_config_static("ns1", "k", "def") == "def"


# ===========================================================================
# 4. HTTP address validation (design §3, Acceptance Criteria 5)
# ===========================================================================


@pytest.mark.parametrize(
    "bad_addr",
    [
        "lb.internal:8080",          # no scheme
        "grpc://lb.internal:8080",   # wrong scheme
        "ftp://lb.internal:8080",
        "//lb.internal:8080",
        "lb.internal",
    ],
)
async def test_http_config_addr_must_be_http_url(recorder, bad_addr):
    """In http mode a non-http(s):// config_service_addr -> ValueError."""
    with pytest.raises(ValueError):
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr=bad_addr,
                transport="http",
                http_client=make_http_client(recorder),
                token=issue_test_token(),
                pull_retries=1,
                pull_timeout=1.0,
            )
        )


async def test_http_empty_config_addr_raises(recorder):
    """Empty config_service_addr in http mode -> ValueError (addr needed for routing)."""
    with pytest.raises(ValueError):
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr="",
                transport="http",
                http_client=make_http_client(recorder),
                token=issue_test_token(),
                pull_retries=1,
                pull_timeout=1.0,
            )
        )


async def test_http_https_scheme_accepted(recorder):
    """An https:// base URL is accepted (TLS terminated upstream)."""
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    cli = await init(
        http_config(
            recorder,
            base_url="https://lb.internal:8443",
            abtest_base_url="https://lb.internal:8443",
            pull_interval=10.0,
        )
    )
    try:
        assert cli.get_config_static("ns1", "k", "def") == "v"
    finally:
        await cli.aclose()


async def test_http_empty_abtest_addr_degrades(recorder):
    """Empty abtest_service_addr in http mode -> init OK, abtest永久不可用降级.

    GetConfigStatic stays usable; get_config sees abtest as permanently
    unavailable and falls back to full release (design §3 / F2).
    """
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "full"})}))
    cli = await init(
        http_config(recorder, abtest_base_url=None, pull_interval=10.0)
    )
    try:
        # Static read works.
        assert cli.get_config_static("ns1", "k", "def") == "full"
        # Dynamic read degrades to full (no abtest transport).
        abctx = cli.new_abtest_context("u1")
        val = await cli.get_config(abctx, "ns1", "k", "def")
        assert val == "full"
        # No abtest endpoint should ever have been hit.
        assert recorder.abtest_calls == 0
        assert cli.metrics.abtest_fallback_total("ns1") >= 1
    finally:
        await cli.aclose()


async def test_http_trailing_slash_stripped(recorder):
    """A trailing slash on the base URL is stripped; path is not doubled."""
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    cli = await init(
        http_config(
            recorder,
            base_url="http://lb.internal:8080/",
            abtest_base_url="http://lb.internal:8080/",
            pull_interval=10.0,
        )
    )
    try:
        assert cli.get_config_static("ns1", "k", "def") == "v"
        # Exact path, no doubling from a stray trailing slash on the base URL.
        assert CONFIG_PULL_PATH in recorder.paths_seen, (
            f"expected exact path {CONFIG_PULL_PATH}; saw {recorder.paths_seen}"
        )
        # Guard against '.../pull_all' becoming '...//api/...' or trailing '/'.
        assert "//" not in "".join(recorder.paths_seen), (
            f"doubled slash in request path: {recorder.paths_seen}"
        )
    finally:
        await cli.aclose()


# ===========================================================================
# 5. Injected http_client lifecycle (design §5: SDK must not close it)
# ===========================================================================


async def test_injected_http_client_not_closed_by_sdk(recorder):
    """aclose() must NOT close a caller-injected httpx client.

    The injected client is the caller's responsibility (httpx convention,
    design §5). After SDK aclose() the injected client must still be open and
    usable.
    """
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    client = make_http_client(recorder)
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr="http://lb.internal:8080",
            abtest_service_addr="http://lb.internal:8080",
            transport="http",
            http_client=client,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_timeout=2.0,
            pull_retries=1,
        )
    )
    await cli.aclose()
    # The injected client must remain open (not closed by the SDK).
    assert client.is_closed is False, "SDK closed the injected httpx client"
    # And it must still be usable for a direct request.
    resp = await client.post(
        "http://lb.internal:8080" + CONFIG_PULL_PATH,
        content=json_format.MessageToJson(
            config_pb2.PullAllRequest(namespaces=["ns1"])
        ),
    )
    assert resp.status_code == 200
    await client.aclose()
