"""Local per-group merge algorithm tests (actual-enrollment-log design §3).

The internal per-ns fetch now requests ``display_type=EACH_EXPERIMENT_GROUP``
and merges ``gray_hits`` + ``groups`` locally into the ``_ComputeResult``
``key_versions`` map (key → versionId). These tests pin the merge's VALUE
track, observed through ``AbtestContext.wait_for_abtest`` — the same object
every ``get_config`` consumes.

Guarantee levels (design §3, user decision 2025-08-26):

- CROSS-source conflict (gray vs experiment, same key) — STRICT, gate-level:
  gray wins unconditionally (platform engine.go:355-356 semantics).
- SAME-source conflict (multi experiment groups / multi gray hits, same key)
  — WEAK, relaxed by the user: the winner must be ∈ the candidate versionId
  set and the key must NOT be dropped; the winner's identity is NOT asserted.
  Contract (verbatim from the design): 正常情况下不会发生同类型 key 冲突，
  同类型 key 冲突是异常情况，此时平台 + SDK 只需保障至少返回可选值中的一个
  就算符合承诺。

Test convention (three-SDK-wide, design Testing Plan): any case observing
"second and later call" behaviour MUST build a NEW AbtestContext — the ctx
memoises the per-(request, ns) result (abtest_context.py:190 _ensure_fetch),
so a reused ctx serves the cached merge and the assertion passes trivially.

What these tests do NOT prove (do not read the greens as proof of these):
- They cannot detect a REAL server whose per-group assembly disagrees with
  its flat assembly — every fixture here is authored by us. That capability
  belongs to the platform-side review/tests (design Testing Plan 3b) and the
  pre-release live-environment check (design R1 / AC10).
- The empty-response case has ZERO discriminating power for the merge (an
  entirely broken merge also yields an empty result); it is a shape guard
  only and must not be counted as merge coverage.
- Attribution (reason/experiment_id/...) is asserted on emitted LOG lines in
  test_enrollment_log.py, not here — key_versions equality alone says nothing
  about the attribution track.
"""

from __future__ import annotations

from tipsy_ab_config import Config, init
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_per_group_result,
    make_snapshot,
)


async def _make_client(cfg_servicer: FakeConfigServicer, cfg_addr: str, ab_addr: str):
    # A minimal snapshot: the merge itself never reads the config snapshot,
    # but a subscribed ns is required for _ensure_fetch to fire the RPC.
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
    """Set the canned per-group response and return the merged key_versions.

    Always builds a FRESH AbtestContext (memo convention, module docstring) so
    each scenario re-executes the merge instead of reading a cached result.
    """
    ab_servicer.set_response("ns1", resp)
    ctx = cli.new_abtest_context("u-merge", {"country": "US"})
    result = await ctx.wait_for_abtest("ns1")
    return dict(result.key_versions)


# ---------------------------------------------------------------------------
# Strict scenarios (gate-level).
# ---------------------------------------------------------------------------


async def test_merge_pure_experiment(
    cfg_servicer, ab_servicer, running_servers
):
    """groups only ⇒ every params_versions entry lands in key_versions."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli, ab_servicer, make_per_group_result({"k": 201, "j": 202})
        )
        # Full-map equality (not containment): a merge that drops or invents
        # keys must go red.
        assert got == {"k": 201, "j": 202}
    finally:
        await cli.aclose()


async def test_merge_pure_gray(cfg_servicer, ab_servicer, running_servers):
    """gray_hits only ⇒ every key_versions entry lands in key_versions."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli,
            ab_servicer,
            make_per_group_result(gray_hits=[(7, {"k": 301, "g": 302})]),
        )
        assert got == {"k": 301, "g": 302}
    finally:
        await cli.aclose()


async def test_merge_cross_source_gray_beats_experiment(
    cfg_servicer, ab_servicer, running_servers
):
    """STRICT (gate-level): gray vs experiment on the same key ⇒ gray wins.

    Replicates platform engine.go:355-356 (`if _, grayOwned := ...; grayOwned
    { continue }`): the experiment group must never overwrite a gray-owned
    key, while its non-conflicting keys still land.

    Fixture discipline (platform conflictkey_test.go:79-80, quoted verbatim in
    the design): the two candidates MUST carry different versionIds — `Exact
    winner, not "one of {101,202}"`. With equal values a priority inversion
    would pass on the identical result (no-op form 4). Here gray=410 vs
    experiment=420.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli,
            ab_servicer,
            make_per_group_result(
                {"k": 420, "other": 421},
                gray_hits=[(7, {"k": 410})],
            ),
        )
        # Exact equality: k resolved to the GRAY versionId, and the
        # experiment's non-conflicting key survived.
        assert got == {"k": 410, "other": 421}
    finally:
        await cli.aclose()


async def test_merge_empty_response(cfg_servicer, ab_servicer, running_servers):
    """groups + gray_hits both empty ⇒ empty result (falls to full/default).

    ZERO discriminating power for the merge mechanism (a completely broken
    merge also produces {}); kept as a shape guard only. Do NOT count this
    case as merge coverage (design Testing Plan 1, no-op form 1).
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(cli, ab_servicer, make_per_group_result())
        assert got == {}
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# F3 value/attribution decoupling: keyVersions writes never depend on the
# attribution fields being valid (design §3 core invariant).
# ---------------------------------------------------------------------------


async def test_merge_empty_id_group_value_still_written(
    cfg_servicer, ab_servicer, running_servers
):
    """A group with empty experiment_id/group_id still contributes its values.

    F3 fixture constraint (design Testing Plan 1): the empty-id group is
    PAIRED with an attributed group so that "writes neither attribution nor
    value" mutants cannot hide behind an all-unattributed fixture. The
    discriminating assertion is the VALUE map: skipping the empty-id group
    would drop k_noattr entirely (a value-resolution change, not just an
    attribution downgrade).
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli,
            ab_servicer,
            make_per_group_result(
                {"k_attr": 88},
                extra_groups=[("", "", {"k_noattr": 77})],
            ),
        )
        assert got == {"k_attr": 88, "k_noattr": 77}
    finally:
        await cli.aclose()


async def test_merge_zero_release_id_gray_value_still_written(
    cfg_servicer, ab_servicer, running_servers
):
    """A gray_hit with release_id=0 still contributes its values (F3).

    Paired with an attributed gray hit for the same reason as above. Passed in
    ascending release_id order (0 < 7) to model the wire's sort contract.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli,
            ab_servicer,
            make_per_group_result(
                gray_hits=[(0, {"k0": 55}), (7, {"kg": 66})]
            ),
        )
        assert got == {"k0": 55, "kg": 66}
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Same-source conflicts — WEAK assertions only (user relaxation, design §3).
# ---------------------------------------------------------------------------


async def test_merge_same_source_experiment_conflict_weak(
    cfg_servicer, ab_servicer, running_servers
):
    """Two experiment groups claim the same key ⇒ winner ∈ candidates, no loss.

    WEAK by user decision: we assert only the contract "至少返回可选值中的一个"
    — the winner is one of the candidate versionIds and the key is NOT
    dropped. We deliberately do NOT assert WHICH candidate wins (the platform
    treats same-source conflicts as gated dirty data; SDK mirrors its
    last-write-wins as best effort without guaranteeing it) and do NOT assert
    stability across repeated merges (user decision, third round: no such
    test; the ordered-traversal implementation constraint is a code comment).
    Candidates carry DISTINCT versionIds (101 vs 202) so a "drop the key"
    mutant and an "invent a value" mutant both go red.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli,
            ab_servicer,
            make_per_group_result(
                {"k": 101, "a": 1},
                extra_groups=[("exp-2", "grp-2", {"k": 202, "b": 2})],
            ),
        )
        # Key set is exact: nothing dropped, nothing invented.
        assert set(got) == {"k", "a", "b"}
        # Winner ∈ candidate set — the direct encoding of the user contract.
        assert got["k"] in {101, 202}
        # Non-conflicting keys from BOTH groups always land.
        assert got["a"] == 1
        assert got["b"] == 2
    finally:
        await cli.aclose()


async def test_merge_same_source_multi_gray_conflict_defensive(
    cfg_servicer, ab_servicer, running_servers
):
    """Two gray_hits claim the same key — DEFENSIVE guard only.

    This wire shape is structurally unreachable from the real server (design
    §3 / r3-F2: platform computeGray topology.go:433-437 collapses to one
    releaseID per key BEFORE assembleGrayHits buckets, so the same key can
    never appear in two gray_hits). The synthetic fixture guards the SDK's
    defensive first-writer code against panics/key loss ONLY; it is NOT
    wire-level equivalence evidence and has no flat-side expectation to
    compare against (which is why alignment 3a excludes this scenario).
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        got = await _merged(
            cli,
            ab_servicer,
            make_per_group_result(
                gray_hits=[(1, {"k": 11, "x": 12}), (2, {"k": 22, "y": 23})]
            ),
        )
        assert set(got) == {"k", "x", "y"}
        assert got["k"] in {11, 22}
        assert got["x"] == 12
        assert got["y"] == 23
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# experiment_type filter + internal request shape.
# ---------------------------------------------------------------------------


async def test_merge_ignores_non_config_version_groups(
    cfg_servicer, ab_servicer, running_servers
):
    """Only experiment_type=CONFIG_VERSION groups feed the merge (design §3
    step 2). A CUSTOM_PARAMS group's params_versions must be ignored.

    Note this filter is a DELIBERATE fail-closed exception to the F3 "value
    never depends on metadata" invariant (design r6 suggestion 1): the
    platform guards the field's population (probe M10) and a custom_params
    group's params_versions is not a config_version value to begin with.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        resp = make_per_group_result({"k": 2})
        g = resp.groups.add()
        g.experiment_id = "exp-cp"
        g.group_id = "grp-cp"
        g.experiment_type = abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CUSTOM_PARAMS
        g.params_versions["k_cp"] = 999
        got = await _merged(cli, ab_servicer, resp)
        assert got == {"k": 2}
    finally:
        await cli.aclose()


async def test_internal_fetch_request_shape_per_group(
    cfg_servicer, ab_servicer, running_servers
):
    """The internal per-ns fetch sends CONFIG_VERSION + EACH_EXPERIMENT_GROUP.

    Asserted on the proto request the fake server actually received (design
    Testing Plan 1 "内部 fetch 请求形状"). The public GetExperimentResult
    passthrough shape is covered — and must stay untouched — in
    test_v2_namespace.py::test_get_experiment_result_client.
    """
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    try:
        await _merged(cli, ab_servicer, make_per_group_result({"k": 2}))
        req = ab_servicer.last_req
        assert req is not None
        assert req.namespace == "ns1"
        assert (
            req.experiment_type
            == abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION
        )
        assert (
            req.display_type
            == abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP
        )
    finally:
        await cli.aclose()
