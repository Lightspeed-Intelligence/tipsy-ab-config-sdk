"""Smoke tests for the Metrics counters."""

from __future__ import annotations

from tipsy_ab_config.metrics import Metrics


def test_metrics_initial_zero():
    m = Metrics()
    assert m.cache_empty_total() == 0
    assert m.pull_failure_total("ns") == 0
    assert m.subscribe_disconnect_total("ns") == 0
    assert m.subscribe_event_received_total("ns") == 0
    assert m.local_cache_bytes("ns") == 0
    assert m.abtest_fallback_total("ns") == 0
    assert m.business_seq_change_total("ns") == 0
    assert m.experiment_seq_change_total("ns") == 0


def test_metrics_increment_counters():
    m = Metrics()
    m.inc_cache_empty()
    m.inc_pull_failure("a")
    m.inc_pull_failure("a")
    m.inc_pull_failure("b")
    assert m.cache_empty_total() == 1
    assert m.pull_failure_total("a") == 2
    assert m.pull_failure_total("b") == 1
    assert m.pull_failure_total("never") == 0


def test_metrics_local_cache_bytes_gauge():
    m = Metrics()
    m.set_local_cache_bytes("ns", 1024)
    assert m.local_cache_bytes("ns") == 1024
    m.set_local_cache_bytes("ns", 2048)
    assert m.local_cache_bytes("ns") == 2048  # last-write-wins
