"""secretKey auth mode tests (issue #16).

What is locked down, and why the assertions look the way they do:

* ``Config.secret_key`` alone (no ``token`` / ``token_provider``) initialises
  the client and every RPC carries ``Authorization: SecretKey <secret>`` —
  asserted with **exact equality on the value the server received**, on BOTH
  transports. Python has TWO independent header-construction cut points
  (the gRPC interceptors in ``client.py`` and the HTTP transport in
  ``_http_transport.py``, design r1-F1); asserting the wire value per path is
  what catches a change that fixes one cut point and misses the other.
* Credential priority is SecretKey > token_provider > token, resolved per
  request — again asserted per transport path.
* The scheme is the exact literal ``SecretKey`` (wire contract with the
  platform verifier, design r1-F3): the SDK always sends this spelling, the
  server matches it case-insensitively. Hence ``==``, never ``startswith``
  or a case-normalising comparison, in every header assertion here.
* Init with none of the three credentials raises, with the message shape
  shared across Go/Java/Python (adapted to each language's field naming).
* ``channel_factory`` exemption regression: a factory-built channel bypasses
  the SDK's auth instrumentation entirely, so Init requires no credential on
  that path and ``secret_key`` does NOT attach — pre-existing semantics that
  the Init-validation relaxation must not disturb.
* The secret value never appears in anything the SDK logs.
"""

from __future__ import annotations

import asyncio
import logging

import pytest

from tipsy_ab_config import Config, UserInfo, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_per_group_result,
    make_snapshot,
)

grpc = pytest.importorskip("grpc", reason="grpcio required for the gRPC-path tests")

# A value that cannot collide with anything else a log line would contain.
SECRET = "sk-test-secret-3f9a1c-DO-NOT-LOG"

# Init error message: same shape as Go ("SecretKey, Token or TokenProvider
# must be set") and Java, with Python's cfg-field naming.
_INIT_ERR = (
    "tipsy_ab_config: cfg.secret_key, cfg.token or cfg.token_provider must be set"
)


async def _wait_until(predicate, timeout=3.0, step=0.05):
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if predicate():
            return True
        await asyncio.sleep(step)
    return predicate()


def _grpc_config(cfg_addr: str, ab_addr: str, **overrides) -> Config:
    kwargs = dict(
        namespaces=["ns1"],
        config_service_addr=cfg_addr,
        abtest_service_addr=ab_addr,
        pull_interval=10.0,
        pull_retries=1,
    )
    kwargs.update(overrides)
    return Config(**kwargs)


# ===========================================================================
# 1. gRPC path: exact wire value per method kind (cut point #1, client.py).
# ===========================================================================


async def test_grpc_secret_key_only_init_and_exact_header(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """secret_key alone initialises; PullAll AND Subscribe both carry the
    exact literal ``SecretKey <secret>`` (never Bearer, never re-cased)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))
    ab_servicer.set_response("ns1", make_per_group_result({"k": 1}))

    cli = await init(_grpc_config(cfg_addr, ab_addr, secret_key=SECRET))
    try:
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        assert ok, "Subscribe never attached"

        # Unary cut (PullAll) — exact value, catches scheme misspelling,
        # a stray Bearer prefix, and double-prefixing alike.
        assert cfg_servicer.pull_metadata, "PullAll was never called"
        assert (
            cfg_servicer.pull_metadata[0].get("authorization")
            == "SecretKey " + SECRET
        )
        # Server-streaming cut (Subscribe) — same exact value.
        assert (
            cfg_servicer.subscribe_metadata[0].get("authorization")
            == "SecretKey " + SECRET
        )

        # The RPC surface works end-to-end with the secret as the only
        # credential (issue #16 acceptance: Init + PullAll + Subscribe +
        # GetExperimentResult).
        val = await cli.get_config(cli.new_abtest_context("u1"), "ns1", "k", "def")
        assert val == "v1"
        assert ab_servicer.calls >= 1, "GetExperimentResult was never called"
    finally:
        await cli.aclose()


async def test_grpc_secret_key_wins_over_token_and_provider(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """All three credentials configured -> the gRPC path sends the SecretKey
    scheme, not any Bearer form (priority SecretKey > token_provider > token)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))
    provided = issue_test_token(subject="should-lose-to-secret")

    async def provider() -> str:
        return provided

    cli = await init(
        _grpc_config(
            cfg_addr,
            ab_addr,
            secret_key=SECRET,
            token=issue_test_token(subject="static-should-lose"),
            token_provider=provider,
        )
    )
    try:
        ok = await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        assert ok, "Subscribe never attached"
        for md in cfg_servicer.pull_metadata + cfg_servicer.subscribe_metadata:
            assert md.get("authorization") == "SecretKey " + SECRET, (
                f"secret_key must win over token/token_provider; "
                f"got {md.get('authorization')!r}"
            )
    finally:
        await cli.aclose()


# ===========================================================================
# 2. HTTP path: exact wire value (cut point #2, _http_transport.py).
#    These assertions are the discriminator for a dual-cut-point miss: a
#    change that only touches the gRPC interceptors leaves this path sending
#    "Bearer <token>" (an empty-token "Bearer " in secret-only setups).
# ===========================================================================


async def test_http_secret_key_only_init_and_exact_header():
    """secret_key alone initialises in HTTP mode; pull_all AND
    experiment_result both carry the exact literal ``SecretKey <secret>``."""
    pytest.importorskip("httpx", reason="httpx required for the HTTP-path tests")
    from .test_http_transport import HttpRecorder, http_config, make_exp_result

    recorder = HttpRecorder()
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    recorder.set_abtest_response(make_exp_result({"k": 1}))

    cli = await init(
        http_config(recorder, token="", secret_key=SECRET, pull_interval=10.0)
    )
    try:
        # Exercise the second HTTP endpoint too so both transport classes'
        # requests are captured.
        await cli.get_experiment_result("ns1", UserInfo(experiment_hash_id="u1"))
        assert recorder.auth_headers, "no HTTP requests captured"
        for hdr in recorder.auth_headers:
            assert hdr == "SecretKey " + SECRET, f"bad auth header: {hdr!r}"
    finally:
        await cli.aclose()


async def test_http_secret_key_wins_over_token_and_provider():
    """All three credentials configured -> the HTTP path sends the SecretKey
    scheme (per-path priority assertion, dual-cut-point discriminator)."""
    pytest.importorskip("httpx", reason="httpx required for the HTTP-path tests")
    from .test_http_transport import HttpRecorder, http_config

    provided = issue_test_token(subject="should-lose-to-secret")

    async def provider() -> str:
        return provided

    recorder = HttpRecorder()
    recorder.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))

    cli = await init(
        http_config(
            recorder,
            token=issue_test_token(subject="static-should-lose"),
            token_provider=provider,
            secret_key=SECRET,
            pull_interval=10.0,
        )
    )
    try:
        assert recorder.auth_headers, "no HTTP requests captured"
        for hdr in recorder.auth_headers:
            assert hdr == "SecretKey " + SECRET, (
                f"secret_key must win over token/token_provider; got {hdr!r}"
            )
    finally:
        await cli.aclose()


# ===========================================================================
# 3. Init validation: relaxed to "any of the three", not further.
# ===========================================================================


async def test_grpc_init_rejects_when_all_three_credentials_missing():
    with pytest.raises(ValueError) as excinfo:
        await init(
            Config(namespaces=["ns1"], config_service_addr="dns:///example:18081")
        )
    assert str(excinfo.value) == _INIT_ERR


async def test_http_init_rejects_when_all_three_credentials_missing():
    """The HTTP-mode Init check is its own validation point (client.py
    ``_init_http``) and must reject with the same message."""
    with pytest.raises(ValueError) as excinfo:
        await init(
            Config(
                namespaces=["ns1"],
                config_service_addr="http://lb.internal:8080",
                transport="http",
            )
        )
    assert str(excinfo.value) == _INIT_ERR


# ===========================================================================
# 4. channel_factory exemption: pre-existing semantics, unchanged.
# ===========================================================================


def _routing_factory(cfg_addr: str, ab_addr: str):
    """Factory routing the two SDK channel builds to the two fake servers."""

    def factory(addr: str) -> "grpc.aio.Channel":
        # _init_grpc calls the factory once per service; the config address is
        # passed through verbatim, the abtest one may be empty.
        return grpc.aio.insecure_channel(cfg_addr if addr == "cfg" else ab_addr)

    return factory


async def test_channel_factory_init_needs_no_credentials(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """channel_factory set + no credential at all -> Init succeeds and the
    factory channel carries NO authorization metadata (current semantics: the
    factory bypasses the SDK's auth instrumentation; the caller wires auth).
    The Init relaxation must not have tightened this path."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr="cfg",
            abtest_service_addr="ab",
            channel_factory=_routing_factory(cfg_addr, ab_addr),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        assert cfg_servicer.pull_metadata, "PullAll was never called"
        assert "authorization" not in cfg_servicer.pull_metadata[0]
    finally:
        await cli.aclose()


async def test_channel_factory_secret_key_does_not_attach(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """secret_key + channel_factory -> the secret does NOT attach (the factory
    path carries no SDK auth at all — pre-existing semantics, not a defect)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))

    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr="cfg",
            abtest_service_addr="ab",
            channel_factory=_routing_factory(cfg_addr, ab_addr),
            secret_key=SECRET,
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        assert cfg_servicer.pull_metadata, "PullAll was never called"
        md = cfg_servicer.pull_metadata[0]
        assert "authorization" not in md, (
            f"factory channels must stay un-instrumented; got "
            f"authorization={md.get('authorization')!r}"
        )
    finally:
        await cli.aclose()


# ===========================================================================
# 5. The secret never reaches a log line.
# ===========================================================================


async def test_secret_key_never_logged(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
    caplog: pytest.LogCaptureFixture,
):
    """A full init + RPC cycle at DEBUG capture: the secret value must not
    appear in any emitted record (message or attached extras)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v1"})}))
    ab_servicer.set_response("ns1", make_per_group_result({"k": 1}))

    with caplog.at_level(logging.DEBUG):
        cli = await init(_grpc_config(cfg_addr, ab_addr, secret_key=SECRET))
        try:
            await cli.get_config(cli.new_abtest_context("u1"), "ns1", "k", "def")
            await _wait_until(lambda: cfg_servicer.subscribe_calls >= 1)
        finally:
            await cli.aclose()

    assert caplog.records, "expected the cycle to log something at DEBUG"
    for rec in caplog.records:
        rendered = rec.getMessage() + " " + repr(rec.__dict__)
        assert SECRET not in rendered, (
            f"secret leaked into log record from {rec.name}: {rendered[:200]}"
        )
