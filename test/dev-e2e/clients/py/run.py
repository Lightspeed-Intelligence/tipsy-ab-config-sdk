#!/usr/bin/env python3
"""ST4 client-correctness driver for the Tipsy AB-config Python SDK.

Exercises the Python SDK over BOTH transports (HTTP and gRPC) against the seeded
dev topology and asserts every fixtures/expectations.json row applicable to
``py_sdk_*``. Mirrors the Go SDK driver:

  - init the SDK once per transport,
  - for each applicable row, ``get_config`` / ``get_config_static`` and assert
    the resolved value (custom rows go through ``get_experiment_result``),
  - print PASS/FAIL per (transport, case) and a summary; exit non-zero on any
    failure or a degraded gRPC transport.

Transport notes (docs/dev-http-token.md):
  - HTTP mode uses httpx via the SDK ``[http]`` extra against AB_CONFIG_HTTP_BASE.
  - gRPC mode uses the dedicated Cloudflare-proxied domain
    (``dev-ab-config-grpc.infra.fantacy.live:443``) with standard TLS — no
    :authority override, no skip-verify, no Origin CA PEM workaround. The
    SDK address is ``grpcs://{grpc_addr}``. Legacy direct-IP form (with
    ``AB_CONFIG_GRPC_AUTHORITY`` + ``AB_CONFIG_GRPC_CA_PEM``) is kept as a
    fallback only when those env vars are explicitly set.

Env vars (never hard-code secrets):
  AB_CONFIG_HTTP_BASE       (default https://dev-ab-config.infra.fantacy.live)
  AB_CONFIG_GRPC_ADDR       (default dev-ab-config-grpc.infra.fantacy.live:443)
  AB_CONFIG_GRPC_AUTHORITY  (legacy override; if set, switches to IP-direct form)
  AB_CONFIG_GRPC_CA_PEM     (legacy override; Origin CA PEM for direct-IP gRPC TLS)
  AB_CONFIG_TOKEN           (REQUIRED)

Run (after setup_venv.sh):
  .venv/bin/python test/dev-e2e/clients/py/run.py
  .venv/bin/python test/dev-e2e/clients/py/run.py --transport http
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional

NAMESPACES = ["demo-test", "for_dev_agent_test"]
CUSTOM_KEY = "__custom__"
DEFAULT_SENTINEL = "<DEFAULT>"


def env_or(key: str, default: str) -> str:
    v = os.environ.get(key)
    return v if v else default


def resolve_fixtures_path(flag: Optional[str]) -> Path:
    if flag:
        return Path(flag).resolve()
    # Walk up to the repo root (go.mod) and use the well-known relative path.
    here = Path(__file__).resolve()
    for parent in [here] + list(here.parents):
        if (parent / "go.mod").exists():
            return parent / "test/dev-e2e/fixtures/expectations.json"
    # Fallback: relative to this file (clients/py → ../../fixtures).
    return (here.parent.parent.parent / "fixtures/expectations.json").resolve()


def load_expectations(path: Path) -> List[Dict[str, Any]]:
    with path.open("r", encoding="utf-8") as f:
        return json.load(f)


def raw_attrs(envelope: Optional[Dict[str, Any]]) -> Dict[str, Any]:
    """Convert the typed Value envelope ({"country":{"s":"US"}}) back to a RAW
    dict ({"country":"US"}) — the SDK re-encodes raw values into Value on the
    wire, so passing the envelope would double-wrap."""
    if not envelope:
        return {}
    out: Dict[str, Any] = {}
    for k, v in envelope.items():
        if isinstance(v, dict):
            if "s" in v:
                out[k] = v["s"]
            elif "b" in v:
                out[k] = bool(v["b"])
            elif "d" in v:
                out[k] = float(v["d"])
            elif "i" in v:
                # proto int64 is a JSON string in the envelope.
                out[k] = int(v["i"])
            else:
                out[k] = v
        else:
            out[k] = v
    return out


class Results:
    def __init__(self) -> None:
        self.passed = 0
        self.failed = 0
        self.grpc_degraded = False

    def ok(self, msg: str) -> None:
        self.passed += 1
        print(f"PASS  {msg}")

    def bad(self, msg: str) -> None:
        self.failed += 1
        print(f"FAIL  {msg}")


def applies_to(exp: Dict[str, Any], client: str) -> bool:
    return client in exp.get("applies_to", [])


def kv_equal(got: Dict[str, Any], want: Dict[str, Any]) -> bool:
    if len(got) != len(want):
        return False
    for k, wv in want.items():
        if k not in got:
            return False
        gv = got[k]
        if isinstance(wv, (int, float)) and isinstance(gv, (int, float)):
            if float(gv) != float(wv):
                return False
        elif gv != wv:
            return False
    return True


def struct_to_dict(struct) -> Dict[str, Any]:
    """Convert a protobuf Struct (custom_flat_kv) to a plain dict."""
    if struct is None:
        return {}
    try:
        from google.protobuf.json_format import MessageToDict

        return MessageToDict(struct)
    except Exception:  # noqa: BLE001
        # Fallback: manual field walk.
        out: Dict[str, Any] = {}
        for k, v in struct.fields.items():
            kind = v.WhichOneof("kind")
            out[k] = getattr(v, kind) if kind else None
        return out


async def run_transport(client_tag: str, cfg, exps: List[Dict[str, Any]], r: Results) -> None:
    """Init the SDK with cfg, run all applicable expectations, then aclose.

    A gRPC init/connect failure is caught, marked degraded, and does not crash
    the run (HTTP mode still proceeds)."""
    import tipsy_ab_config as tac

    print(f"\n=== transport: {client_tag} ===")

    try:
        cli = await tac.init(cfg)
    except Exception as e:  # noqa: BLE001
        if client_tag == "py_sdk_grpc":
            r.grpc_degraded = True
            print(f"WARNING: gRPC SDK init failed; marking gRPC degraded and skipping: {e}")
            return
        r.bad(f"[{client_tag}] init failed: {e}")
        return

    try:
        # The abtest_pb2 enum constants for the custom path.
        from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

        # Warm up the abtest path once before the asserted loop: the first RPC
        # over a freshly-dialed cross-internet connection can be slow; priming it
        # avoids a one-off slow first call skewing a single assertion. Ignored.
        try:
            wctx = cli.new_abtest_context("warmup-probe", None, NAMESPACES[0])
            await cli.get_config(wctx, NAMESPACES[0], "welcome_text", "")
        except Exception:  # noqa: BLE001
            pass

        for exp in exps:
            if not applies_to(exp, client_tag):
                continue
            await assert_expectation(cli, client_tag, exp, r, tac, abtest_pb2)
    finally:
        await cli.aclose()


async def assert_expectation(cli, client_tag, exp, r: Results, tac, abtest_pb2) -> None:
    ns = exp["ns"]
    uid = exp["user_id"]
    key = exp["key"]
    attrs = raw_attrs(exp.get("user_attrs"))
    source = exp.get("source", "")

    if key == CUSTOM_KEY:
        want = exp["expected_value"]
        if not isinstance(want, dict):
            r.bad(f"[{client_tag}] {ns}/{uid} custom: expected_value not an object")
            return
        try:
            resp = await cli.get_experiment_result(
                ns,
                user_info=tac.UserInfo(uid=uid, attrs=attrs),
                experiment_type=abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CUSTOM_PARAMS,
                display_type=abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_FLAT_KV,
            )
        except Exception as e:  # noqa: BLE001
            r.bad(f"[{client_tag}] get_experiment_result(custom) {ns}/{uid}: {e}")
            return
        got = struct_to_dict(resp.custom_flat_kv if resp.HasField("custom_flat_kv") else None)
        if kv_equal(got, want):
            r.ok(f"[{client_tag}] custom {ns}/{uid} custom_flat_kv = {want} ({source})")
        else:
            r.bad(f"[{client_tag}] custom {ns}/{uid}: custom_flat_kv {got} want {want}")
        return

    want_val = exp["expected_value"]
    if not isinstance(want_val, str):
        r.bad(f"[{client_tag}] {ns}/{uid}/{key}: expected_value not a string")
        return

    ctx = cli.new_abtest_context(uid, attrs, ns)
    try:
        got = await cli.get_config(ctx, ns, key, DEFAULT_SENTINEL)
    except Exception as e:  # noqa: BLE001
        r.bad(f"[{client_tag}] get_config {ns}/{uid}/{key}: {e}")
        return
    if got == want_val:
        r.ok(f"[{client_tag}] get_config {ns}/{uid}/{key} = {got!r} ({source})")
    else:
        r.bad(f"[{client_tag}] get_config {ns}/{uid}/{key}: got {got!r} want {want_val!r} ({source})")


def build_http_config(tac, token: str, http_base: str):
    return tac.Config(
        namespaces=NAMESPACES,
        config_service_addr=http_base,
        abtest_service_addr=http_base,
        token=token,
        transport="http",
        startup_fail_open=False,
    )


def build_grpc_config(tac, token: str, grpc_addr: str, authority: str, ca_pem: Optional[bytes]):
    # Default: standard TLS to the gRPC domain (no :authority override, no
    # skip-verify). Legacy: when `authority` is set (operator opted into the
    # direct-IP form), append the query string and require the Origin CA PEM.
    if authority:
        target = f"grpcs://{grpc_addr}?authority={authority}&insecure=true"
    else:
        target = f"grpcs://{grpc_addr}"
    return tac.Config(
        namespaces=NAMESPACES,
        config_service_addr=target,
        abtest_service_addr=target,
        token=token,
        transport="grpc",
        tls_root_certificates=ca_pem,
        startup_fail_open=False,
    )


async def amain(args) -> int:
    # Quiet the SDK's own degraded-mode warnings unless verbose.
    logging.basicConfig(level=logging.ERROR)

    token = os.environ.get("AB_CONFIG_TOKEN", "")
    if not token:
        print("FATAL: AB_CONFIG_TOKEN env var is required (see docs/dev-http-token.md)", file=sys.stderr)
        return 2

    http_base = env_or("AB_CONFIG_HTTP_BASE", "https://dev-ab-config.infra.fantacy.live")
    grpc_addr = env_or("AB_CONFIG_GRPC_ADDR", "dev-ab-config-grpc.infra.fantacy.live:443")
    # Legacy direct-IP overrides; empty by default → standard TLS to grpc_addr.
    authority = os.environ.get("AB_CONFIG_GRPC_AUTHORITY", "")
    ca_pem_path = os.environ.get("AB_CONFIG_GRPC_CA_PEM", "")

    fixtures_path = resolve_fixtures_path(args.fixtures)
    exps = load_expectations(fixtures_path)

    print("================================================================")
    print("ST4 Python SDK client-correctness driver")
    print(f"  http base     : {http_base}")
    print(f"  grpc addr     : {grpc_addr}")
    if authority:
        print(f"  grpc authority: {authority} (legacy IP-direct fallback)")
    print(f"  fixtures      : {fixtures_path} ({len(exps)} rows)")
    print("  WARNING       : hitting the SHARED dev environment")
    print("================================================================")

    import tipsy_ab_config as tac

    r = Results()

    if args.transport in ("both", "http"):
        await run_transport("py_sdk_http", build_http_config(tac, token, http_base), exps, r)

    if args.transport in ("both", "grpc"):
        ca_pem: Optional[bytes] = None
        if ca_pem_path:
            try:
                ca_pem = Path(ca_pem_path).read_bytes()
            except OSError as e:
                print(f"WARNING: could not read AB_CONFIG_GRPC_CA_PEM ({ca_pem_path}): {e}")
        elif authority:
            # Legacy IP-direct mode: standard TLS won't validate the origin
            # cert against the bare IP, so a CA PEM is required.
            print(
                "NOTE: AB_CONFIG_GRPC_AUTHORITY is set (IP-direct mode) but "
                "AB_CONFIG_GRPC_CA_PEM is not — grpcio cannot skip TLS verify, "
                "so gRPC mode is best-effort and will likely fail TLS verification."
            )
        await run_transport(
            "py_sdk_grpc",
            build_grpc_config(tac, token, grpc_addr, authority, ca_pem),
            exps,
            r,
        )

    print("----------------------------------------------------------------")
    print(f"SUMMARY: {r.passed} passed, {r.failed} failed (of {r.passed + r.failed} checks)")
    if r.grpc_degraded:
        print("NOTE: gRPC transport was DEGRADED (connect/init failed) — see WARNING above.")
    if r.failed > 0 or r.grpc_degraded:
        return 1
    return 0


def main() -> None:
    p = argparse.ArgumentParser(description="Tipsy AB-config Python SDK e2e driver")
    p.add_argument("--transport", choices=["both", "http", "grpc"], default="both")
    p.add_argument("--fixtures", default="", help="path to expectations.json")
    args = p.parse_args()
    sys.exit(asyncio.run(amain(args)))


if __name__ == "__main__":
    main()
