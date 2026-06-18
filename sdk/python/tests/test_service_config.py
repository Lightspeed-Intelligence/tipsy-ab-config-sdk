"""Unit tests for the ``dns:///`` → round_robin service-config injection.

Covers ST2 of the ``sdk-headless-roundrobin`` task. Two layers under test:

1. ``_service_config_for(dial_target)`` — a pure synchronous helper that
   returns the round_robin service-config JSON string when the dial target
   starts with ``dns:///`` and ``None`` for every other dial-target form.

2. ``_build_channel(cfg, addr, auth_plugin)`` — must append
   ``("grpc.service_config", <json>)`` to the channel options ONLY when the
   parsed dial target uses the ``dns:///`` resolver; for every other address
   form (bare host:port, grpc://, grpcs://, grpcs://...?authority=...,
   passthrough:///, unix:, xds:///) the option must NOT appear (design
   Acceptance Criteria #10 — the non-``dns:///`` negative regression).

Style notes:
  - Skip module cleanly when grpcio is absent (``client.py`` imports ``grpc``
    at module top level). Mirrors ``test_grpc_target.py``.
  - ``_service_config_for`` is sync; ``_build_channel`` is sync; no event
    loop needed. We patch ``grpc.aio.insecure_channel`` / ``secure_channel``
    with spies (same pattern as ``test_grpc_target.py``::
    ``_patch_channel_builders``) so no real channel is opened.
"""

from __future__ import annotations

import json

import pytest

# Skip the whole module cleanly when grpcio is absent — see test_grpc_target.py
# for the same pattern. client.py imports grpc at module top level.
grpc = pytest.importorskip(
    "grpc", reason="grpcio required to import tipsy_ab_config.client"
)

from tipsy_ab_config.client import (  # noqa: E402  (after importorskip)
    Config,
    _build_channel,
    _service_config_for,
)


# ===========================================================================
# 1. _service_config_for — dns:/// dial targets return the round_robin JSON
# ===========================================================================


@pytest.mark.parametrize(
    "dial_target",
    [
        "dns:///foo",
        "dns:///foo:50051",
        "dns:///foo.bar.svc.cluster.local:50051",
    ],
)
def test_service_config_for_dns_prefix(dial_target):
    """dns:/// dial targets must produce the round_robin service-config JSON.

    Asserts three layers of the contract:
      1. result is not None;
      2. the raw string contains the substring ``round_robin`` (cheap grep so
         a future formatting change doesn't silently drop the LB name);
      3. parsing the JSON yields ``loadBalancingConfig=[{ "round_robin": {} }]``
         — the canonical gRPC service-config shape grpcio/grpc-go consume.
    """
    result = _service_config_for(dial_target)

    assert result is not None, f"dns:/// target {dial_target!r} must opt into round_robin"
    assert "round_robin" in result, (
        f"service-config JSON must mention round_robin (got {result!r})"
    )

    parsed = json.loads(result)
    lb_cfg = parsed["loadBalancingConfig"]
    assert isinstance(lb_cfg, list) and lb_cfg, "loadBalancingConfig must be a non-empty list"
    assert "round_robin" in lb_cfg[0], (
        f"loadBalancingConfig[0] must contain a round_robin entry (got {lb_cfg[0]!r})"
    )


# ===========================================================================
# 2. _service_config_for — every other dial-target form returns None
#    (design Acceptance Criteria #10 — non-dns:/// regression guard)
# ===========================================================================


@pytest.mark.parametrize(
    "dial_target",
    [
        # empty string — defensive (a misconfig must not silently flip LB).
        "",
        # bare host:port (the historic K8S ClusterIP form).
        "foo:50051",
        "foo.bar:50051",
        # scheme-prefixed plaintext / TLS / TLS+query forms (方案 Y).
        "grpc://foo:50051",
        "grpcs://foo:443",
        "grpcs://foo:443?authority=x.y&insecure=true",
        # native grpc resolver prefixes that are NOT dns:///.
        "passthrough:///foo:50051",
        "unix:/tmp/abconfig.sock",
        "xds:///foo",
    ],
)
def test_service_config_for_other_prefixes(dial_target):
    """Design AC #10 (non-``dns:///`` negative regression).

    Every non-``dns:///`` dial-target form — bare host:port, grpc://, grpcs://
    (with or without ?authority/?insecure query), passthrough:///, unix:,
    xds:/// — must return ``None`` from ``_service_config_for`` so the SDK
    does NOT inject ``grpc.service_config`` and grpcio keeps its default
    ``pick_first`` LB policy. Acceptance Criteria #10 calls this out as the
    backward-compatibility backstop for every existing deployment shape.
    """
    assert _service_config_for(dial_target) is None, (
        f"non-dns:/// target {dial_target!r} must NOT trigger round_robin "
        "(design AC #10 negative regression)"
    )


# ===========================================================================
# 3. _build_channel — dns:/// addr injects the service-config channel option
# ===========================================================================


def _patch_channel_builders(monkeypatch):
    """Replace ``grpc.aio.insecure_channel`` / ``secure_channel`` + ssl creds
    with spies. Returns a dict capturing which builder ran and its options.

    Mirrors the same helper in ``test_grpc_target.py`` so this file stays
    consistent with the existing test style (no real channels opened, no
    event loop required for sync ``_build_channel``).
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


def _service_config_option(options):
    """Return the value of the single ``grpc.service_config`` option, or None.

    The option name is the canonical grpcio key (design Risk R2 resolution:
    ``grpc.service_config``, not ``grpc.service_config_disable_resolution``).
    """
    matches = [v for (k, v) in options if k == "grpc.service_config"]
    if not matches:
        return None
    assert len(matches) == 1, (
        f"grpc.service_config should appear at most once in options (got {matches!r})"
    )
    return matches[0]


def test_build_channel_dns_target_injects_service_config(monkeypatch):
    """dns:/// dial target → ``grpc.service_config`` option carrying round_robin
    JSON is threaded into ``grpc.aio.insecure_channel``.

    Asserts:
      - the insecure builder ran (dns:/// is a plaintext native-resolver form,
        ``_parse_grpc_target`` leaves ``use_tls=False``);
      - the options list contains exactly one ``("grpc.service_config", <json>)``
        entry;
      - that JSON parses to ``loadBalancingConfig=[{"round_robin": {}}]``.
    """
    calls = _patch_channel_builders(monkeypatch)
    addr = "dns:///foo:50051"
    cfg = Config(namespaces=["ns1"], config_service_addr=addr)

    _build_channel(cfg, addr, auth_plugin=None)

    assert calls["insecure"] is not None, "dns:/// is plaintext → insecure_channel"
    assert calls["secure"] is None, "dns:/// must NOT take the TLS path"
    # The dial target passes through verbatim (rule 2 of _parse_grpc_target).
    assert calls["insecure"]["target"] == addr

    sc_json = _service_config_option(calls["insecure"]["options"])
    assert sc_json is not None, (
        "dns:/// dial target MUST inject a ('grpc.service_config', ...) option"
    )
    assert "round_robin" in sc_json, (
        f"injected service-config JSON must mention round_robin (got {sc_json!r})"
    )
    parsed = json.loads(sc_json)
    assert "round_robin" in parsed["loadBalancingConfig"][0]


@pytest.mark.parametrize(
    "addr",
    [
        # Bare host:port — the historic K8S ClusterIP form. AC #10 backstop.
        "foo:50051",
        # grpcs:// with explicit port — public / dev TLS form. AC #10 backstop.
        "grpcs://foo:443",
    ],
)
def test_build_channel_other_targets_no_service_config(monkeypatch, addr):
    """Design AC #10 — for non-``dns:///`` addresses the SDK must NOT inject
    the ``grpc.service_config`` channel option.

    Covers the two highest-traffic regression points:
      - bare ``host:port`` (the legacy ClusterIP / loopback / VM form);
      - ``grpcs://host:443`` (the public TLS / Dev domain form).

    Either builder may run (plaintext → insecure_channel; TLS →
    secure_channel); whichever ran, its options list MUST NOT contain a
    ``grpc.service_config`` entry, so grpcio keeps its default ``pick_first``
    LB policy on these existing deployment shapes.
    """
    calls = _patch_channel_builders(monkeypatch)
    cfg = Config(namespaces=["ns1"], config_service_addr=addr)

    _build_channel(cfg, addr, auth_plugin=None)

    # Exactly one builder must have been called; pick its options.
    if calls["insecure"] is not None:
        assert calls["secure"] is None
        options = calls["insecure"]["options"]
    else:
        assert calls["secure"] is not None, (
            f"either insecure_channel or secure_channel must have been called for {addr!r}"
        )
        options = calls["secure"]["options"]

    assert _service_config_option(options) is None, (
        f"non-dns:/// addr {addr!r} must NOT inject grpc.service_config "
        "(design AC #10 negative regression)"
    )
