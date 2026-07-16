"""Heartbeat oneof handling tests for ``Client._handle_event`` (ST4).

The server may emit a ``ConfigUpdateEvent`` whose ``payload`` oneof is a
``Heartbeat`` (an idle-keepalive DATA frame with no config payload — see
config.proto). The SDK MUST ignore it completely:

- no cache mutation / no seq advance,
- ``subscribe_event_received_total`` (the per-snapshot counter) does NOT move,
- business/experiment seq-change counters do NOT move,
- no exception is raised.

A snapshot arriving before/after a heartbeat must still apply normally
(regression guard against a switch that swallows real snapshots).

``_handle_event`` only touches ``self._metrics`` and ``self._cache`` and is
synchronous, so we build a bare ``Client`` directly (no gRPC / event loop):
its ``__init__`` performs no I/O.
"""

from __future__ import annotations

from tipsy_ab_config.cache import ConfigCache
from tipsy_ab_config.client import Client, Config
from tipsy_ab_config.metrics import Metrics
from tipsy_ab_config._proto.tipsy.config.v1 import config_pb2

from .conftest import make_snapshot


def _make_client(namespaces=("ns1",)) -> Client:
    """A minimal Client wired only with a real cache + metrics.

    Transports / channels / auth are all ``None`` because ``_handle_event``
    never reaches them. ``__init__`` does no network I/O.
    """
    return Client(
        cfg=Config(namespaces=list(namespaces)),
        cache=ConfigCache(),
        metrics=Metrics(),
        config_transport=None,
        abtest_transport=None,
        auth_plugin=None,
    )


def _snapshot_event(snap) -> config_pb2.ConfigUpdateEvent:
    return config_pb2.ConfigUpdateEvent(snapshot=snap)


def _heartbeat_event(unix_nanos: int = 123) -> config_pb2.ConfigUpdateEvent:
    ev = config_pb2.ConfigUpdateEvent(
        heartbeat=config_pb2.Heartbeat(unix_nanos=unix_nanos)
    )
    # Sanity: the payload oneof really is the heartbeat leg, so
    # ``WhichOneof("payload")`` in _handle_event routes into the no-op branch.
    assert ev.WhichOneof("payload") == "heartbeat"
    return ev


def test_heartbeat_is_pure_noop_on_empty_cache() -> None:
    cli = _make_client()

    cli._handle_event(_heartbeat_event())  # must not raise

    # Nothing was ever applied: the namespace stays uncached, so known_seqs
    # reports the (0, 0) zero-pair and no per-snapshot metric moved.
    assert cli.cache.snapshot("ns1") is None
    assert cli.cache.known_seqs(["ns1"]) == {"ns1": (0, 0)}
    assert cli.metrics.subscribe_event_received_total("ns1") == 0
    assert cli.metrics.business_seq_change_total("ns1") == 0
    assert cli.metrics.experiment_seq_change_total("ns1") == 0


def test_heartbeat_does_not_disturb_existing_cached_snapshot() -> None:
    cli = _make_client()

    # Seed the cache with a real snapshot via the snapshot leg first.
    cli._handle_event(
        _snapshot_event(make_snapshot("ns1", 5, 7, {"k": (2, {2: "v2"})}))
    )
    baseline_events = cli.metrics.subscribe_event_received_total("ns1")
    baseline_biz = cli.metrics.business_seq_change_total("ns1")
    baseline_exp = cli.metrics.experiment_seq_change_total("ns1")
    baseline_seqs = cli.cache.known_seqs(["ns1"])
    baseline_full = cli.cache.full_release_version("ns1", "k")
    assert baseline_seqs == {"ns1": (5, 7)}
    assert baseline_full == 2

    # A flurry of heartbeats must leave every observable untouched.
    for _ in range(3):
        cli._handle_event(_heartbeat_event())

    assert cli.cache.known_seqs(["ns1"]) == baseline_seqs
    assert cli.cache.full_release_version("ns1", "k") == baseline_full
    assert cli.cache.value_of("ns1", "k", 2) == "v2"
    # The per-snapshot counter counts snapshots only — heartbeats are not events.
    assert cli.metrics.subscribe_event_received_total("ns1") == baseline_events
    assert cli.metrics.business_seq_change_total("ns1") == baseline_biz
    assert cli.metrics.experiment_seq_change_total("ns1") == baseline_exp


def test_snapshot_still_applies_when_interleaved_with_heartbeats() -> None:
    """Regression: heartbeat handling must not swallow real snapshots."""
    cli = _make_client()

    # heartbeat, snapshot(seq 5/5), heartbeat, snapshot(seq 6/6), heartbeat
    cli._handle_event(_heartbeat_event())
    cli._handle_event(
        _snapshot_event(make_snapshot("ns1", 5, 5, {"k": (1, {1: "a"})}))
    )
    cli._handle_event(_heartbeat_event())
    cli._handle_event(
        _snapshot_event(make_snapshot("ns1", 6, 6, {"k": (2, {2: "b"})}))
    )
    cli._handle_event(_heartbeat_event())

    # Cache reflects the LATEST snapshot; both snapshots advanced the seqs.
    assert cli.cache.known_seqs(["ns1"]) == {"ns1": (6, 6)}
    assert cli.cache.full_release_version("ns1", "k") == 2
    assert cli.cache.value_of("ns1", "k", 2) == "b"
    # Exactly two snapshots were counted; the three heartbeats contributed 0.
    assert cli.metrics.subscribe_event_received_total("ns1") == 2
    assert cli.metrics.business_seq_change_total("ns1") == 2
    assert cli.metrics.experiment_seq_change_total("ns1") == 2
