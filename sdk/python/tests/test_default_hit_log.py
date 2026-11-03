"""Default-value hit log tests (real-enrollment-dashboard design part-c §C1/§C6).

Contract under test: when dynamic ``get_config`` / ``get_config_default`` (and
the typed accessors built on them) resolve NO value and return the caller's
default, the SDK emits exactly ONE Info record per call:

- message ``get_config hit (default)`` (no ``tipsyabconfig:`` prefix, like the
  two existing Python hit messages);
- ``extra`` fields exactly {ns, key, reason="default", default_cause, uid,
  trace_id} — never version / experiment_id / group_id / release_id, never the
  default value itself.

``default_cause`` is the FIRST matching row of the C1 table: unknown_key >
fast_path > no_user > abtest_unavailable > ab_version_missing > no_full_release
> full_release_value_missing. Error exits (closed client, namespace errors,
missing ctx, cancellation) raise as before and emit no record.

Cross-language alignment: the cause scenarios come from the repo-shared list
``test/sdk-alignment/default_cause_scenarios.txt``, which the Go and Java
suites also execute row by row with the same expected cause. Edit that file,
not a Python-local table.

Observation: assertions target EMITTED records (``caplog`` scoped to the
``tipsy_ab_config`` logger). Every "no record" assertion is paired with a
positive control on the same client and capture.

Not proven here: a real server's empty-parameter-group payload (see the
dev-e2e scenario under test/dev-e2e/default-hit).
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, fields
from pathlib import Path

import grpc
import pytest
from tipsy_ab_config import (
    AbtestContextMissing,
    Config,
    NamespaceNotSubscribed,
    NamespaceRequired,
    SDKClosed,
    init,
)
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2
from tipsy_ab_config.cache import KeyState, NamespaceSnapshot
from tipsy_ab_config.client import _default_cause

from .conftest import issue_test_token, make_per_group_result, make_snapshot
from .test_enrollment_log import _FIELDS_BY_REASON, extra_fields

_LOGGER = "tipsy_ab_config"
_HIT_PREFIX = "get_config hit"
_DEFAULT_MSG = "get_config hit (default)"

_SCENARIO_FILE = (
    Path(__file__).resolve().parents[3]
    / "test"
    / "sdk-alignment"
    / "default_cause_scenarios.txt"
)

# The complete default_cause value set of part-c §C1.
_C1_CAUSES = frozenset(
    {
        "unknown_key",
        "fast_path",
        "no_user",
        "abtest_unavailable",
        "ab_version_missing",
        "no_full_release",
        "full_release_value_missing",
    }
)

# Closed vocabulary per column: an unknown value fails the load, so a typo in
# the shared file can never fall through to a builder branch silently.
_COLUMN_VALUES = {
    "snapshot": {"none", "key_absent", "key"},
    "full": {"-", "absent", "zero", "uncached"},
    "hdr": {"-", "absent", "true", "false"},
    "ctx": {"new", "empty", "mock_seeded", "mock_unseeded"},
    "uid": {"real", "empty", "zero"},
    "abtest": {
        "ok_empty",
        "ok_empty_group",
        "ok_other",
        "ok_v0",
        "ok_uncached",
        "rpc_error",
        "timeout",
        "not_configured",
    },
}

_UID_VALUES = {"real": "u-sc", "empty": "", "zero": "0"}


@dataclass(frozen=True)
class Scenario:
    id: str
    expect: str
    snapshot: str
    full: str
    hdr: str
    ctx: str
    uid: str
    abtest: str


def load_scenarios() -> list[Scenario]:
    """Parse + validate the shared list. A missing file fails (never skips)."""
    out: list[Scenario] = []
    seen: set[str] = set()
    for n, raw in enumerate(_SCENARIO_FILE.read_text().splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        cols = line.split()
        assert len(cols) == 8, f"{_SCENARIO_FILE}:{n}: want 8 columns: {line!r}"
        s = Scenario(*cols)
        assert s.id not in seen, f"duplicate scenario id {s.id}"
        seen.add(s.id)
        assert s.expect in _C1_CAUSES, f"{s.id}: {s.expect!r} is not a C1 cause"
        for col, allowed in _COLUMN_VALUES.items():
            v = getattr(s, col)
            assert v in allowed, f"{s.id}: column {col} has unknown value {v!r}"
        assert (s.snapshot == "key") == (s.full != "-") == (s.hdr != "-"), (
            f"{s.id}: full/hdr must be set iff snapshot=key"
        )
        assert s.ctx != "empty" or s.uid == "empty", f"{s.id}: ctx=empty implies uid=empty"
        out.append(s)
    assert out, f"shared scenario list {_SCENARIO_FILE} has no rows"
    return out


_SCENARIOS = load_scenarios()


def _hit_records(caplog: pytest.LogCaptureFixture) -> list[logging.LogRecord]:
    return [r for r in caplog.records if r.getMessage().startswith(_HIT_PREFIX)]


def _default_records(caplog: pytest.LogCaptureFixture) -> list[logging.LogRecord]:
    return [r for r in caplog.records if r.getMessage() == _DEFAULT_MSG]


def assert_default_line(
    rec: logging.LogRecord,
    *,
    ns: str,
    key: str,
    cause: str,
    uid: str,
    trace_id: str,
    default: str | None,
) -> None:
    """One default record: Info, exact message, EXACT extra field set, exact
    values, and (when ``default`` is given) the default value nowhere in it."""
    assert rec.getMessage() == _DEFAULT_MSG
    assert rec.levelno == logging.INFO
    got = extra_fields(rec)
    assert set(got) == set(_FIELDS_BY_REASON["default"]) and len(got) == len(
        _FIELDS_BY_REASON["default"]
    ), (
        f"default line extra fields = {got}, want exactly "
        f"{sorted(_FIELDS_BY_REASON['default'])} (no version / experiment_id / "
        "group_id / release_id / default value)"
    )
    assert rec.ns == ns
    assert rec.key == key
    assert rec.reason == "default"
    assert rec.default_cause == cause
    assert rec.uid == uid
    assert rec.trace_id == trace_id
    if default:
        rendered = [rec.getMessage()] + [repr(getattr(rec, f)) for f in got]
        assert not any(default in part for part in rendered), (
            f"default line leaks the caller's default {default!r}: {rendered}"
        )


def _scenario_snapshot(s: Scenario):
    keys = {"k2": (3, {3: "full-k2"})}
    hdr = {}
    if s.snapshot == "key":
        full = {"absent": None, "zero": 0, "uncached": 7}[s.full]
        keys["k"] = (full, {0: "stray-v0", 5: "v5"})
        if s.hdr != "absent":
            hdr["k"] = s.hdr == "true"
    return make_snapshot("ns1", 1, 1, keys, has_dynamic_resolution=hdr)


def _group(params):
    return make_per_group_result(params, experiment_id="exp-sc", group_id="grp-sc")


async def _scenario_client(s: Scenario, cfg_servicer, ab_servicer, cfg_addr, ab_addr):
    if s.snapshot != "none":
        cfg_servicer.set_pull_snapshot(_scenario_snapshot(s))
    abtest_addr = ab_addr
    abtest_timeout = 1.5
    if s.abtest == "ok_empty":
        ab_servicer.set_response("ns1", abtest_pb2.GetExperimentResultResponse())
    elif s.abtest == "ok_empty_group":
        ab_servicer.set_response("ns1", _group({}))
    elif s.abtest == "ok_other":
        ab_servicer.set_response("ns1", _group({"k2": 3}))
    elif s.abtest == "ok_v0":
        ab_servicer.set_response("ns1", _group({"k": 0}))
    elif s.abtest in ("ok_uncached", "timeout"):
        # timeout: content that would yield ab_version_missing if the timeout
        # did not fire.
        ab_servicer.set_response("ns1", _group({"k": 99}))
        if s.abtest == "timeout":
            ab_servicer.delay = 0.5
            abtest_timeout = 0.05
    elif s.abtest == "rpc_error":
        ab_servicer.set_error("ns1", grpc.StatusCode.UNAVAILABLE)
    elif s.abtest == "not_configured":
        abtest_addr = ""
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=abtest_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
            startup_fail_open=True,
            abtest_timeout=abtest_timeout,
        )
    )
    return cli


def _scenario_ctx(cli, s: Scenario):
    uid = _UID_VALUES[s.uid]
    if s.ctx == "empty":
        return cli.empty_abtest_context()
    if s.ctx == "mock_seeded":
        return cli.mock_abtest_context(uid, {"ns1": {}})
    if s.ctx == "mock_unseeded":
        return cli.mock_abtest_context(uid, {})
    return cli.new_abtest_context(uid, trace_id=f"tr-{s.id}")


def test_default_cause_helper_none_abtest_result():
    """The ``None`` arm of the cause helper: a ``None`` abtest result on a
    non-fast-path key is ``abtest_unavailable``, never "evaluated" (part-c §C1
    judgement point 1: Python's ``None`` must not be read as a successful
    evaluation). ``get_config`` cannot reach this arm today —
    ``wait_for_abtest`` never returns ``None``, and ``None`` on the fast path
    is classified earlier as ``fast_path`` — so it is asserted on the pure
    helper directly."""
    snap = NamespaceSnapshot(
        namespace="ns1",
        business_snapshot_seq=1,
        experiment_snapshot_seq=1,
        keys={"k": KeyState(full_release_version=None, versions={5: "v5"})},
    )
    assert _default_cause(snap, "k", None, "u") == "abtest_unavailable"


def test_scenario_list_shape():
    """The shared list covers every C1 cause and the four §C6 error-prone
    scenarios; E4 is a pair differing ONLY in the full-release id."""
    by_id = {s.id: s for s in _SCENARIOS}
    assert {s.expect for s in _SCENARIOS} == _C1_CAUSES
    for sid in (
        "E1.rpc_ok_empty_param_group",
        "E2.fast_path_valid_uid",
        "E3.merged_version_zero",
        "E4.full_id_absent",
        "E4.full_id_zero",
    ):
        assert sid in by_id, f"shared list is missing §C6 scenario {sid}"
    a, b = by_id["E4.full_id_absent"], by_id["E4.full_id_zero"]
    assert (a.full, b.full) == ("absent", "zero")
    diff = [f.name for f in fields(Scenario) if getattr(a, f.name) != getattr(b, f.name)]
    assert diff == ["id", "full"], f"E4 pair must differ only in full, differs in {diff}"


@pytest.mark.parametrize("s", _SCENARIOS, ids=[s.id for s in _SCENARIOS])
async def test_default_hit_log_shared_scenario(
    s: Scenario, cfg_servicer, ab_servicer, running_servers, caplog
):
    """Every shared row: one get_config call on a fresh client + ctx ⇒ the
    caller's default is returned and exactly one default record carries the
    row's default_cause with the exact field set."""
    cfg_addr, ab_addr = running_servers
    cli = await _scenario_client(s, cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        if s.snapshot == "none":
            assert cli.cache.snapshot("ns1") is None, "fixture: ns1 must have no snapshot"
        ctx = _scenario_ctx(cli, s)
        default = f"DEF-{s.id}"
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            val = await cli.get_config(ctx, "ns1", "k", default)
        assert val == default
        hits = _hit_records(caplog)
        assert len(hits) == 1, f"expected exactly one hit record, got {[h.getMessage() for h in hits]}"
        assert_default_line(
            hits[0],
            ns="ns1",
            key="k",
            cause=s.expect,
            uid=_UID_VALUES[s.uid],
            trace_id=ctx.trace_id,
            default=default,
        )
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Error exits, entry-point variants, call multiplicity, out-of-scope APIs.
# ---------------------------------------------------------------------------


async def _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr, **cfg_kw):
    """ns1 holds a no-full-release key ``k`` and a full-release key ``k2``;
    abtest succeeds without ``k`` ⇒ a valid call is a no_full_release default."""
    cfg_servicer.set_pull_snapshot(
        make_snapshot(
            "ns1",
            1,
            1,
            {
                "k": (None, {5: "v5"}),
                "k2": (3, {3: "full-k2"}),
                "bad": (4, {4: "not-a-number"}),
            },
        )
    )
    ab_servicer.set_response("ns1", _group({"k2": 3}))
    base = {
        "namespaces": ["ns1"],
        "config_service_addr": cfg_addr,
        "abtest_service_addr": ab_addr,
        "token": issue_test_token(),
        "pull_interval": 10.0,
        "pull_retries": 1,
    }
    base.update(cfg_kw)
    return await init(Config(**base))


async def _positive_control(cli, caplog) -> None:
    """A valid call emits one no_full_release default record and it is the
    ONLY hit record in the capture (so a preceding error exit emitted none)."""
    ctx = cli.new_abtest_context("u-ctl", trace_id="tr-ctl")
    assert await cli.get_config(ctx, "ns1", "k", "DEF-ctl") == "DEF-ctl"
    hits = _hit_records(caplog)
    assert len(hits) == 1, f"error exit emitted a hit record: {[h.getMessage() for h in hits]}"
    assert_default_line(
        hits[0], ns="ns1", key="k", cause="no_full_release", uid="u-ctl",
        trace_id="tr-ctl", default="DEF-ctl",
    )


async def test_error_exit_sdk_closed_no_record(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """Closed client raises SDKClosed and emits nothing. The positive control
    runs FIRST on the same client/capture (it cannot run after aclose)."""
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    with caplog.at_level(logging.INFO, logger=_LOGGER):
        await _positive_control(cli, caplog)
        await cli.aclose()
        ctx = cli.new_abtest_context("u-1")
        with pytest.raises(SDKClosed):
            await cli.get_config(ctx, "ns1", "k", "DEF")
        with pytest.raises(SDKClosed):
            await cli.get_config_default(ctx, "k", "DEF")
    assert len(_hit_records(caplog)) == 1, "the closed-client exit emitted a record"


async def test_error_exit_missing_ctx_no_record(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            with pytest.raises(AbtestContextMissing):
                await cli.get_config(None, "ns1", "k", "DEF")
            assert _hit_records(caplog) == []
            await _positive_control(cli, caplog)
    finally:
        await cli.aclose()


async def test_error_exit_namespace_required_no_record(
    cfg_servicer, ab_servicer, running_servers, caplog, monkeypatch
):
    monkeypatch.delenv("PROJECT_DEFAULT_NAMESPACE", raising=False)
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        ctx = cli.new_abtest_context("u-1")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            with pytest.raises(NamespaceRequired):
                await cli.get_config_default(ctx, "k", "DEF")
            with pytest.raises(NamespaceRequired):
                await cli.get_config(ctx, None, "k", "DEF")
            assert _hit_records(caplog) == []
            await _positive_control(cli, caplog)
    finally:
        await cli.aclose()


async def test_error_exit_namespace_not_subscribed_no_record(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        ctx = cli.new_abtest_context("u-1")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            with pytest.raises(NamespaceNotSubscribed):
                await cli.get_config(ctx, "not-subscribed", "k", "DEF")
            assert _hit_records(caplog) == []
            await _positive_control(cli, caplog)
    finally:
        await cli.aclose()


async def test_error_exit_cancelled_while_waiting_no_record(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """The caller's task is cancelled while awaiting the abtest result ⇒
    CancelledError propagates (abtest_context.py re-raises it) and no record."""
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(
        cfg_servicer, ab_servicer, cfg_addr, ab_addr, abtest_timeout=5.0
    )
    try:
        ab_servicer.delay = 2.0
        ctx = cli.new_abtest_context("u-1")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            task = asyncio.ensure_future(cli.get_config(ctx, "ns1", "k", "DEF"))
            await asyncio.sleep(0.1)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
            assert _hit_records(caplog) == []
            ab_servicer.delay = 0.0
            await _positive_control(cli, caplog)
    finally:
        await cli.aclose()


async def test_get_config_default_emits_with_resolved_ns(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(
        cfg_servicer, ab_servicer, cfg_addr, ab_addr, default_namespace="ns1"
    )
    try:
        ctx = cli.new_abtest_context("u-dns", trace_id="tr-dns")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            assert await cli.get_config_default(ctx, "k", "DEF-dns") == "DEF-dns"
        hits = _hit_records(caplog)
        assert len(hits) == 1
        assert_default_line(
            hits[0], ns="ns1", key="k", cause="no_full_release", uid="u-dns",
            trace_id="tr-dns", default="DEF-dns",
        )
    finally:
        await cli.aclose()


async def test_contextvar_ctx_emits(cfg_servicer, ab_servicer, running_servers, caplog):
    """ctx=None falls back to the abtest_ctx_var ctx; the record carries it."""
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            async with cli.abtest_scope("u-var", trace_id="tr-var"):
                assert await cli.get_config(None, "ns1", "k", "DEF-var") == "DEF-var"
        hits = _hit_records(caplog)
        assert len(hits) == 1
        assert_default_line(
            hits[0], ns="ns1", key="k", cause="no_full_release", uid="u-var",
            trace_id="tr-var", default="DEF-var",
        )
    finally:
        await cli.aclose()


async def test_typed_accessors_emit_and_parse_failure_does_not(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """Each dynamic typed accessor emits one default record on a no-value key
    and returns its typed default. A key whose value RESOLVES but fails to parse
    returns the default too, yet logs only its full hit — a value was resolved
    (part-c §C1)."""
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        calls = [
            ("bool", lambda c: cli.get_config_bool(c, "ns1", "k", True), True),
            ("long", lambda c: cli.get_config_long(c, "ns1", "k", 42), 42),
            ("double", lambda c: cli.get_config_double(c, "ns1", "k", 1.5), 1.5),
            ("string", lambda c: cli.get_config_string(c, "ns1", "k", "DEF-s"), "DEF-s"),
            ("json", lambda c: cli.get_config_json(c, "ns1", "k", {"d": 1}), {"d": 1}),
        ]
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            for i, (name, call, want) in enumerate(calls):
                ctx = cli.new_abtest_context("u-typed", trace_id=f"tr-typed-{name}")
                assert await call(ctx) == want, name
                defaults = _default_records(caplog)
                assert len(defaults) == i + 1, f"{name}: {len(defaults)} default records"
                assert_default_line(
                    defaults[i], ns="ns1", key="k", cause="no_full_release",
                    uid="u-typed", trace_id=f"tr-typed-{name}", default=None,
                )
            before = len(_hit_records(caplog))
            ctx = cli.new_abtest_context("u-typed")
            assert await cli.get_config_long(ctx, "ns1", "bad", 7) == 7
            new = _hit_records(caplog)[before:]
        assert [r.getMessage() for r in new] == ["get_config hit (full)"]
    finally:
        await cli.aclose()


async def test_every_call_emits_no_dedup(cfg_servicer, ab_servicer, running_servers, caplog):
    """No sampling / dedup: three calls on the SAME ctx ⇒ three records, while
    the abtest RPC stays memoised at one."""
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        before = ab_servicer.calls_by_ns.get("ns1", 0)
        ctx = cli.new_abtest_context("u-rep", trace_id="tr-rep")
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            for _ in range(3):
                assert await cli.get_config(ctx, "ns1", "k", "DEF-rep") == "DEF-rep"
        recs = _default_records(caplog)
        assert len(recs) == 3
        for r in recs:
            assert_default_line(
                r, ns="ns1", key="k", cause="no_full_release", uid="u-rep",
                trace_id="tr-rep", default="DEF-rep",
            )
        assert ab_servicer.calls_by_ns.get("ns1", 0) - before == 1
    finally:
        await cli.aclose()


async def test_static_and_get_all_do_not_emit(
    cfg_servicer, ab_servicer, running_servers, caplog
):
    """get_config_static / get_all_configs are out of scope (part-c §C1)."""
    cfg_addr, ab_addr = running_servers
    cli = await _plain_client(cfg_servicer, ab_servicer, cfg_addr, ab_addr)
    try:
        with caplog.at_level(logging.INFO, logger=_LOGGER):
            assert cli.get_config_static("ns1", "k", "DEF-s") == "DEF-s"
            got = await cli.get_all_configs(cli.new_abtest_context("u-all"), "ns1")
            assert "k" not in got
            assert _hit_records(caplog) == []
            await _positive_control(cli, caplog)
    finally:
        await cli.aclose()
