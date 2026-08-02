#!/usr/bin/env python3
"""ST9 combo data-plane driver for the Python SDK (assertion #2).

Mirrors the Go driver one-for-one. The fixture makes frozen != live on purpose
(group frozen at v1 while the full release moved to v2); if they were equal the
assertion would pass either way and prove nothing.

Also asserts the abtest_fallback DELTA is zero: a correct-looking value could
otherwise have been delivered by the ab->full fallback arm (client.py:553)
instead of the abtest main path. Delta rather than absolute, so unrelated
startup/other-key fallbacks cannot make it red for the wrong reason.
"""
from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys

import tipsy_ab_config as tac

NS = "st9_combo"

# (uid, role, key, expected value, why)
CASES = [
    ("st9-probe-9", "holdout", "managed_a", "LIVE_v1", "held by h1 -> frozen v1"),
    ("st9-probe-24", "holdout", "managed_a", "LIVE_v1", "held by h1 -> frozen v1"),
    ("st9-probe-4", "opt", "managed_a", "LIVE_v1", "held by o1 -> frozen v1"),
    ("st9-probe-10", "opt", "managed_a", "LIVE_v1", "held by o1 -> frozen v1"),
    # These two take the LIVE value because no combo group holds managed_a FOR
    # THEM — not because they are outside every combo. st9-probe-1 is in c2mig's
    # o_wide group (holding mig_key only), which makes it the stronger case: "in a
    # combo group, but that group does not hold this key => live value". See
    # fixture.md section 10 for the full membership table; claiming managed_a into
    # c2mig or c3zero would turn these red.
    # st9-probe-0 is deliberately NOT used here: assertion #5 pins it into the
    # holdout-opt domain, making its expected value ambiguous.
    ("st9-probe-1", "in c2mig/o_wide", "managed_a", "LIVE_v2", "group does not hold this key -> live full v2"),
    ("st9-probe-2", "in E_mig/gA", "managed_a", "LIVE_v2", "group does not hold this key -> live full v2"),
    ("st9-probe-9", "holdout", "unheld_key", "UNHELD_v1", "NOT held -> live full"),
    ("st9-probe-4", "opt", "unheld_key", "UNHELD_v1", "NOT held -> live full"),
    ("st9-probe-1", "in c2mig/o_wide", "unheld_key", "UNHELD_v1", "no group holds it -> live full"),
]


async def run(label: str, cfg, results: dict) -> None:
    print(f"\n=== {label} ===")
    try:
        cli = await tac.init(cfg)
    except Exception as e:  # noqa: BLE001
        print(f"FAIL [{label}] init: {e}")
        results["fail"] += 1
        return
    try:
        before = cli.metrics.abtest_fallback_total(NS)
        for uid, _role, key, want, why in CASES:
            ctx = cli.new_abtest_context(uid)
            try:
                got = await cli.get_config(ctx, NS, key, "<DEFAULT>")
            except Exception as e:  # noqa: BLE001
                print(f"FAIL [{label}] {uid}/{key}: {e}")
                results["fail"] += 1
                continue
            if got == want:
                print(f"PASS [{label}] {uid:13} {key:11} = {got!r:12} ({why})")
                results["pass"] += 1
            else:
                print(f"FAIL [{label}] {uid:13} {key:11} got={got!r} want={want!r} ({why})")
                results["fail"] += 1
        delta = cli.metrics.abtest_fallback_total(NS) - before
        if delta == 0:
            print(f"PASS [{label}] abtest_fallback delta = 0 (values came from the abtest main path)")
            results["pass"] += 1
        else:
            print(f"FAIL [{label}] abtest_fallback delta = {delta} (values may be fallback-delivered)")
            results["fail"] += 1
    finally:
        await cli.aclose()


async def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--transport", default="both", choices=["grpc", "http", "both"])
    ap.add_argument("--http", default="http://localhost:8081")
    ap.add_argument("--grpc", default="grpc://localhost:50052")
    args = ap.parse_args()

    token = os.environ.get("AB_CONFIG_TOKEN")
    if not token:
        print("FATAL: AB_CONFIG_TOKEN required", file=sys.stderr)
        return 2

    logging.basicConfig(level=logging.ERROR)
    results = {"pass": 0, "fail": 0}

    if args.transport in ("both", "http"):
        await run("py_http", tac.Config(
            namespaces=[NS], config_service_addr=args.http,
            abtest_service_addr=args.http, token=token,
            transport="http", startup_fail_open=True), results)
    if args.transport in ("both", "grpc"):
        await run("py_grpc", tac.Config(
            namespaces=[NS], config_service_addr=args.grpc,
            abtest_service_addr=args.grpc, token=token,
            transport="grpc", startup_fail_open=True), results)

    total = results["pass"] + results["fail"]
    print(f"\n----\nSUMMARY: pass={results['pass']} fail={results['fail']} of {total} checks")
    per = len(CASES) + 1
    want_total = 2 * per if args.transport == "both" else per
    if total != want_total:
        print(f"OBSERVATION-COUNT MISMATCH: ran {total} checks, expected {want_total}")
        return 1
    print(f"OBSERVATION-COUNT OK ({want_total} expected)")
    return 1 if results["fail"] else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
