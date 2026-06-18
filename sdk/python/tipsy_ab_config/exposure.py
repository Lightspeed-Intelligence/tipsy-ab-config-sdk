"""Asynchronous exposure emitter with 5-minute per-process dedup.

Mirrors ``sdk/go/tipsyabconfig/exposure.go``. The Python implementation is
asyncio-native: one background task drains a bounded ``asyncio.Queue`` and
hands events to the registered :class:`ExposureSink`. The dedup map is
``dict[str, float]`` guarded by the queue's coroutine loop (no thread lock —
exposures only enter from coroutine code paths reachable from
``get_config``).
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field
from typing import Awaitable, Callable, Iterable, List, Optional, Protocol

logger = logging.getLogger("tipsy_ab_config.exposure")


@dataclass
class ExposureEvent:
    """One (uid, ns, key, version) hit attributed to an abtest path."""

    user_id: str
    namespace: str
    key: str
    version: int
    source: str = ""  # "experiment_group" | "whitelist"
    experiment_id: str = ""  # v2: TEXT ids; "" when source = whitelist
    group_id: str = ""  # v2: TEXT ids; "" when source = whitelist
    experiment_status: str = ""
    release_id: int = 0  # gray release_whitelist.id stays int64
    emitted_at: float = field(default_factory=time.time)


class ExposureSink(Protocol):
    """Sink receives one event per exposure.

    Implementations may be async (``await sink(ev)`` is awaited) — the
    emitter inspects the return type and awaits when needed.
    """

    def __call__(self, ev: ExposureEvent) -> Optional[Awaitable[None]]:
        ...


def _default_sink(ev: ExposureEvent) -> None:
    logger.info(
        "exposure",
        extra={
            "uid": ev.user_id,
            "ns": ev.namespace,
            "key": ev.key,
            "version": ev.version,
            "source": ev.source,
            "experiment_id": ev.experiment_id,
            "group_id": ev.group_id,
            "release_id": ev.release_id,
        },
    )


DEFAULT_QUEUE_SIZE = 4096
DEFAULT_DEDUP_MAX_ENTRIES = 16384


class ExposureEmitter:
    """Async fire-and-forget exposure pipeline.

    Use:

        emitter = ExposureEmitter(sink, ttl_seconds=300)
        await emitter.start()              # spawns background task
        emitter.emit(uid, ns, key, ver, ab_exposures)
        ...
        await emitter.aclose()             # drains queue, exits task
    """

    def __init__(
        self,
        sink: Optional[ExposureSink] = None,
        ttl_seconds: float = 300.0,
        queue_size: int = DEFAULT_QUEUE_SIZE,
        dedup_max_entries: int = DEFAULT_DEDUP_MAX_ENTRIES,
        loop: Optional[asyncio.AbstractEventLoop] = None,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self._sink: ExposureSink = sink or _default_sink
        self._ttl = ttl_seconds
        self._queue: asyncio.Queue[Optional[ExposureEvent]] = asyncio.Queue(
            maxsize=queue_size
        )
        self._dedup: dict[str, float] = {}
        self._dedup_max = dedup_max_entries
        self._loop = loop
        self._clock = clock
        self._task: Optional[asyncio.Task[None]] = None
        self._closed = False

    async def start(self) -> None:
        if self._task is not None:
            return
        self._loop = self._loop or asyncio.get_running_loop()
        self._task = self._loop.create_task(self._run())

    def emit(
        self,
        user_id: str,
        namespace: str,
        key: str,
        version: int,
        ab_exposures: Iterable,
    ) -> None:
        """Non-blocking emit. Drops the event on a full queue."""
        if self._closed:
            return
        matched = _pick_exposure_for_key(key, version, ab_exposures)
        if matched is None:
            # No source attribution from Compute (shouldn't happen for an
            # abtest hit, but stay defensive). Default to experiment_group
            # with zero ids so downstream still records the resolve.
            ev = ExposureEvent(
                user_id=user_id,
                namespace=namespace,
                key=key,
                version=version,
                source="experiment_group",
                emitted_at=self._clock(),
            )
        else:
            ev = ExposureEvent(
                user_id=user_id,
                namespace=namespace,
                key=key,
                version=version,
                source=matched.source,
                experiment_id=matched.experiment_id if matched.HasField("experiment_id") else "",
                group_id=matched.group_id if matched.HasField("group_id") else "",
                experiment_status=matched.experiment_status if matched.HasField("experiment_status") else "",
                release_id=int(matched.release_id) if matched.HasField("release_id") else 0,
                emitted_at=self._clock(),
            )
        if not self._should_emit(user_id, key, version):
            return
        try:
            self._queue.put_nowait(ev)
        except asyncio.QueueFull:
            logger.warning(
                "tipsy_ab_config: exposure queue full; dropping event ns=%s key=%s version=%d",
                namespace,
                key,
                version,
            )

    def _should_emit(self, uid: str, key: str, version: int) -> bool:
        now = self._clock()
        k = f"{uid}\x1f{key}\x1f{version}"
        last = self._dedup.get(k)
        if last is not None and (now - last) < self._ttl:
            return False
        if len(self._dedup) >= self._dedup_max:
            # Opportunistic GC.
            stale = [ek for ek, t in self._dedup.items() if (now - t) >= self._ttl]
            for ek in stale:
                del self._dedup[ek]
        self._dedup[k] = now
        return True

    async def _run(self) -> None:
        while True:
            ev = await self._queue.get()
            if ev is None:
                return
            try:
                result = self._sink(ev)
                if asyncio.iscoroutine(result):
                    await result
            except Exception:  # noqa: BLE001
                logger.exception("tipsy_ab_config: exposure sink raised")

    async def aclose(self) -> None:
        """Signal the worker to drain and exit, then await it."""
        if self._closed:
            return
        self._closed = True
        # Sentinel.
        try:
            self._queue.put_nowait(None)
        except asyncio.QueueFull:
            # Best-effort: if full, the worker is far behind. Cancel.
            if self._task is not None:
                self._task.cancel()
        if self._task is not None:
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass


def _pick_exposure_for_key(key: str, version: int, exposures: Iterable):
    for ex in exposures:
        if ex is None:
            continue
        if ex.key == key and int(ex.version) == int(version):
            return ex
    return None
