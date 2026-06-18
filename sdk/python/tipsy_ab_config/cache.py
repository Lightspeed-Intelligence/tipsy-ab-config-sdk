"""Local NamespaceSnapshot cache with dual snapshot_seq semantics.

Mirrors the Go ``configCache`` exactly:

- Per-namespace immutable ``NamespaceSnapshot`` value objects.
- ``apply`` replaces the cached snapshot iff EITHER ``business_snapshot_seq``
  or ``experiment_snapshot_seq`` strictly advances (including the 0 → 1
  bootstrap case).
- Single ``threading.Lock`` guards the namespace map; readers grab the
  snapshot pointer lock-free and treat it as immutable.
- ``known_seqs`` builds the per-ns pair the SDK sends with each PullAll /
  Subscribe.

This module is import-light: no asyncio, no grpcio. It can be exercised from
synchronous code paths (Flask sync) the same way the Go side can be called
from any goroutine.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from threading import Lock
from typing import Dict, Iterable, List, Mapping, Optional, Tuple


@dataclass(frozen=True)
class KeyState:
    """SDK-local view of one config key.

    ``full_release_version`` is ``None`` when the key has no active full
    release; ``versions`` maps version_id → value (full release version + any
    versions abtest has flagged as potentially applicable).
    """

    full_release_version: Optional[int]
    versions: Mapping[int, str]


@dataclass(frozen=True)
class NamespaceSnapshot:
    """SDK-local view of one NamespaceSnapshot.

    Both seqs MUST be set; ``experiment_snapshot_seq == 0`` is the documented
    degraded marker (abtest unavailable, config fell back to "full-release
    only").
    """

    namespace: str
    business_snapshot_seq: int
    experiment_snapshot_seq: int
    keys: Mapping[str, KeyState]
    cached_bytes: int = 0


@dataclass
class ApplyResult:
    """Diagnostic record returned by ``ConfigCache.apply``."""

    replaced: bool = False
    business_moved: bool = False
    experiment_moved: bool = False


class ConfigCache:
    """Per-process namespace cache."""

    def __init__(self) -> None:
        self._lock = Lock()
        self._by_ns: Dict[str, NamespaceSnapshot] = {}

    # ---- public reads ----

    def snapshot(self, namespace: str) -> Optional[NamespaceSnapshot]:
        with self._lock:
            return self._by_ns.get(namespace)

    def full_release_version(self, namespace: str, key: str) -> Optional[int]:
        """Return the active full-release version_id for ``(ns, key)``.

        ``None`` on a cache miss (no snapshot, no such key, or the key has no
        full release set).  Callers MUST gate on ``is None``.
        """
        snap = self.snapshot(namespace)
        if snap is None:
            return None
        ks = snap.keys.get(key)
        if ks is None or ks.full_release_version is None:
            return None
        return ks.full_release_version

    def value_of(
        self, namespace: str, key: str, version_id: int
    ) -> Optional[str]:
        """Return the cached value for ``(ns, key, version_id)``.

        ``None`` on a cache miss.  Empty string / "[]" / "{}" are all valid
        values; callers MUST use ``is None`` to test for "no value" per
        design §10.5.
        """
        snap = self.snapshot(namespace)
        if snap is None:
            return None
        ks = snap.keys.get(key)
        if ks is None:
            return None
        return ks.versions.get(version_id)

    def list_namespaces(self) -> List[str]:
        with self._lock:
            return sorted(self._by_ns.keys())

    def known_seqs(
        self, namespaces: Iterable[str]
    ) -> Dict[str, Tuple[int, int]]:
        """Return per-ns (business_seq, experiment_seq) for the listed ns.

        Namespaces not yet cached are returned as (0, 0) so the server treats
        them as "send everything" per backend §4.1.2.
        """
        out: Dict[str, Tuple[int, int]] = {}
        with self._lock:
            for ns in namespaces:
                snap = self._by_ns.get(ns)
                if snap is None:
                    out[ns] = (0, 0)
                else:
                    out[ns] = (
                        snap.business_snapshot_seq,
                        snap.experiment_snapshot_seq,
                    )
        return out

    # ---- mutate ----

    def apply(self, pb_snapshot) -> ApplyResult:
        """Apply a protobuf NamespaceSnapshot.

        Replaces the cached snapshot iff either snapshot_seq is strictly
        greater than the cached value.  Returns an ``ApplyResult`` that
        records which seq moved so the caller can fire metrics.
        """
        result = ApplyResult()
        if pb_snapshot is None or not getattr(pb_snapshot, "namespace", ""):
            return result
        ns = pb_snapshot.namespace
        new_biz = int(pb_snapshot.business_snapshot_seq)
        new_exp = int(pb_snapshot.experiment_snapshot_seq)

        with self._lock:
            current = self._by_ns.get(ns)
            cur_biz = current.business_snapshot_seq if current else 0
            cur_exp = current.experiment_snapshot_seq if current else 0
            result.business_moved = new_biz > cur_biz
            result.experiment_moved = new_exp > cur_exp
            if current is not None and not (
                result.business_moved or result.experiment_moved
            ):
                return result

            keys: Dict[str, KeyState] = {}
            byte_size = 0
            for k in pb_snapshot.keys:
                if not k.key:
                    continue
                # `optional int64 full_release_version` — use HasField to
                # discriminate "unset" from "id = 0".
                if k.HasField("full_release_version"):
                    full_v: Optional[int] = int(k.full_release_version)
                else:
                    full_v = None
                versions: Dict[int, str] = {}
                for vid, val in k.versions.items():
                    versions[int(vid)] = val
                    byte_size += len(val)
                byte_size += len(k.key)
                keys[k.key] = KeyState(
                    full_release_version=full_v, versions=versions
                )
            self._by_ns[ns] = NamespaceSnapshot(
                namespace=ns,
                business_snapshot_seq=new_biz,
                experiment_snapshot_seq=new_exp,
                keys=keys,
                cached_bytes=byte_size,
            )
            result.replaced = True
        return result
