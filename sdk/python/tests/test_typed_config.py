"""Typed config accessor tests (bool/long/double/string/json, static + dynamic).

Mirrors the Go SDK ``typed_config_test.go`` semantics but follows Python idioms:
values stay STRING end-to-end and are parsed at the edge. Covers, for each typed
method:

- hit → parsed value,
- miss → default,
- empty → default (dynamic path only; the static path preserves a genuine ""),
- parse-fail → default (long/double/json; bool never fails),

plus the lenient-bool matrix, the arbitrary-precision long round-trip, and the
JSON object/array/malformed cases.
"""

from __future__ import annotations

import pytest

from tipsy_ab_config import Config, init
from tipsy_ab_config.client import _parse_bool_lenient

from .conftest import (
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


# ---------------------------------------------------------------------------
# Shared client builder. Seeds ns1 full-release values covering every type +
# malformed inputs, and (for the dynamic abtest-hit cases) an experiment
# version per key so the abtest path can be exercised too.
# ---------------------------------------------------------------------------

# full_release_version → value, plus an abtest version (2) per key so an armed
# GetExperimentResult can steer resolution to the ab value in the dynamic tests.
_KEYS = {
    # bool
    "b_true": (1, {1: "true", 2: "false"}),
    "b_false": (1, {1: "false"}),
    "b_one": (1, {1: "1"}),
    "b_garbage": (1, {1: "garbage"}),
    # long / int
    "l_42": (1, {1: "42", 2: "999"}),
    "l_big53": (1, {1: "9007199254740993"}),
    "l_huge": (1, {1: "123456789012345678901234567890"}),
    "l_neg": (1, {1: "  -7  "}),
    "l_bad": (1, {1: "not-a-number"}),
    "l_float": (1, {1: "3.14"}),
    # double / float
    "d_pi": (1, {1: "3.14", 2: "2.71"}),
    "d_int": (1, {1: "10"}),
    "d_bad": (1, {1: "not-a-float"}),
    # string
    "s_hello": (1, {1: "hello", 2: "ab-hello"}),
    "s_empty": (1, {1: ""}),
    # json
    "j_obj": (1, {1: '{"a": 1, "b": [2, 3]}', 2: '{"ab": true}'}),
    "j_arr": (1, {1: "[1, 2, 3]"}),
    "j_bad": (1, {1: "{not valid json"}),
}


async def _client(cfg_servicer, running_servers):
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1, _KEYS))
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
# Lenient bool matrix — pure helper, never touches the network.
# ---------------------------------------------------------------------------


def test_parse_bool_lenient_matrix():
    truthy = ["true", "TRUE", "True", "  true  ", "1", " 1 ", "tRuE"]
    falsy = ["false", "FALSE", "False", "0", " 0 ", "yes", "no", "garbage", "", "  ", "2", "01", "1.0"]
    for s in truthy:
        assert _parse_bool_lenient(s) is True, f"expected True for {s!r}"
    for s in falsy:
        assert _parse_bool_lenient(s) is False, f"expected False for {s!r}"


def test_parse_bool_lenient_never_raises():
    # Feed a pile of odd-but-str inputs; the contract is "never raises".
    for s in ["", " ", "\t\n", "true\x00", "１", "null", "None", "-1"]:
        _parse_bool_lenient(s)  # must not raise


# ---------------------------------------------------------------------------
# Static bool
# ---------------------------------------------------------------------------


async def test_static_bool_hit_miss(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        assert cli.get_config_static_bool("ns1", "b_true", False) is True
        assert cli.get_config_static_bool("ns1", "b_one", False) is True
        assert cli.get_config_static_bool("ns1", "b_false", True) is False
        assert cli.get_config_static_bool("ns1", "b_garbage", True) is False
        # miss → default (both provided-True and default-False)
        assert cli.get_config_static_bool("ns1", "nope", True) is True
        assert cli.get_config_static_bool("ns1", "nope") is False
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Static long (int; arbitrary precision)
# ---------------------------------------------------------------------------


async def test_static_long_hit_miss_parsefail(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        assert cli.get_config_static_long("ns1", "l_42", -1) == 42
        assert cli.get_config_static_long("ns1", "l_neg", -1) == -7  # whitespace trimmed
        # miss → default
        assert cli.get_config_static_long("ns1", "nope", 77) == 77
        assert cli.get_config_static_long("ns1", "nope") == 0
        # parse-fail → default (non-numeric AND float syntax both fail int())
        assert cli.get_config_static_long("ns1", "l_bad", 5) == 5
        assert cli.get_config_static_long("ns1", "l_float", 5) == 5
    finally:
        await cli.aclose()


async def test_static_long_precision_lossless(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        # 2^53 + 1: a float64/JSON round-trip would collapse this to 2^53.
        assert cli.get_config_static_long("ns1", "l_big53", 0) == 9007199254740993
        assert cli.get_config_static_long("ns1", "l_big53", 0) != 9007199254740992
        # Way beyond int64 — Python int is unbounded; must round-trip exactly.
        assert (
            cli.get_config_static_long("ns1", "l_huge", 0)
            == 123456789012345678901234567890
        )
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Static double
# ---------------------------------------------------------------------------


async def test_static_double_hit_miss_parsefail(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        assert cli.get_config_static_double("ns1", "d_pi", -1.0) == pytest.approx(3.14)
        assert cli.get_config_static_double("ns1", "d_int", -1.0) == 10.0
        # miss → default
        assert cli.get_config_static_double("ns1", "nope", 1.5) == 1.5
        assert cli.get_config_static_double("ns1", "nope") == 0.0
        # parse-fail → default
        assert cli.get_config_static_double("ns1", "d_bad", 9.9) == 9.9
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Static string (empty preserved — cache miss signal, not raw=="")
# ---------------------------------------------------------------------------


async def test_static_string_hit_miss_empty(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        assert cli.get_config_static_string("ns1", "s_hello", "def") == "hello"
        # A genuinely empty string value is a HIT, not a miss — preserved.
        assert cli.get_config_static_string("ns1", "s_empty", "def") == ""
        # miss → default
        assert cli.get_config_static_string("ns1", "nope", "def") == "def"
        assert cli.get_config_static_string("ns1", "nope") == ""
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Static json
# ---------------------------------------------------------------------------


async def test_static_json_hit_miss_parsefail(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        assert cli.get_config_static_json("ns1", "j_obj") == {"a": 1, "b": [2, 3]}
        assert cli.get_config_static_json("ns1", "j_arr") == [1, 2, 3]
        # miss → default
        assert cli.get_config_static_json("ns1", "nope") is None
        assert cli.get_config_static_json("ns1", "nope", {"d": 1}) == {"d": 1}
        # parse-fail → default
        assert cli.get_config_static_json("ns1", "j_bad", {"fallback": True}) == {
            "fallback": True
        }
        # empty-string static hit → json.loads("") raises → default
        assert cli.get_config_static_json("ns1", "s_empty", {"e": 1}) == {"e": 1}
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Dynamic bool
# ---------------------------------------------------------------------------


async def test_dynamic_bool_hit_miss_empty(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        assert await cli.get_config_bool(ctx, "ns1", "b_true", False) is True
        assert await cli.get_config_bool(ctx, "ns1", "b_one", False) is True
        assert await cli.get_config_bool(ctx, "ns1", "b_false", True) is False
        assert await cli.get_config_bool(ctx, "ns1", "b_garbage", True) is False
        # miss → default
        assert await cli.get_config_bool(ctx, "ns1", "nope", True) is True
        # empty resolved value → treated as miss → default
        assert await cli.get_config_bool(ctx, "ns1", "s_empty", True) is True
    finally:
        await cli.aclose()


async def test_dynamic_bool_abtest_hit(cfg_servicer, ab_servicer, running_servers):
    # b_true's ab version (2) resolves to "false"; prove the dynamic accessor
    # parses the ABTEST-resolved value, not the full release.
    ab_servicer.set_response("ns1", make_exp_result({"b_true": 2}))
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1", {"country": "US"})
        assert await cli.get_config_bool(ctx, "ns1", "b_true", True) is False
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Dynamic long
# ---------------------------------------------------------------------------


async def test_dynamic_long_hit_miss_empty_parsefail(
    cfg_servicer, ab_servicer, running_servers
):
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        assert await cli.get_config_long(ctx, "ns1", "l_42", -1) == 42
        assert await cli.get_config_long(ctx, "ns1", "l_neg", -1) == -7
        # miss → default
        assert await cli.get_config_long(ctx, "ns1", "nope", 77) == 77
        # empty → default
        assert await cli.get_config_long(ctx, "ns1", "s_empty", 77) == 77
        # parse-fail → default
        assert await cli.get_config_long(ctx, "ns1", "l_bad", 5) == 5
        assert await cli.get_config_long(ctx, "ns1", "l_float", 5) == 5
    finally:
        await cli.aclose()


async def test_dynamic_long_precision_lossless(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        assert await cli.get_config_long(ctx, "ns1", "l_big53", 0) == 9007199254740993
        assert (
            await cli.get_config_long(ctx, "ns1", "l_huge", 0)
            == 123456789012345678901234567890
        )
    finally:
        await cli.aclose()


async def test_dynamic_long_abtest_hit(cfg_servicer, ab_servicer, running_servers):
    ab_servicer.set_response("ns1", make_exp_result({"l_42": 2}))
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1", {"country": "US"})
        assert await cli.get_config_long(ctx, "ns1", "l_42", -1) == 999
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Dynamic double
# ---------------------------------------------------------------------------


async def test_dynamic_double_hit_miss_empty_parsefail(
    cfg_servicer, ab_servicer, running_servers
):
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        assert await cli.get_config_double(ctx, "ns1", "d_pi", -1.0) == pytest.approx(3.14)
        assert await cli.get_config_double(ctx, "ns1", "d_int", -1.0) == 10.0
        # miss → default
        assert await cli.get_config_double(ctx, "ns1", "nope", 1.5) == 1.5
        # empty → default
        assert await cli.get_config_double(ctx, "ns1", "s_empty", 1.5) == 1.5
        # parse-fail → default
        assert await cli.get_config_double(ctx, "ns1", "d_bad", 9.9) == 9.9
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Dynamic string (empty → default; the dynamic path can't distinguish)
# ---------------------------------------------------------------------------


async def test_dynamic_string_hit_miss_empty(cfg_servicer, ab_servicer, running_servers):
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        assert await cli.get_config_string(ctx, "ns1", "s_hello", "def") == "hello"
        # miss → default
        assert await cli.get_config_string(ctx, "ns1", "nope", "def") == "def"
        # empty resolved value → treated as a miss on the dynamic path → default
        assert await cli.get_config_string(ctx, "ns1", "s_empty", "def") == "def"
    finally:
        await cli.aclose()


async def test_dynamic_string_abtest_hit(cfg_servicer, ab_servicer, running_servers):
    ab_servicer.set_response("ns1", make_exp_result({"s_hello": 2}))
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1", {"country": "US"})
        assert await cli.get_config_string(ctx, "ns1", "s_hello", "def") == "ab-hello"
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Dynamic json
# ---------------------------------------------------------------------------


async def test_dynamic_json_hit_miss_empty_parsefail(
    cfg_servicer, ab_servicer, running_servers
):
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        assert await cli.get_config_json(ctx, "ns1", "j_obj") == {"a": 1, "b": [2, 3]}
        assert await cli.get_config_json(ctx, "ns1", "j_arr") == [1, 2, 3]
        # miss → default
        assert await cli.get_config_json(ctx, "ns1", "nope") is None
        assert await cli.get_config_json(ctx, "ns1", "nope", {"d": 1}) == {"d": 1}
        # empty → default
        assert await cli.get_config_json(ctx, "ns1", "s_empty", {"d": 2}) == {"d": 2}
        # parse-fail → default
        assert await cli.get_config_json(ctx, "ns1", "j_bad", {"fallback": True}) == {
            "fallback": True
        }
    finally:
        await cli.aclose()


async def test_dynamic_json_abtest_hit(cfg_servicer, ab_servicer, running_servers):
    ab_servicer.set_response("ns1", make_exp_result({"j_obj": 2}))
    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1", {"country": "US"})
        assert await cli.get_config_json(ctx, "ns1", "j_obj") == {"ab": True}
    finally:
        await cli.aclose()


# ---------------------------------------------------------------------------
# Underlying exceptions propagate (NOT swallowed as a parse default).
# ---------------------------------------------------------------------------


async def test_dynamic_typed_propagates_sdk_closed(
    cfg_servicer, ab_servicer, running_servers
):
    from tipsy_ab_config.exceptions import SDKClosed

    cli = await _client(cfg_servicer, running_servers)
    ctx = cli.new_abtest_context("u1")
    await cli.aclose()
    # A closed client raises SDKClosed from get_config; the typed wrapper must
    # let it propagate rather than returning the parse default.
    with pytest.raises(SDKClosed):
        await cli.get_config_bool(ctx, "ns1", "b_true", False)
    with pytest.raises(SDKClosed):
        await cli.get_config_json(ctx, "ns1", "j_obj")


async def test_dynamic_typed_propagates_namespace_not_subscribed(
    cfg_servicer, ab_servicer, running_servers
):
    from tipsy_ab_config.exceptions import NamespaceNotSubscribed

    cli = await _client(cfg_servicer, running_servers)
    try:
        ctx = cli.new_abtest_context("u1")
        with pytest.raises(NamespaceNotSubscribed):
            await cli.get_config_long(ctx, "other-ns", "l_42", -1)
    finally:
        await cli.aclose()
