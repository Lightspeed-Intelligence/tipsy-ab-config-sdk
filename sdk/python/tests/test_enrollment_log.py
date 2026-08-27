"""getConfig enrollment-log contract tests (actual-enrollment-log design §4-§6).

Every dynamic ``get_config`` hit emits ONE Info record whose message text is
unchanged (``get_config hit (abtest)`` / ``get_config hit (full)``) and whose
``extra`` carries the SLS parsing contract (design §4 F7):

- always: ``reason`` + ``uid`` + ``trace_id`` + ``ns`` + ``key`` + ``version``;
- conditional, by reason (absent fields are OMITTED — the key is not set,
  never an empty-string placeholder):
  - ``reason=experiment``        ⇒ ``experiment_id`` + ``group_id``;
  - ``reason=gray_whitelist``    ⇒ ``release_id`` (int);
  - ``reason=full_release`` / ``reason=abtest_unattributed`` ⇒ neither.

Assertion placement rule (design Testing Plan 1, no-op form 6): every reason
value is asserted on an EMITTED log record (``caplog`` scoped to the
``tipsy_ab_config`` logger — precedent test_grpc_target.py), never on an enum
declaration.

Memo convention (three-SDK-wide): any case observing "second and later call"
behaviour builds a NEW AbtestContext — the per-(request, ns) memo
(abtest_context.py:190) otherwise serves a cached result and RPC-count /
degrade assertions pass trivially.

What these tests do NOT prove (do not read the greens as proof of these):
- The MockAbtestContext case only proves the unattributed fallback works; it
  says nothing about attribution being written correctly (a merge that never
  writes attribution also yields ``abtest_unattributed`` there — design
  Testing Plan 1, no-op form 3). Attribution correctness is proven by the
  experiment/gray/paired-unattributed cases.
- Nothing here observes a real server; see test_enrollment_alignment.py's
  capability-boundary note.
"""

from __future__ import annotations

import logging

import grpc
import pytest

from tipsy_ab_config import Config, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_per_group_result,
    make_snapshot,
)

_LOGGER = "tipsy_ab_config"
# Matches "get_config hit (abtest)" and "get_config hit (full)" but NOT
# "get_config_static hit" (differs at the char after "get_config").
_HIT_PREFIX = "get_config hit"

# The always-present field set of the SLS contract; the conditional fields.
_ALWAYS_FIELDS = ("reason", "uid", "trace_id", "ns", "key", "version")
_CONDITIONAL_FIELDS = ("experiment_id", "group_id", "release_id")


def _hit_records(caplog: pytest.LogCaptureFixture) -> list[logging.LogRecord]:
    return [
        r for r in caplog.records if r.getMessage().startswith(_HIT_PREFIX)
    ]


def _assert_contract(
    rec: logging.LogRecord,
    *,
    reason: str,
    ns: str,
    key: str,
    version: int,
    uid: str,
    trace_id: str,
    experiment_id: str | None = None,
    group_id: str | None = None,
    release_id: int | None = None,
) -> None:
    """Assert one hit record against the full SLS field contract.

    Full equality on every field (no containment: the version is a templated
    id-like value and substring matches would let "renders the version" and
    "renders the release_id" both pass on overlapping digits). ``None`` for a
    conditional field means it MUST be omitted (attribute absent).
    """
    assert rec.levelno == logging.INFO
    assert rec.reason == reason
    assert rec.ns == ns
    assert rec.key == key
    assert rec.version == version
    assert rec.uid == uid
    assert rec.trace_id == trace_id
    for field, want in (
        ("experiment_id", experiment_id),
        ("group_id", group_id),
        ("release_id", release_id),
    ):
        if want is None:
            assert not hasattr(rec, field), (
                f"{field} must be OMITTED (reason={reason}), "
                f"got {getattr(rec, field)!r}"
            )
        else:
            assert getattr(rec, field) == want
    if release_id is not None:
        # SLS contract: release_id is logged as an integer (wire int64).
        assert isinstance(rec.release_id, int) and not isinstance(
            rec.release_id, bool
        )


async def _make_client(
    cfg_servicer: FakeConfigServicer,
    cfg_addr: str,
    ab_addr: str,
    keys=None,
    has_dynamic_resolution=None,
):
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1",
            1,
            1,
            keys or {"k": (1, {1: "full-v1", 2: "ab-v2"})},
            has_dynamic_resolution=has_dynamic_resolution,
        )
    )
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


# ---------------------------------------------------------------------------
# The four reason values, each asserted on an emitted line.
# ---------------------------------------------------------------------------


async def test_hit_log_reason_experiment(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """Attributed experiment hit ⇒ reason=experiment + experiment_id/group_id,
    release_id omitted; uid/trace_id/ns/key/version all present (Goal 1+2)."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    ab_servicer.set_response(
        "ns1",
        make_per_group_result({"k": 2}, experiment_id="exp-7", group_id="grp-9"),
    )
    try:
        ctx = cli.new_abtest_context("u1", {"country": "US"}, trace_id="t-exp")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "ab-v2"
        recs = _hit_records(caplog)
        assert len(recs) == 1, f"expected exactly one hit line, got {len(recs)}"
        assert recs[0].getMessage() == "get_config hit (abtest)"
        _assert_contract(
            recs[0],
            reason="experiment",
            ns="ns1",
            key="k",
            version=2,
            uid="u1",
            trace_id="t-exp",
            experiment_id="exp-7",
            group_id="grp-9",
            release_id=None,
        )
    finally:
        await cli.aclose()


async def test_hit_log_reason_gray_whitelist(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """Gray whitelist hit ⇒ reason=gray_whitelist + int release_id;
    experiment_id/group_id omitted."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer, cfg_addr, ab_addr, keys={"k": (1, {1: "full-v1", 5: "gray-v5"})}
    )
    ab_servicer.set_response(
        "ns1", make_per_group_result(gray_hits=[(7, {"k": 5})])
    )
    try:
        ctx = cli.new_abtest_context("u1", trace_id="t-gray")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "gray-v5"
        recs = _hit_records(caplog)
        assert len(recs) == 1
        assert recs[0].getMessage() == "get_config hit (abtest)"
        _assert_contract(
            recs[0],
            reason="gray_whitelist",
            ns="ns1",
            key="k",
            version=5,
            uid="u1",
            trace_id="t-gray",
            experiment_id=None,
            group_id=None,
            release_id=7,
        )
    finally:
        await cli.aclose()


async def test_hit_log_reason_full_release(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """No abtest hit ⇒ full branch: reason=full_release, no conditional
    fields, trace_id present (the Python trace_id backfill, Goal 4)."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(cfg_servicer, cfg_addr, ab_addr)
    ab_servicer.set_response("ns1", make_per_group_result())  # empty per-group
    try:
        ctx = cli.new_abtest_context("u1", trace_id="t-full")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "full-v1"
        recs = _hit_records(caplog)
        assert len(recs) == 1
        assert recs[0].getMessage() == "get_config hit (full)"
        _assert_contract(
            recs[0],
            reason="full_release",
            ns="ns1",
            key="k",
            version=1,
            uid="u1",
            trace_id="t-full",
            experiment_id=None,
            group_id=None,
            release_id=None,
        )
    finally:
        await cli.aclose()


async def test_hit_log_reason_unattributed_paired_with_attributed(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """F3 on the log surface: an empty-id group's key still resolves (value
    track untouched) and logs reason=abtest_unattributed with no conditional
    fields, while — in the SAME response — an attributed group's key logs
    reason=experiment. The pairing keeps this case discriminating: a mutant
    that never writes attribution at all would still produce
    abtest_unattributed for k_noattr, but fails on k_attr (no-op form 1)."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer,
        cfg_addr,
        ab_addr,
        keys={
            "k_attr": (1, {1: "fa", 88: "aa"}),
            "k_noattr": (1, {1: "fn", 77: "an"}),
        },
    )
    ab_servicer.set_response(
        "ns1",
        make_per_group_result(
            {"k_attr": 88},
            extra_groups=[("", "", {"k_noattr": 77})],
        ),
    )
    try:
        ctx = cli.new_abtest_context("u1", trace_id="t-unattr")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            # Same ctx: the merge ran once (memoised); each get_config still
            # emits its own hit line.
            assert await cli.get_config(ctx, "ns1", "k_noattr", "def") == "an"
            assert await cli.get_config(ctx, "ns1", "k_attr", "def") == "aa"
        recs = {r.key: r for r in _hit_records(caplog)}
        assert set(recs) == {"k_noattr", "k_attr"}
        _assert_contract(
            recs["k_noattr"],
            reason="abtest_unattributed",
            ns="ns1",
            key="k_noattr",
            version=77,
            uid="u1",
            trace_id="t-unattr",
            experiment_id=None,
            group_id=None,
            release_id=None,
        )
        _assert_contract(
            recs["k_attr"],
            reason="experiment",
            ns="ns1",
            key="k_attr",
            version=88,
            uid="u1",
            trace_id="t-unattr",
            experiment_id="exp-1",
            group_id="grp-1",
            release_id=None,
        )
    finally:
        await cli.aclose()


async def test_hit_log_zero_release_id_gray_unattributed(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """release_id=0 gray hit ⇒ value still resolves (F3), logged as
    abtest_unattributed with release_id OMITTED (0 is not an attribution)."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer, cfg_addr, ab_addr, keys={"k": (1, {1: "full-v1", 5: "gray-v5"})}
    )
    ab_servicer.set_response(
        "ns1", make_per_group_result(gray_hits=[(0, {"k": 5})])
    )
    try:
        ctx = cli.new_abtest_context("u1", trace_id="t-gray0")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            assert await cli.get_config(ctx, "ns1", "k", "def") == "gray-v5"
        recs = _hit_records(caplog)
        assert len(recs) == 1
        _assert_contract(
            recs[0],
            reason="abtest_unattributed",
            ns="ns1",
            key="k",
            version=5,
            uid="u1",
            trace_id="t-gray0",
            experiment_id=None,
            group_id=None,
            release_id=None,
        )
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Cross-source conflict on the log surface (gate-level semantic, Goal 2/AC8).
# ---------------------------------------------------------------------------


async def test_hit_log_cross_source_conflict_logs_gray(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """Gray vs experiment on the same key ⇒ the emitted line is the GRAY hit:
    reason=gray_whitelist + gray's release_id + gray's versionId; the
    experiment ids must NOT appear. Distinct versionIds (5 vs 2) per the
    conflict-fixture discipline (conflictkey_test.go:79-80)."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer,
        cfg_addr,
        ab_addr,
        keys={"k": (1, {1: "full-v1", 2: "exp-v2", 5: "gray-v5"})},
    )
    ab_servicer.set_response(
        "ns1",
        make_per_group_result({"k": 2}, gray_hits=[(7, {"k": 5})]),
    )
    try:
        ctx = cli.new_abtest_context("u1", trace_id="t-conflict")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "gray-v5", "gray must win the cross-source conflict"
        recs = _hit_records(caplog)
        assert len(recs) == 1
        _assert_contract(
            recs[0],
            reason="gray_whitelist",
            ns="ns1",
            key="k",
            version=5,
            uid="u1",
            trace_id="t-conflict",
            experiment_id=None,
            group_id=None,
            release_id=7,
        )
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Fallback / degrade / fast-path / mock.
# ---------------------------------------------------------------------------


async def test_ab_full_fallback_warn_carries_trace_id(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """ab version missing from the snapshot ⇒ WARN (with trace_id — the §6
    backfill) + the hit line downgrades to reason=full_release."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer, cfg_addr, ab_addr, keys={"k": (1, {1: "full-only"})}
    )
    ab_servicer.set_response("ns1", make_per_group_result({"k": 99}))
    try:
        before = cli.metrics.abtest_fallback_total("ns1")
        ctx = cli.new_abtest_context("u1", trace_id="t-warn")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "full-only"

        warns = [
            r
            for r in caplog.records
            if r.levelno == logging.WARNING
            and "falling back to full" in r.getMessage()
        ]
        assert len(warns) == 1, f"expected one ab→full WARN, got {len(warns)}"
        assert warns[0].trace_id == "t-warn", (
            "the ab→full fallback WARN must carry the ctx trace_id (design §6)"
        )
        assert warns[0].ns == "ns1"

        recs = _hit_records(caplog)
        assert len(recs) == 1
        assert recs[0].getMessage() == "get_config hit (full)"
        _assert_contract(
            recs[0],
            reason="full_release",
            ns="ns1",
            key="k",
            version=1,
            uid="u1",
            trace_id="t-warn",
            experiment_id=None,
            group_id=None,
            release_id=None,
        )
        # Fallback metric: strict before/after delta (+1), not a total.
        assert cli.metrics.abtest_fallback_total("ns1") - before == 1
    finally:
        await cli.aclose()


async def test_rpc_failure_degrades_reason_full_release_delta_one(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """F8: GetExperimentResult fails ⇒ empty result, get_config lands on full,
    reason=full_release, and abtest_fallback moves by EXACTLY +1 (before/after
    delta — a total-value assertion could be satisfied for the wrong reason,
    no-op form 8). Fresh ctx: a reused ctx's memo would skip the failing RPC
    entirely and the delta would trivially be 0/pass-by-accident."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer, cfg_addr, ab_addr, keys={"k": (1, {1: "full-v1"})}
    )
    ab_servicer.set_error("ns1", grpc.StatusCode.UNAVAILABLE)
    try:
        before = cli.metrics.abtest_fallback_total("ns1")
        ctx = cli.new_abtest_context("u1", trace_id="t-degrade")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", "def")
        assert val == "full-v1"
        assert cli.metrics.abtest_fallback_total("ns1") - before == 1

        recs = _hit_records(caplog)
        assert len(recs) == 1
        _assert_contract(
            recs[0],
            reason="full_release",
            ns="ns1",
            key="k",
            version=1,
            uid="u1",
            trace_id="t-degrade",
            experiment_id=None,
            group_id=None,
            release_id=None,
        )
    finally:
        await cli.aclose()


async def test_fast_path_no_rpc_reason_full_release(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """has_dynamic_resolution=False fast path: ZERO RPC and the hit line still
    carries the full contract with reason=full_release. Fresh ctx + only the
    False key queried (memo convention — see test_get_config.py's fast-path
    comment block for the false-green mechanics)."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer,
        cfg_addr,
        ab_addr,
        keys={"pure": (10, {10: "full10", 11: "ab11"})},
        has_dynamic_resolution={"pure": False},
    )
    # Tripwire: an armed DIFFERENT ab version would flip the value (and the
    # reason) if the fast path wrongly consulted abtest.
    ab_servicer.set_response("ns1", make_per_group_result({"pure": 11}))
    try:
        ctx = cli.new_abtest_context("u-fast", trace_id="t-fast")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "pure", "def")
        assert val == "full10"
        assert ab_servicer.calls == 0
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 0
        recs = _hit_records(caplog)
        assert len(recs) == 1
        _assert_contract(
            recs[0],
            reason="full_release",
            ns="ns1",
            key="pure",
            version=10,
            uid="u-fast",
            trace_id="t-fast",
            experiment_id=None,
            group_id=None,
            release_id=None,
        )
    finally:
        await cli.aclose()


async def test_mock_ctx_hit_logs_unattributed(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """MockAbtestContext seeds key_versions only ⇒ reason=abtest_unattributed.

    Scope honesty (design Testing Plan 1, no-op form 3): this proves the
    unattributed FALLBACK works — it holds even if the merge never writes
    attribution — so it is NOT attribution-mechanism coverage. That coverage
    lives in the experiment/gray/paired cases above."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer, cfg_addr, ab_addr, keys={"k": (1, {1: "full", 9: "ab9"})}
    )
    try:
        ctx = cli.mock_abtest_context("u-mock", {"ns1": {"k": 9}})
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            assert await cli.get_config(ctx, "ns1", "k", "def") == "ab9"
        recs = _hit_records(caplog)
        assert len(recs) == 1
        assert recs[0].reason == "abtest_unattributed"
        assert recs[0].version == 9
        assert recs[0].uid == "u-mock"
        assert recs[0].trace_id  # mock ctx still auto-generates a trace_id
        for field in _CONDITIONAL_FIELDS:
            assert not hasattr(recs[0], field)
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# F4: log position — getConfig logs, getAllConfigs does not (design §6).
# ---------------------------------------------------------------------------


async def test_get_config_logs_get_all_configs_does_not_same_capture(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """F4 (design Testing Plan 1, no-op form 7): within ONE caplog capture —

    positive: the get_config call emits its hit Info line WITH attribution;
    positive: the get_all_configs call emits its aggregate Debug line
              (proves the capture window and logger scoping are LIVE for the
              get_all_configs call — without this, the negative below would
              pass trivially under a mis-scoped capture);
    negative: the get_all_configs call emits ZERO per-key hit lines.

    The two calls use distinct trace_ids so every record is attributable to
    its call. get_all_configs runs FIRST so a per-key leak cannot hide behind
    the single-key line. NOTE: the leak get_all_configs would produce under
    the old shared-method logging includes BOTH "(abtest)" and "(full)"
    per-key lines — the negative assertion counts either."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer,
        cfg_addr,
        ab_addr,
        keys={
            "kx": (1, {1: "fx", 2: "ax"}),
            "ky": (5, {5: "fy"}),
        },
    )
    ab_servicer.set_response(
        "ns1",
        make_per_group_result({"kx": 2}, experiment_id="exp-7", group_id="grp-9"),
    )
    try:
        # Fresh ctx per call (memo convention) with distinct trace_ids.
        ctx_all = cli.new_abtest_context("u1", trace_id="t-all")
        ctx_single = cli.new_abtest_context("u1", trace_id="t-single")
        with caplog.at_level(logging.DEBUG, logger=_LOGGER):
            got = await cli.get_all_configs(ctx_all, "ns1")
            val = await cli.get_config(ctx_single, "ns1", "kx", "def")
        assert got == {"kx": "ax", "ky": "fy"}
        assert val == "ax"

        hits = _hit_records(caplog)
        by_trace: dict = {}
        for r in hits:
            by_trace.setdefault(getattr(r, "trace_id", "<missing>"), []).append(r)

        # NEGATIVE: not a single per-key hit line from the get_all_configs
        # call (its two resolved keys would have produced two lines under the
        # old shared-method logging).
        assert "t-all" not in by_trace, (
            f"get_all_configs must not emit per-key hit lines, got "
            f"{[r.getMessage() for r in by_trace['t-all']]}"
        )

        # POSITIVE 1: the single-key call emitted exactly one hit line, with
        # attribution — proves hit lines as such ARE captured here.
        assert [r.key for r in by_trace.get("t-single", [])] == ["kx"]
        _assert_contract(
            by_trace["t-single"][0],
            reason="experiment",
            ns="ns1",
            key="kx",
            version=2,
            uid="u1",
            trace_id="t-single",
            experiment_id="exp-7",
            group_id="grp-9",
            release_id=None,
        )

        # POSITIVE 2: the aggregate Debug line for the get_all_configs call
        # was captured — proves the capture window covered that call too, so
        # the NEGATIVE above is an observation, not a scoping accident.
        aggregates = [
            r
            for r in caplog.records
            if "get_all_configs" in r.getMessage()
            and getattr(r, "trace_id", None) == "t-all"
        ]
        assert len(aggregates) == 1, (
            "expected exactly one aggregate get_all_configs Debug line "
            f"for t-all, got {len(aggregates)}"
        )
        assert aggregates[0].levelno == logging.DEBUG
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Static-path regression (AC9): contract boundary r2-F7.
# ---------------------------------------------------------------------------


async def test_static_hit_log_unchanged_no_reason(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """get_config_static's line keeps its current shape: source=full_static,
    NO reason, NO uid. Downstream identifies enrollment events by the PRESENCE
    of `reason` (design §4 contract boundary) — a reason leaking onto the
    static line would corrupt that predicate."""
    cfg_addr, ab_addr = running_servers
    cli = await _make_client(
        cfg_servicer, cfg_addr, ab_addr, keys={"k": (10, {10: "value10"})}
    )
    try:
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            assert cli.get_config_static("ns1", "k", "def") == "value10"
        statics = [
            r
            for r in caplog.records
            if r.getMessage() == "get_config_static hit"
        ]
        assert len(statics) == 1
        rec = statics[0]
        assert rec.source == "full_static"
        assert rec.ns == "ns1"
        assert rec.key == "k"
        assert rec.version == 10
        assert not hasattr(rec, "reason"), (
            "static path must NOT grow a reason field (AC9 / r2-F7 boundary)"
        )
        assert not hasattr(rec, "uid")
        for field in _CONDITIONAL_FIELDS:
            assert not hasattr(rec, field)
    finally:
        await cli.aclose()
