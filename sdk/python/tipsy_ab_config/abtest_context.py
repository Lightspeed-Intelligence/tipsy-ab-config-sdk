"""Per-request AbtestContext for the Python SDK.

Mirrors the Go ``AbtestContext`` semantics (design 04 §B.2/§B.3/§B.4),
adapted for asyncio:

- :meth:`Client.new_abtest_context` is a synchronous factory that purely
  CREATES the context: construction issues NO ``GetExperimentResult`` RPC.
  Every namespace is fetched lazily on first dynamic ``get_config`` for that
  ns (or eagerly via the explicit
  :meth:`AbtestContext.prefetch_config_version_flat_kv_for_namespace` API)
  and memoised into ``results`` so the whole request link issues AT MOST ONE
  ``GetExperimentResult`` RPC per namespace.
- Concurrency: business code may fan out multiple ``asyncio.Task``s within
  one request that share the same AbtestContext (the contract is
  "AbtestContext is safe for concurrent use by all tasks participating in
  the same request"). Per-ns first-access is deduplicated via a shared
  ``_NsResult`` slot created by the synchronous :meth:`_ensure_fetch` helper:
  the first accessor of a not-yet-fetched ns creates the slot + spawns the
  single RPC task, every other accessor awaits the SAME task. The slot-
  creation critical section crosses no ``await`` (single-threaded event loop),
  so it needs no lock. Net effect mirrors Go's ``computeStatus`` (done
  channel).
- :meth:`Client.empty_abtest_context` returns an identity-less ctx whose
  every not-yet-resolved ns short-circuits to the empty result (no RPC).

We expose ``abtest_ctx_var`` (a :class:`contextvars.ContextVar`) so the
FastAPI / Starlette middleware can stash the ctx for the request lifetime,
and ``Client.get_config`` retrieves it via ``contextvars`` when the caller
didn't pass one explicitly.
"""

from __future__ import annotations

import asyncio
import logging
import uuid
from dataclasses import dataclass, field
from typing import Any, Dict, Mapping, Optional, TYPE_CHECKING

import contextvars

from .exceptions import AbtestContextMissing

if TYPE_CHECKING:  # pragma: no cover
    from .client import Client


logger = logging.getLogger("tipsy_ab_config")


# ``abtest_ctx_var`` is the contextvar the FastAPI middleware writes to. Use
# ``token = abtest_ctx_var.set(ctx)`` then ``abtest_ctx_var.reset(token)`` in
# try/finally to clean up per design §3.4.
abtest_ctx_var: contextvars.ContextVar[Optional["AbtestContext"]] = (
    contextvars.ContextVar("tipsy_ab_config.abtest_ctx", default=None)
)


@dataclass
class _ComputeResult:
    """GetExperimentResult result the SDK keeps on the AbtestContext.

    ``key_versions`` maps config_key.name → version_id (from the
    ``config_flat_kv`` map).
    """

    key_versions: Dict[str, int] = field(default_factory=dict)


_EMPTY_RESULT = _ComputeResult()


@dataclass
class UserInfo:
    """SDK-stable view of the user identity carried by an AbtestContext.

    Business code retrieves it via ``ctx.user_info`` (design 04 §B.4).
    ``attrs`` aliases the constructor map (may be empty) and is read-only.
    Mirrors the Go ``UserInfo{UID, Attrs}`` struct.
    """

    uid: str = ""
    attrs: Mapping[str, Any] = field(default_factory=dict)


class _NsResult:
    """Shared result slot for one (request, namespace) pair.

    Mirrors Go's ``computeStatus``: ``task`` is the single in-flight (or
    completed) ``GetExperimentResult`` task for this ns within the request
    link, or ``None`` for a pre-resolved (mock/empty/unsubscribed) ns whose
    ``result`` is already set. Concurrent first-accessors share one
    ``_NsResult`` and await the same ``task``, so AT MOST ONE RPC is issued
    per ns per request.
    """

    __slots__ = ("task", "result")

    def __init__(
        self,
        task: "Optional[asyncio.Task[_ComputeResult]]" = None,
        result: Optional[_ComputeResult] = None,
    ) -> None:
        self.task = task
        self.result = result


class AbtestContext:
    """Per-request abtest result memo.

    Construct via :meth:`Client.new_abtest_context` (pure create — issues NO
    RPC), :meth:`Client.empty_abtest_context` (identity-less, no RPC), or
    :meth:`Client.mock_abtest_context` (test helper). ``AbtestContext``
    survives only the lifetime of a single inbound request and is bound to the
    issuing ``Client`` (which owns the local cache referenced during
    ``get_config``). To warm a namespace up front, call
    :meth:`prefetch_config_version_flat_kv_for_namespace` explicitly.
    """

    def __init__(
        self,
        user_id: str = "",
        user_attrs: Optional[Mapping[str, Any]] = None,
        owner: "Optional[Client]" = None,
        empty: bool = False,
        trace_id: Optional[str] = None,
    ) -> None:
        # Normalise a None uid to "" (parity with the Java constructor's
        # null → "" at AbtestContext.java:84). A None would otherwise bypass
        # the no-user shortcut in _ensure_fetch and break proto encoding
        # downstream (user_id must be a str on the wire).
        self.user_id = user_id if user_id is not None else ""
        self.user_attrs: Dict[str, Any] = dict(user_attrs or {})
        # trace_id is the request-scoped identifier shared by every RPC this
        # ctx issues (design 04 §B.2 + sdk-trace-id §5). Empty / None on input
        # ⇒ generate a fresh UUID v4 here so downstream call sites can always
        # rely on a non-empty value. We use ``str(uuid.uuid4())`` (36-char
        # with-dashes form) to match the Go side / the server's
        # ``uuid.New().String()``.
        self.trace_id: str = trace_id if trace_id else str(uuid.uuid4())
        # ns → _NsResult. Populated lazily on first dynamic get_config for a
        # ns (or eagerly via prefetch_config_version_flat_kv_for_namespace).
        self._results: Dict[str, _NsResult] = {}
        # owner is the Client that issued this ctx; used to fire the lazy
        # per-ns RPC and to check subscription.
        self._owner = owner
        # empty marks an identity-less / mock ctx: every not-yet-resolved ns
        # short-circuits to the empty result without issuing any RPC.
        self._empty = empty

    # ---- accessors ----

    @property
    def user_info(self) -> UserInfo:
        """Full user identity (uid + attrs) this ctx was constructed with.

        Mirrors Go's ``UserInfo()`` accessor (design 04 §B.4). ``attrs``
        aliases the constructor map and is read-only.
        """
        return UserInfo(uid=self.user_id, attrs=self.user_attrs)

    # ---- per-ns memoised result ----

    def _seed_result(self, ns: str, result: _ComputeResult) -> None:
        """Pre-resolve ``ns`` to ``result`` (mock / eager-sync helpers)."""
        self._results[ns] = _NsResult(result=result)

    def _ensure_fetch(self, ns: str) -> _NsResult:
        """Ensure ``ns`` is fetched (or short-circuited) exactly once.

        The single slot-creation primitive shared by :meth:`wait_for_abtest`
        and :meth:`prefetch_config_version_flat_kv_for_namespace`. Looks up the
        per-ns ``_NsResult`` slot; when absent it either short-circuits to the
        empty result (identity-less / mock / owner-less / unsubscribed ns, or a
        no-user uid ``""`` / ``"0"`` — NO RPC) or spawns the single
        ``GetExperimentResult`` task and memoises its slot. Idempotent: an
        already-present ns returns the existing slot without spawning a new
        task, preserving at-most-once.

        A pre-seeded slot (mock via :meth:`_seed_result`) always wins: the slot
        lookup precedes the short-circuit condition, so a
        ``MockAbtestContext(user_id="", …)`` still serves its seeded per-ns
        result rather than the empty short-circuit.

        Synchronous and lock-free: the lookup-then-create section crosses no
        ``await``, so on the single-threaded event loop it is atomic with
        respect to other tasks. Spawning the task requires a running event loop
        (``asyncio.ensure_future``).
        """
        slot = self._results.get(ns)
        if slot is None:
            no_user = self.user_id in ("", "0")
            if (
                self._empty
                or self._owner is None
                or no_user
                or not self._owner.is_subscribed(ns)
            ):
                # Identity-less / mock / no-user uid / unsubscribed ns: resolve
                # to empty without an RPC. A uid of "" or "0" carries no real
                # user identity, so it never participates in experiment /
                # whitelist logic — it short-circuits here (no abtestFallback
                # metric: this is a deliberate skip, not a degradation).
                # (Dynamic get_config rejects unsubscribed ns earlier via
                # resolve_namespace; this guards the low-level entry.)
                if no_user and not self._empty and self._owner is not None:
                    logger.debug(
                        "tipsy_ab_config: skip abtest: no-user uid",
                        extra={
                            "ns": ns,
                            "uid": self.user_id,
                            "trace_id": self.trace_id,
                        },
                    )
                slot = _NsResult(result=_EMPTY_RESULT)
            else:
                task = asyncio.ensure_future(
                    self._owner._fetch_config_version_flat_kv_for_ns(
                        ns, self.user_id, self.user_attrs, self.trace_id
                    )
                )
                slot = _NsResult(task=task)
            self._results[ns] = slot
        return slot

    def prefetch_config_version_flat_kv_for_namespace(self, ns: str) -> None:
        """Eagerly warm the ``config_version`` flat_kv result for ``ns``.

        Explicit opt-in prefetch: spawns the single per-ns
        ``GetExperimentResult`` (type=config_version, display=flat_kv) task in
        the background and memoises its slot, so a later ``get_config`` for
        ``ns`` reuses the SAME task (at-most-once). Returns immediately — it
        does NOT await the result.

        Idempotent: prefetching an already-fetched ns is a no-op. An empty /
        identity-less / mock ctx and an unsubscribed ns short-circuit inside
        :meth:`_ensure_fetch` without issuing any RPC.

        Must be called from within a running asyncio event loop (the spawn uses
        ``asyncio.ensure_future``).
        """
        self._ensure_fetch(ns)

    async def wait_for_abtest(self, namespace: str) -> _ComputeResult:
        """Return the memoised abtest result for ``namespace``.

        First access for a not-yet-fetched ns fetches it exactly once and
        memoises it (design 04 §B.3) via the shared :meth:`_ensure_fetch`
        primitive; the first accessor creates the slot + spawns the single RPC
        task, every other task that races on the same ns finds the existing
        slot and awaits the SAME task. Net effect: AT MOST ONE
        GetExperimentResult RPC per ns per request link.

        Returns the empty result (no hits) when the per-ns call failed —
        a single-ns failure degrades that ns silently so the awaiting
        ``get_config`` falls through to the full-release branch.
        """
        slot = self._ensure_fetch(namespace)
        if slot.result is not None:
            return slot.result
        assert slot.task is not None
        try:
            return await asyncio.shield(slot.task)
        except asyncio.CancelledError:
            raise
        except Exception:  # noqa: BLE001
            # Per-ns failure degrades silently (the RPC helper already logs +
            # bumps the fallback metric); the caller falls through to full.
            return _EMPTY_RESULT


def _ensure_ctx(ctx: Optional[AbtestContext]) -> AbtestContext:
    """Raise ``AbtestContextMissing`` for ``None``; return ``ctx`` otherwise."""
    if ctx is None:
        raise AbtestContextMissing("abtest context missing")
    return ctx
