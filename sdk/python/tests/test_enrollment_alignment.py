"""Alignment tests (design Testing Plan 3a, user-named category "alignment").

Prove that the NEW SDK's per-group response + local merge yields the SAME
key→versionId map an OLD SDK obtained by consuming the server's FLAT_KV
``config_flat_kv`` for the same logical scenario.

Both fixtures of every scenario are authored here, side by side:

- the FLAT side is what an old SDK would have received — its expected map is
  hand-computed from the platform's flat assembly rules
  (tipsy-ab-config internal/abtest/compute/engine.go:329-385 @ dd3cf76):
  gray hits are written first and own their keys unconditionally
  (engine.go:355-356); experiment groups fill the rest;
- the PER-GROUP side is the same logical hits expressed as groups[] +
  gray_hits[], merged locally by the new SDK.

Capability boundary (design r3-F4 — MUST NOT be over-read): both fixtures and
the flat expectation are OUR OWN encoding of the platform rules, so these
tests verify "SDK merge == our understanding of the platform" and are
STRUCTURALLY UNABLE to detect a real server whose per-group assembly deviates
from its flat assembly. That risk is carried by the platform-side source
review + platform test suite run (Testing Plan 3b) and the pre-release live
check (R1/AC10). Do not read these greens as server-equivalence proof.

Weak scenario (same-source conflict): per the user relaxation, only
"winner ∈ candidates + key not dropped" is asserted — see
test_enrollment_merge.py's module docstring for the verbatim contract.
Multi-gray conflicts have NO alignment case at all: that wire shape is
unreachable (topology.go:433-437 collapse), so there is no flat-side
expectation to compute (design 3a explicitly excludes it; the defensive
guard lives in test_enrollment_merge.py).

Memo convention: every scenario builds a FRESH AbtestContext (the per-ctx
(request, ns) memo would otherwise serve a cached merge — see
abtest_context.py:190).
"""

from __future__ import annotations

from tipsy_ab_config import Config, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_per_group_result,
    make_snapshot,
)


async def _make_client(cfg_servicer: FakeConfigServicer, cfg_addr: str, ab_addr: str):
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    return await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )


async def _merged(cli, ab_servicer: FakeAbtestServicer, resp) -> dict:
    """Merge a canned per-group response via a FRESH ctx (memo convention)."""
    ab_servicer.set_response("ns1", resp)
    ctx = cli.new_abtest_context("u-align", {"country": "US"})
    result = await ctx.wait_for_abtest("ns1")
    return dict(result.key_versions)


# ---------------------------------------------------------------------------
# Strict scenarios (gate-level): merged(per-group) == flat map, exact equality.
# ---------------------------------------------------------------------------


async def test_align_pure_experiment(cfg_servicer, ab_servicer, running_servers):
    """One experiment group, no gray. Flat rule: single owner ⇒ identity map."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        # Old-SDK wire: config_flat_kv exactly the group's params_versions
        # (engine.go:340-385, no gray ownership, no conflicts).
        flat = make_exp_result({"k": 201, "j": 202})
        per_group = make_per_group_result({"k": 201, "j": 202})
        got = await _merged(cli, ab_servicer, per_group)
        assert got == dict(flat.config_flat_kv)
    finally:
        await cli.aclose()


async def test_align_pure_gray(cfg_servicer, ab_servicer, running_servers):
    """One gray release, no experiments. Flat rule: gray writes all its keys."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        # Old-SDK wire: gray keys land in config_flat_kv (engine.go:335-337).
        flat = make_exp_result({"k": 301, "g": 302})
        per_group = make_per_group_result(gray_hits=[(7, {"k": 301, "g": 302})])
        got = await _merged(cli, ab_servicer, per_group)
        assert got == dict(flat.config_flat_kv)
    finally:
        await cli.aclose()


async def test_align_gray_experiment_overlap_gray_wins(
    cfg_servicer, ab_servicer, running_servers
):
    """STRICT: gray + experiment overlap on one key ⇒ flat has the GRAY value.

    Flat expectation hand-computed per engine.go:355-356: gray writes k=410
    first; the experiment's k=420 hits `grayOwned { continue }` and is
    dropped; its non-conflicting key other=421 lands. NOT derived from the
    slogan "whitelist > experiment" — transcribed from the implementation.

    Fixture discipline: candidates carry DIFFERENT versionIds (410 vs 420),
    platform conflictkey_test.go:79-80 — with equal values a priority
    inversion would produce the same map and this case would assert nothing.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        flat = make_exp_result({"k": 410, "other": 421})
        per_group = make_per_group_result(
            {"k": 420, "other": 421},
            gray_hits=[(7, {"k": 410})],
        )
        got = await _merged(cli, ab_servicer, per_group)
        assert got == dict(flat.config_flat_kv)
        # Belt-and-braces: pin the conflicted key to the gray candidate so a
        # wrong flat fixture above cannot silently weaken this gate case.
        assert got["k"] == 410
    finally:
        await cli.aclose()


async def test_align_empty(cfg_servicer, ab_servicer, running_servers):
    """Empty on both shapes ⇒ empty map. Zero merge-discriminating power
    (see test_enrollment_merge.py::test_merge_empty_response); kept because
    the 3a scenario list names it, never counted as merge coverage."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        flat = make_exp_result({})
        got = await _merged(cli, ab_servicer, make_per_group_result())
        assert got == dict(flat.config_flat_kv) == {}
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Weak scenario (non-gate, user relaxation).
# ---------------------------------------------------------------------------


async def test_align_same_source_experiment_conflict_weak(
    cfg_servicer, ab_servicer, running_servers
):
    """Two experiment groups on one key: winner ∈ candidates + key not lost.

    The platform's flat side resolves this last-write-wins (engine.go:380,
    conflictkey_test.go:81-82 pins 202), but the user relaxed same-source
    conflicts to "any candidate is acceptable", so this case deliberately
    does NOT assert equality with the platform's winner — only the direct
    encoding of the relaxed contract. Distinct candidate versionIds
    (101 vs 202) keep the "key dropped" and "value invented" mutants red.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        per_group = make_per_group_result(
            {"k": 101, "a": 1},
            extra_groups=[("exp-2", "grp-2", {"k": 202, "b": 2})],
        )
        got = await _merged(cli, ab_servicer, per_group)
        assert set(got) == {"k", "a", "b"}
        assert got["k"] in {101, 202}
        assert got["a"] == 1
        assert got["b"] == 2
    finally:
        await cli.aclose()
