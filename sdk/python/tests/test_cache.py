"""Dual snapshot_seq tests for the Python ConfigCache."""

from __future__ import annotations

import threading

from tipsy_ab_config.cache import ConfigCache

from .conftest import make_snapshot


def test_first_apply_replaces_even_at_zero_seqs() -> None:
    c = ConfigCache()
    snap = make_snapshot("ns1", 0, 0, {"k": (None, {1: "v"})})
    r = c.apply(snap)
    # cur is None, so any apply replaces; biz/exp didn't strictly advance
    # (both stay at 0), but the cache must still install the snapshot.
    assert r.replaced is True


def test_same_seqs_not_replaced() -> None:
    c = ConfigCache()
    first = make_snapshot("ns1", 1, 1, {"k": (10, {10: "v10"})})
    c.apply(first)
    again = make_snapshot("ns1", 1, 1, {"k": (20, {20: "x"})})
    r = c.apply(again)
    assert r.replaced is False
    assert r.business_moved is False
    assert r.experiment_moved is False
    # The old version remains.
    assert c.full_release_version("ns1", "k") == 10


def test_business_only_advances() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("ns1", 1, 5, {"k": (10, {10: "a"})}))
    r = c.apply(make_snapshot("ns1", 2, 5, {"k": (20, {20: "b"})}))
    assert r.replaced is True
    assert r.business_moved is True
    assert r.experiment_moved is False
    assert c.full_release_version("ns1", "k") == 20


def test_experiment_only_advances() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("ns1", 1, 1, {"k": (1, {1: "a"})}))
    r = c.apply(make_snapshot("ns1", 1, 9, {"k": (2, {2: "b"})}))
    assert r.replaced is True
    assert r.business_moved is False
    assert r.experiment_moved is True


def test_reject_older_seqs() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("ns1", 10, 10, {"k": (1, {1: "v"})}))
    r = c.apply(make_snapshot("ns1", 5, 5, {"k": (2, {2: "x"})}))
    assert r.replaced is False
    assert c.full_release_version("ns1", "k") == 1


def test_full_release_unset_vs_zero() -> None:
    c = ConfigCache()
    # Key without full_release_version set -> None.
    snap = make_snapshot("ns1", 1, 1, {"noFull": (None, {42: "v42"})})
    c.apply(snap)
    assert c.full_release_version("ns1", "noFull") is None
    # value_of still resolves the version.
    assert c.value_of("ns1", "noFull", 42) == "v42"


def test_empty_value_is_valid() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("ns1", 1, 1, {"k": (1, {1: ""})}))
    # Empty string is a valid cached value — must NOT be reported as miss.
    assert c.value_of("ns1", "k", 1) == ""
    assert c.value_of("ns1", "k", 999) is None  # genuine miss


def test_missing_namespace_returns_none() -> None:
    c = ConfigCache()
    assert c.snapshot("missing") is None
    assert c.full_release_version("missing", "k") is None
    assert c.value_of("missing", "k", 1) is None


def test_known_seqs_includes_uncached_as_zero_pair() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("nsA", 7, 9))
    seqs = c.known_seqs(["nsA", "nsB"])
    assert seqs["nsA"] == (7, 9)
    assert seqs["nsB"] == (0, 0)


def test_list_namespaces_sorted() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("zzz", 1, 1))
    c.apply(make_snapshot("aaa", 1, 1))
    c.apply(make_snapshot("mmm", 1, 1))
    assert c.list_namespaces() == ["aaa", "mmm", "zzz"]


def test_concurrent_reads_during_replace() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))

    stop = threading.Event()

    def reader():
        while not stop.is_set():
            c.snapshot("ns1")
            c.full_release_version("ns1", "k")

    threads = [threading.Thread(target=reader) for _ in range(4)]
    for t in threads:
        t.start()

    for i in range(2, 200):
        c.apply(make_snapshot("ns1", i, i, {"k": (i, {i: "v"})}))

    stop.set()
    for t in threads:
        t.join()


def test_nil_snapshot_ignored() -> None:
    c = ConfigCache()
    r = c.apply(None)
    assert r.replaced is False


def test_empty_namespace_ignored() -> None:
    from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2

    c = ConfigCache()
    r = c.apply(config_pb2.NamespaceSnapshot(namespace=""))
    assert r.replaced is False


def test_zero_to_one_triggers_business_moved() -> None:
    c = ConfigCache()
    c.apply(make_snapshot("ns1", 0, 0))
    r = c.apply(make_snapshot("ns1", 1, 0))
    assert r.replaced is True
    assert r.business_moved is True
    assert r.experiment_moved is False


# ---- has_dynamic_resolution accessor (ST4) ----


def test_hdr_missing_namespace_returns_false_false() -> None:
    c = ConfigCache()
    # No snapshot at all -> (False, False).
    assert c.has_dynamic_resolution("missing", "k") == (False, False)


def test_hdr_missing_key_returns_false_false() -> None:
    c = ConfigCache()
    c.apply(
        make_snapshot(
            "ns1", 1, 1,
            {"k": (1, {1: "v"})},
            has_dynamic_resolution={"k": False},
        )
    )
    # ns exists but the requested key does not -> (False, False).
    assert c.has_dynamic_resolution("ns1", "absent-key") == (False, False)


def test_hdr_absent_field_returns_false_false() -> None:
    c = ConfigCache()
    # Field left UNSET on the wire (old server): present must be False so the
    # caller keeps the always-wait path. value defaults to False but the
    # (value, present) contract is what matters.
    c.apply(make_snapshot("ns1", 1, 1, {"k": (1, {1: "v"})}))
    assert c.has_dynamic_resolution("ns1", "k") == (False, False)


def test_hdr_explicit_false_returns_false_true() -> None:
    c = ConfigCache()
    c.apply(
        make_snapshot(
            "ns1", 1, 1,
            {"k": (1, {1: "v"})},
            has_dynamic_resolution={"k": False},
        )
    )
    # Explicit False -> (False, True): present True unlocks the fast path.
    assert c.has_dynamic_resolution("ns1", "k") == (False, True)


def test_hdr_explicit_true_returns_true_true() -> None:
    c = ConfigCache()
    c.apply(
        make_snapshot(
            "ns1", 1, 1,
            {"k": (1, {1: "v"})},
            has_dynamic_resolution={"k": True},
        )
    )
    assert c.has_dynamic_resolution("ns1", "k") == (True, True)


def test_apply_preserves_none_vs_false_vs_true() -> None:
    """`apply` must round-trip the three proto presence states distinctly."""
    c = ConfigCache()
    c.apply(
        make_snapshot(
            "ns1", 1, 1,
            {
                "absentK": (1, {1: "a"}),
                "falseK": (2, {2: "b"}),
                "trueK": (3, {3: "c"}),
            },
            has_dynamic_resolution={
                # absentK intentionally omitted -> field UNSET (None).
                "falseK": False,
                "trueK": True,
            },
        )
    )
    snap = c.snapshot("ns1")
    assert snap is not None
    # None ⇒ field absent (old server). MUST be None, not False.
    assert snap.keys["absentK"].has_dynamic_resolution is None
    assert snap.keys["falseK"].has_dynamic_resolution is False
    assert snap.keys["trueK"].has_dynamic_resolution is True

    # And the accessor's (value, present) view mirrors that:
    assert c.has_dynamic_resolution("ns1", "absentK") == (False, False)
    assert c.has_dynamic_resolution("ns1", "falseK") == (False, True)
    assert c.has_dynamic_resolution("ns1", "trueK") == (True, True)
