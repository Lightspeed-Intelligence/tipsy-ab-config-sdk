"""Per-request AbtestContext for the Python SDK.

Mirrors the Go ``AbtestContext`` semantics (design 04 §B.2/§B.3/§B.4),
adapted for asyncio:

- :meth:`Client.new_abtest_context` is a synchronous factory that eagerly
  pre-fetches AT MOST the prefetch namespace (the explicit construction ns,
  else the client ``default_namespace``) via one ``GetExperimentResult``
  ``asyncio.Task``. It does NOT fan out to every subscribed namespace.
- Other namespaces are fetched lazily on first dynamic ``get_config`` for
  that ns and memoised into ``results`` so the whole request link issues
  AT MOST ONE ``GetExperimentResult`` RPC per namespace.
- Concurrency: business code may fan out multiple ``asyncio.Task``s within
  one request that share the same AbtestContext (the contract is
  "AbtestContext is safe for concurrent use by all tasks participating in
  the same request"). Per-ns first-access is deduplicated via a shared
  ``_NsResult`` slot guarded by an ``asyncio.Lock``: the first accessor of a
  not-yet-fetched ns creates the slot + spawns the single RPC task, every
  other accessor awaits the SAME task. Net effect mirrors Go's
  ``computeStatus`` (done channel).
- :meth:`Client.empty_abtest_context` returns an identity-less ctx whose
  every not-yet-resolved ns short-circuits to the empty result (no RPC).

We expose ``abtest_ctx_var`` (a :class:`contextvars.ContextVar`) so the
FastAPI / Starlette middleware can stash the ctx for the request lifetime,
and ``Client.get_config`` retrieves it via ``contextvars`` when the caller
didn't pass one explicitly.
"""

from __future__ import annotations

import asyncio
import uuid
from dataclasses import dataclass, field
from typing import Any, Dict, Mapping, Optional, TYPE_CHECKING

import contextvars

from .exceptions import AbtestContextMissing

if TYPE_CHECKING:  # pragma: no cover
    from .client import Client


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
    link, or ``None`` for a pre-resolved (eager/mock/empty) ns whose
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

    Construct via :meth:`Client.new_abtest_context` (eager prefetch of the
    default/explicit ns only), :meth:`Client.empty_abtest_context`
    (identity-less, no RPC), or :meth:`Client.mock_abtest_context` (test
    helper). ``AbtestContext`` survives only the lifetime of a single inbound
    request and is bound to the issuing ``Client`` (which owns the local cache
    referenced during ``get_config``).
    """

    def __init__(
        self,
        user_id: str = "",
        user_attrs: Optional[Mapping[str, Any]] = None,
        owner: "Optional[Client]" = None,
        empty: bool = False,
        trace_id: Optional[str] = None,
    ) -> None:
        self.user_id = user_id
        self.user_attrs: Dict[str, Any] = dict(user_attrs or {})
        # trace_id is the request-scoped identifier shared by every RPC this
        # ctx issues (design 04 §B.2 + sdk-trace-id §5). Empty / None on input
        # ⇒ generate a fresh UUID v4 here so downstream call sites can always
        # rely on a non-empty value. We use ``str(uuid.uuid4())`` (36-char
        # with-dashes form) to match the Go side / the server's
        # ``uuid.New().String()``.
        self.trace_id: str = trace_id if trace_id else str(uuid.uuid4())
        # ns → _NsResult. Populated eagerly for the prefetch ns (if any) and
        # lazily on first dynamic get_config for any other ns.
        self._results: Dict[str, _NsResult] = {}
        # Guards _results mutation so concurrent first-access of the same ns
        # dedups to a single RPC task (mirrors Go's mutex + computeStatus).
        self._lock = asyncio.Lock()
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

    def _spawn_prefetch(self, ns: str) -> None:
        """Eagerly fetch ``ns`` in the background (NewAbtestContext path).

        Spawns the single per-ns RPC task and memoises its slot so the first
        dynamic get_config for ``ns`` reuses it (at-most-once). Must be called
        from within a running event loop.
        """
        assert self._owner is not None
        task = asyncio.ensure_future(
            self._owner._get_experiment_result_for_ns(
                ns, self.user_id, self.user_attrs, self.trace_id
            )
        )
        self._results[ns] = _NsResult(task=task)

    async def wait_for_abtest(self, namespace: str) -> _ComputeResult:
        """Return the memoised abtest result for ``namespace``.

        First access for a not-yet-fetched ns fetches it synchronously exactly
        once and memoises it (design 04 §B.3). Concurrency: under ``_lock`` we
        double-check the per-ns slot; the first task creates it + spawns the
        single RPC task, every other task that races on the same ns finds the
        existing slot and awaits the SAME task. Net effect: AT MOST ONE
        GetExperimentResult RPC per ns per request link.

        Returns the empty result (no hits) when the per-ns call failed —
        a single-ns failure degrades that ns silently so the awaiting
        ``get_config`` falls through to the full-release branch.
        """
        async with self._lock:
            slot = self._results.get(namespace)
            if slot is None:
                if (
                    self._empty
                    or self._owner is None
                    or not self._owner.is_subscribed(namespace)
                ):
                    # Identity-less / mock / unsubscribed ns: resolve to empty
                    # without an RPC. (Dynamic get_config rejects unsubscribed
                    # ns earlier via resolve_namespace; this guards the
                    # low-level entry.)
                    slot = _NsResult(result=_EMPTY_RESULT)
                else:
                    task = asyncio.ensure_future(
                        self._owner._get_experiment_result_for_ns(
                            namespace, self.user_id, self.user_attrs, self.trace_id
                        )
                    )
                    slot = _NsResult(task=task)
                self._results[namespace] = slot

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
