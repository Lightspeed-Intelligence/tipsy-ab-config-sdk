"""Per-process SDK counters.

Mirrors the Go ``Metrics`` struct; same six counters listed in
``config-platform-sdk.md`` §10.6 plus the per-ns local cache gauge.
Implementation uses a single ``threading.Lock`` over plain dicts — these
counters are cold path enough that fine-grained sharding is wasted effort.
"""

from __future__ import annotations

from collections import defaultdict
from threading import Lock
from typing import Dict


class Metrics:
    """Per-process SDK counters."""

    def __init__(self) -> None:
        self._lock = Lock()
        self._cache_empty: int = 0
        self._pull_failure: Dict[str, int] = defaultdict(int)
        self._subscribe_disc: Dict[str, int] = defaultdict(int)
        self._subscribe_event: Dict[str, int] = defaultdict(int)
        self._local_cache_bytes: Dict[str, int] = defaultdict(int)
        self._abtest_fallback: Dict[str, int] = defaultdict(int)
        self._business_seq: Dict[str, int] = defaultdict(int)
        self._experiment_seq: Dict[str, int] = defaultdict(int)

    # ---- record ----

    def inc_cache_empty(self) -> None:
        with self._lock:
            self._cache_empty += 1

    def inc_pull_failure(self, ns: str) -> None:
        with self._lock:
            self._pull_failure[ns] += 1

    def inc_subscribe_disconnect(self, ns: str) -> None:
        with self._lock:
            self._subscribe_disc[ns] += 1

    def inc_subscribe_event(self, ns: str) -> None:
        with self._lock:
            self._subscribe_event[ns] += 1

    def set_local_cache_bytes(self, ns: str, n: int) -> None:
        with self._lock:
            self._local_cache_bytes[ns] = n

    def inc_abtest_fallback(self, ns: str) -> None:
        with self._lock:
            self._abtest_fallback[ns] += 1

    def inc_business_seq_change(self, ns: str) -> None:
        with self._lock:
            self._business_seq[ns] += 1

    def inc_experiment_seq_change(self, ns: str) -> None:
        with self._lock:
            self._experiment_seq[ns] += 1

    # ---- read ----

    def cache_empty_total(self) -> int:
        with self._lock:
            return self._cache_empty

    def pull_failure_total(self, ns: str) -> int:
        with self._lock:
            return self._pull_failure.get(ns, 0)

    def subscribe_disconnect_total(self, ns: str) -> int:
        with self._lock:
            return self._subscribe_disc.get(ns, 0)

    def subscribe_event_received_total(self, ns: str) -> int:
        with self._lock:
            return self._subscribe_event.get(ns, 0)

    def local_cache_bytes(self, ns: str) -> int:
        with self._lock:
            return self._local_cache_bytes.get(ns, 0)

    def abtest_fallback_total(self, ns: str) -> int:
        with self._lock:
            return self._abtest_fallback.get(ns, 0)

    def business_seq_change_total(self, ns: str) -> int:
        with self._lock:
            return self._business_seq.get(ns, 0)

    def experiment_seq_change_total(self, ns: str) -> int:
        with self._lock:
            return self._experiment_seq.get(ns, 0)
