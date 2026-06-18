"""Unit tests for the 方案 Y gRPC address parser + channel builder.

Mirrors the Go ``grpc_target_test.go`` / ``grpc_dial_test.go``. Two concerns:

1. ``_parse_grpc_target`` — pure scheme-whitelist classification (design
   Proposed Design rules 1-5). This is the contract the whole feature rests on;
   the most load-bearing rows are the F1/F9 regression backstops (native grpc
   targets + bare host:port must NOT be misread as a scheme).
2. ``_build_channel`` — the channel_factory short-circuit (Review F3: factory
   wins BEFORE any parse) and the insecure-vs-secure channel selection
   (insecure_channel for plaintext, secure_channel for grpcs://, with the
   authority/SNI options threaded).

grpcio note: ``tipsy_ab_config.client`` imports ``grpc`` at module top level, so
these tests need grpcio installed. The project's ``sdk/python/.venv`` has it; a
bare environment that lacks grpcio is skipped cleanly here (importorskip) rather
than failing collection — codingAgent runs the suite inside the .venv where
grpcio is present.
"""

from __future__ import annotations

import pytest

# Skip the whole module cleanly when grpcio is absent (the parser logic itself
# is pure-stdlib, but client.py imports grpc at module top level so we cannot
# import the symbols without it). In the .venv used by CI/codingAgent grpcio is
# installed and every test below runs.
grpc = pytest.importorskip("grpc", reason="grpcio required to import tipsy_ab_config.client")

from tipsy_ab_config.client import (  # noqa: E402  (after importorskip)
    Config,
    _AuthInterceptor,
    _build_channel,
    _GrpcTarget,
    _parse_grpc_target,
    _TokenCache,
    init,
)

from .conftest import issue_test_token  # noqa: E402  (after importorskip)


# ===========================================================================
# 1. _parse_grpc_target — valid forms (table-driven, mirrors Go success table)
# ===========================================================================


@pytest.mark.parametrize(
    "addr, dial_target, use_tls, authority, insecure",
    [
        # ---- bare host:port → pass-through + plaintext (backward compatible) ----
        # F9 core: urlparse("ab-config-grpc:50051") would misread the hostname as
        # a scheme; the literal-prefix gate must keep this plaintext pass-through.
        ("ab-config-grpc:50051", "ab-config-grpc:50051", False, "", False),
        # F9 core: bare loopback ip:port (the form conftest's running_servers
        # uses) must NOT be classified as TLS.
        ("127.0.0.1:50051", "127.0.0.1:50051", False, "", False),
        ("127.0.0.1:443", "127.0.0.1:443", False, "", False),
        # ---- native grpc resolver targets → pass-through + plaintext (F1) ----
        # Contains "://" yet must pass through plaintext.
        ("passthrough:///bufnet-config", "passthrough:///bufnet-config", False, "", False),
        ("dns:///ab-config-grpc.svc:50051", "dns:///ab-config-grpc.svc:50051", False, "", False),
        ("unix:/var/run/abconfig.sock", "unix:/var/run/abconfig.sock", False, "", False),
        ("xds:///ab-config-grpc", "xds:///ab-config-grpc", False, "", False),
        # ---- grpc:// explicit plaintext ----
        ("grpc://ab-config-grpc:50051", "ab-config-grpc:50051", False, "", False),
        # ---- grpcs:// TLS ----
        (
            "grpcs://prod-ab-config-grpc.infra.example.com:443",
            "prod-ab-config-grpc.infra.example.com:443",
            True,
            "",
            False,
        ),
        # The exact Dev接入串 from the design.
        (
            "grpcs://47.253.175.59:443?authority=dev-ab-config-grpc.infra.fantacy.live&insecure=true",
            "47.253.175.59:443",
            True,
            "dev-ab-config-grpc.infra.fantacy.live",
            True,
        ),
        (
            "grpcs://10.0.0.5:443?authority=ab-config-grpc.internal",
            "10.0.0.5:443",
            True,
            "ab-config-grpc.internal",
            False,
        ),
        ("grpcs://host.example.com:443?insecure=false", "host.example.com:443", True, "", False),
        ("grpcs://host.example.com:443?insecure=1", "host.example.com:443", True, "", True),
        ("grpcs://host.example.com:443?insecure=0", "host.example.com:443", True, "", False),
        # ---- IPv6 ----
        ("grpcs://[::1]:443", "[::1]:443", True, "", False),
        (
            "grpcs://[2001:db8::1]:443?authority=ab-config-grpc.internal",
            "[2001:db8::1]:443",
            True,
            "ab-config-grpc.internal",
            False,
        ),
    ],
)
def test_parse_grpc_target_success(addr, dial_target, use_tls, authority, insecure):
    got = _parse_grpc_target(addr)
    assert isinstance(got, _GrpcTarget)
    assert got.dial_target == dial_target, f"dial_target for {addr!r}"
    assert got.use_tls is use_tls, f"use_tls for {addr!r}"
    assert got.authority == authority, f"authority for {addr!r}"
    assert got.insecure_skip_verify is insecure, f"insecure_skip_verify for {addr!r}"


# ===========================================================================
# 2. _parse_grpc_target — error forms (pytest.raises(ValueError, match=...))
# ===========================================================================


@pytest.mark.parametrize(
    "addr, match",
    [
        # Q1: query on a plaintext grpc:// target is meaningless → error.
        ("grpc://ab-config-grpc:50051?authority=x", "query parameters are only valid"),
        ("grpc://ab-config-grpc:50051?insecure=true", "query parameters are only valid"),
        # Q2: grpcs:// without an explicit port (no implicit :443).
        ("grpcs://ab-config-grpc.internal", "explicit port"),
        ("grpcs://[::1]", "explicit port"),
        # S2: a present-but-non-numeric / out-of-range port is "invalid port",
        # distinct from the missing-port "explicit port" error. Python surfaces
        # it at parse time too: urlparse's `.port` raises ValueError, wrapped as
        # "tipsy_ab_config: invalid port in grpcs:// target ...". Symmetric with
        # the Go "invalid port" parameter error.
        ("grpcs://host:abc", "invalid port"),
        ("grpcs://[::1]:abc", "invalid port"),
        ("grpcs://host:99999", "invalid port"),
        # Unknown / illegal query.
        ("grpcs://host.example.com:443?foo=bar", "unknown query parameter"),
        ("grpcs://host.example.com:443?insecure=maybe", "invalid insecure value"),
        # design rule 5: http(s):// in gRPC mode is a parameter error.
        ("http://lb.internal:8080", "HTTP base URL"),
        ("https://lb.internal:8443", "HTTP base URL"),
    ],
)
def test_parse_grpc_target_errors(addr, match):
    with pytest.raises(ValueError, match=match) as excinfo:
        _parse_grpc_target(addr)
    # All parse errors are tipsy_ab_config: parameter errors (design F6).
    assert str(excinfo.value).startswith("tipsy_ab_config:"), excinfo.value


# ===========================================================================
# 3. F9 / F1 standalone backstops (named so a breakage points at the contract)
# ===========================================================================


def test_bare_hostport_not_misparsed_as_scheme():
    """F9 core: bare host:port must NOT trip urlparse's scheme detection.

    urlparse('ab-config-grpc:50051') yields scheme='ab-config-grpc'; the parser
    must instead gate on the literal grpc://grpcs:// prefix and treat this as
    plaintext pass-through.
    """
    for addr in ("ab-config-grpc:50051", "127.0.0.1:50051", "127.0.0.1:443"):
        got = _parse_grpc_target(addr)
        assert got.use_tls is False, f"{addr!r} misclassified as TLS"
        assert got.dial_target == addr, f"{addr!r} not passed through verbatim"
        assert got.authority == ""
        assert got.insecure_skip_verify is False


def test_passthrough_is_plaintext():
    """F1 core: a native grpc target (contains '://') passes through plaintext."""
    for addr in ("passthrough:///bufnet-config", "passthrough:///bufnet-abtest"):
        got = _parse_grpc_target(addr)
        assert got.dial_target == addr
        assert got.use_tls is False
        assert got.authority == ""
        assert got.insecure_skip_verify is False


# ===========================================================================
# 4. _build_channel — channel_factory short-circuit (Review F3)
# ===========================================================================


def test_build_channel_factory_short_circuits_before_parse():
    """A channel_factory is consulted BEFORE any address parsing (Review F3).

    We pass an address that WOULD fail _parse_grpc_target (a bare http:// URL in
    gRPC mode) together with a factory. The factory must be called with the raw
    addr and its return value used as-is — no parse error must escape.
    """
    sentinel = object()
    seen = {}

    def factory(addr):
        seen["addr"] = addr
        return sentinel

    cfg = Config(
        namespaces=["ns1"],
        # This addr is intentionally one that _parse_grpc_target rejects; if the
        # factory short-circuit regressed to parse-first, this test would raise.
        config_service_addr="http://would-fail-to-parse:8080",
        channel_factory=factory,
    )

    ch = _build_channel(cfg, cfg.config_service_addr, auth_plugin=None)
    assert ch is sentinel, "channel_factory return value must be used as-is"
    assert seen["addr"] == "http://would-fail-to-parse:8080", (
        "factory must receive the raw addr, unparsed"
    )


# ===========================================================================
# 5. _build_channel — plaintext vs TLS channel selection
# ===========================================================================


def _patch_channel_builders(monkeypatch):
    """Replace grpc.aio.insecure_channel / secure_channel + ssl creds with spies.

    Returns a dict recording which builder ran and the args it received, so the
    test can assert selection (insecure vs secure) and the threaded options
    WITHOUT opening a real channel (no event loop / network needed).
    """
    calls = {"insecure": None, "secure": None, "ssl_creds": 0}

    def fake_insecure_channel(target, options=None, interceptors=None):
        calls["insecure"] = {
            "target": target,
            "options": list(options or []),
            "interceptors": list(interceptors or []),
        }
        return object()

    def fake_secure_channel(target, credentials, options=None, interceptors=None):
        calls["secure"] = {
            "target": target,
            "credentials": credentials,
            "options": list(options or []),
            "interceptors": list(interceptors or []),
        }
        return object()

    def fake_ssl_channel_credentials(root_certificates=None):
        calls["ssl_creds"] += 1
        return ("ssl-creds", root_certificates)

    monkeypatch.setattr(grpc.aio, "insecure_channel", fake_insecure_channel)
    monkeypatch.setattr(grpc.aio, "secure_channel", fake_secure_channel)
    monkeypatch.setattr(grpc, "ssl_channel_credentials", fake_ssl_channel_credentials)
    return calls


def test_build_channel_plaintext_uses_insecure_channel(monkeypatch):
    """A bare/native plaintext target builds an insecure_channel, not secure."""
    calls = _patch_channel_builders(monkeypatch)
    cfg = Config(namespaces=["ns1"], config_service_addr="ab-config-grpc:50051")

    _build_channel(cfg, "ab-config-grpc:50051", auth_plugin=None)

    assert calls["insecure"] is not None, "plaintext target must use insecure_channel"
    assert calls["secure"] is None, "plaintext target must NOT use secure_channel"
    assert calls["insecure"]["target"] == "ab-config-grpc:50051"
    assert calls["ssl_creds"] == 0


def test_build_channel_grpc_scheme_plaintext_uses_insecure_channel(monkeypatch):
    """grpc://host:port also routes to insecure_channel with the stripped target."""
    calls = _patch_channel_builders(monkeypatch)
    cfg = Config(namespaces=["ns1"], config_service_addr="grpc://ab-config-grpc:50051")

    _build_channel(cfg, "grpc://ab-config-grpc:50051", auth_plugin=None)

    assert calls["insecure"] is not None
    assert calls["secure"] is None
    # scheme is stripped before dialing.
    assert calls["insecure"]["target"] == "ab-config-grpc:50051"


def test_build_channel_grpcs_uses_secure_channel_with_authority_options(monkeypatch):
    """grpcs://...?authority builds a secure_channel; authority threads to the
    SNI/cert-name override + :authority options; target is the bare host:port."""
    calls = _patch_channel_builders(monkeypatch)
    addr = "grpcs://47.253.175.59:443?authority=dev-ab-config-grpc.infra.fantacy.live&insecure=true"
    cfg = Config(namespaces=["ns1"], config_service_addr=addr)

    _build_channel(cfg, addr, auth_plugin=None)

    assert calls["secure"] is not None, "grpcs:// must use secure_channel"
    assert calls["insecure"] is None, "grpcs:// must NOT use insecure_channel"
    assert calls["ssl_creds"] == 1, "secure_channel needs ssl_channel_credentials"
    # The dial target is the bare host:port (scheme/query stripped).
    assert calls["secure"]["target"] == "47.253.175.59:443"
    # authority is threaded into BOTH the SNI/cert-name override and :authority.
    opts = dict(calls["secure"]["options"])
    assert opts.get("grpc.ssl_target_name_override") == "dev-ab-config-grpc.infra.fantacy.live"
    assert opts.get("grpc.default_authority") == "dev-ab-config-grpc.infra.fantacy.live"


def test_build_channel_grpcs_no_authority_omits_override_options(monkeypatch):
    """grpcs:// without an authority query builds a secure_channel but does NOT
    set the SNI/authority override options (no override requested)."""
    calls = _patch_channel_builders(monkeypatch)
    addr = "grpcs://prod-ab-config-grpc.infra.example.com:443"
    cfg = Config(namespaces=["ns1"], config_service_addr=addr)

    _build_channel(cfg, addr, auth_plugin=None)

    assert calls["secure"] is not None
    assert calls["insecure"] is None
    opts = dict(calls["secure"]["options"])
    assert "grpc.ssl_target_name_override" not in opts
    assert "grpc.default_authority" not in opts


def test_build_channel_grpcs_default_verify_uses_system_roots(monkeypatch):
    """grpcs:// WITHOUT insecure=true is the default-verify path (design F9):
    the secure channel is built with system roots (root_certificates=None), so a
    server cert that does not chain to a system-trusted CA fails verification at
    connect time (the production "misconfig fails fast" semantic). The actual
    handshake failure is a manual/out-of-CI network test; here we lock in the
    code path: secure_channel + ssl_channel_credentials(root_certificates=None).
    """
    calls = _patch_channel_builders(monkeypatch)
    addr = "grpcs://prod-ab-config-grpc.infra.example.com:443"
    cfg = Config(namespaces=["ns1"], config_service_addr=addr)

    _build_channel(cfg, addr, auth_plugin=None)

    assert calls["secure"] is not None
    assert calls["ssl_creds"] == 1
    # fake_ssl_channel_credentials echoes back (tag, root_certificates).
    tag, root_certificates = calls["secure"]["credentials"]
    assert tag == "ssl-creds"
    assert root_certificates is None, (
        "default grpcs:// must use system roots (no injected CA) → standard "
        "verification; host-supplied roots go through Config.tls_root_certificates"
    )


# ===========================================================================
# 6. _build_channel — tls_root_certificates injection (Dev Origin-Cert path)
#
# Background: injecting a custom TLS root via channel_factory bypasses the SDK
# entirely and therefore drops the bearer _AuthInterceptor (token lost on the
# wire). Config.tls_root_certificates fixes that: the SDK still builds its OWN
# secure_channel with the injected root_certificates AND still attaches
# _AuthInterceptor, so the token is auto-on-the-wire. These tests lock in that
# contract (esp. the "token not lost" assertion — the whole point of the field).
# ===========================================================================


# A throwaway PEM-ish byte blob; the parser/builder never inspects its contents
# (grpc.ssl_channel_credentials is patched), so any non-None bytes object proves
# the injected value is threaded straight through.
_FAKE_ROOT_PEM = b"-----BEGIN CERTIFICATE-----\nMIIB-fake-origin-ca\n-----END CERTIFICATE-----\n"


def _auth_interceptor_present(interceptors) -> bool:
    """True if the SDK's bearer _AuthInterceptor is in the interceptor chain.

    This is the "token is auto-attached / not lost" assertion: _AuthInterceptor
    is what stamps ``authorization: Bearer <token>`` on every outgoing RPC.
    """
    return any(isinstance(i, _AuthInterceptor) for i in interceptors)


def test_build_channel_grpcs_injected_root_threads_through_and_keeps_auth(monkeypatch):
    """grpcs:// + tls_root_certificates: the injected PEM bytes reach
    ssl_channel_credentials as root_certificates, AND the bearer _AuthInterceptor
    is still attached to secure_channel (token NOT lost — the core advantage of
    this field over channel_factory)."""
    calls = _patch_channel_builders(monkeypatch)
    addr = "grpcs://47.253.175.59:443?authority=dev-ab-config-grpc.infra.fantacy.live"
    cfg = Config(
        namespaces=["ns1"],
        config_service_addr=addr,
        tls_root_certificates=_FAKE_ROOT_PEM,
    )
    # A real token cache so _AuthInterceptor is constructed and attached.
    auth = _TokenCache("dev-bearer-token", None)

    _build_channel(cfg, addr, auth_plugin=auth)

    assert calls["secure"] is not None, "grpcs:// must use secure_channel"
    assert calls["insecure"] is None
    assert calls["ssl_creds"] == 1
    # The injected root bytes are threaded verbatim into ssl_channel_credentials.
    tag, root_certificates = calls["secure"]["credentials"]
    assert tag == "ssl-creds"
    assert root_certificates == _FAKE_ROOT_PEM, (
        "Config.tls_root_certificates must be passed as root_certificates"
    )
    # Token-not-lost: _AuthInterceptor is on the channel (unlike channel_factory).
    assert _auth_interceptor_present(calls["secure"]["interceptors"]), (
        "tls_root_certificates path must keep _AuthInterceptor so the bearer "
        "token is auto-attached on every RPC"
    )


def test_build_channel_grpcs_insecure_with_injected_root_no_warning(monkeypatch, caplog):
    """grpcs://...?insecure=true + tls_root_certificates: NO warning is logged
    (verification proceeds against the injected anchor), and the injected root is
    used. insecure=true is moot once a real trust anchor is supplied."""
    import logging

    calls = _patch_channel_builders(monkeypatch)
    addr = (
        "grpcs://47.253.175.59:443"
        "?authority=dev-ab-config-grpc.infra.fantacy.live&insecure=true"
    )
    cfg = Config(
        namespaces=["ns1"],
        config_service_addr=addr,
        tls_root_certificates=_FAKE_ROOT_PEM,
    )
    auth = _TokenCache("dev-bearer-token", None)

    with caplog.at_level(logging.WARNING, logger="tipsy_ab_config"):
        _build_channel(cfg, addr, auth_plugin=auth)

    # No skip-verify warning when a trust anchor is injected.
    warnings = [r.getMessage() for r in caplog.records if r.levelno >= logging.WARNING]
    assert not any("insecure=true" in m for m in warnings), (
        f"unexpected insecure-skip-verify warning with injected root: {warnings}"
    )
    # Injected root used; secure channel built with auth still attached.
    assert calls["secure"] is not None
    _, root_certificates = calls["secure"]["credentials"]
    assert root_certificates == _FAKE_ROOT_PEM
    assert _auth_interceptor_present(calls["secure"]["interceptors"])


def test_build_channel_grpcs_insecure_without_root_warns_and_uses_system_roots(
    monkeypatch, caplog
):
    """grpcs://...?insecure=true + NO tls_root_certificates: a WARN fires (root
    is None → grpcio can't truly skip-verify; guides the user to
    tls_root_certificates), and the channel falls back to system roots
    (root_certificates=None)."""
    import logging

    calls = _patch_channel_builders(monkeypatch)
    addr = (
        "grpcs://47.253.175.59:443"
        "?authority=dev-ab-config-grpc.infra.fantacy.live&insecure=true"
    )
    cfg = Config(namespaces=["ns1"], config_service_addr=addr)  # no root injected
    auth = _TokenCache("dev-bearer-token", None)

    with caplog.at_level(logging.WARNING, logger="tipsy_ab_config"):
        _build_channel(cfg, addr, auth_plugin=auth)

    warnings = [r.getMessage() for r in caplog.records if r.levelno >= logging.WARNING]
    assert warnings, "expected a skip-verify warning when insecure=true and no root"
    # The warning must guide the user to the recommended fix.
    joined = " ".join(warnings)
    assert "tls_root_certificates" in joined or "likely fail verification" in joined, (
        f"warning should point at tls_root_certificates / verification: {warnings}"
    )
    # Fell back to system roots (no injected anchor).
    assert calls["secure"] is not None
    _, root_certificates = calls["secure"]["credentials"]
    assert root_certificates is None, "no injected root → system roots (None)"
    # Auth still attached even on the (likely-failing) best-effort path.
    assert _auth_interceptor_present(calls["secure"]["interceptors"])


# ===========================================================================
# 7. init()-level: gRPC address parse errors are parameter errors and are NOT
#    absorbed by startup_fail_open (mirrors Go
#    grpc_dial_test.go::TestInit_GRPCParseErrorIsParameterError, review M3).
#
# A bad gRPC address is rejected during _build_channel (which _init_grpc calls
# BEFORE _run_startup_pull), so the ValueError is raised straight out of init()
# and never reaches the fail-open path. We assert that even with
# startup_fail_open=True the call raises a tipsy_ab_config: ValueError rather
# than returning an empty-cache client.
# ===========================================================================


@pytest.mark.parametrize(
    "bad_addr, match",
    [
        # Q2: grpcs:// without an explicit port (no implicit :443).
        ("grpcs://ab-config-grpc.internal", "explicit port"),
        # design rule 5: http(s):// in gRPC mode is a parameter error.
        ("http://lb.internal:8080", "HTTP base URL"),
        # Q1: query on a plaintext grpc:// target is meaningless.
        ("grpc://ab-config-grpc:50051?insecure=true", "query parameters are only valid"),
    ],
)
async def test_init_grpc_parse_error_is_parameter_error_not_failopen(bad_addr, match):
    """init() rejects a bad gRPC address as a parameter error BEFORE startup pull.

    startup_fail_open=True must NOT swallow it: parse happens in _build_channel,
    which runs before the startup PullAll sweep, so the ValueError propagates.
    A non-empty token is supplied so the failure is unambiguously the address
    parse (not the missing-token guard).
    """
    cfg = Config(
        namespaces=["ns1"],
        config_service_addr=bad_addr,
        token=issue_test_token(),
        startup_fail_open=True,  # must NOT absorb a parameter error
        pull_retries=1,
        pull_timeout=1.0,
    )
    with pytest.raises(ValueError, match=match) as excinfo:
        await init(cfg)
    # All gRPC parse errors are tipsy_ab_config: parameter errors (design F6).
    assert str(excinfo.value).startswith("tipsy_ab_config:"), excinfo.value
