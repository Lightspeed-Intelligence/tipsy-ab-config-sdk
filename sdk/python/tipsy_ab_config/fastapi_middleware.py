"""ASGI / FastAPI middleware that injects an AbtestContext per request.

The middleware is framework-agnostic ASGI; FastAPI uses ``app.add_middleware``
to install it. It is also compatible with Starlette and any other ASGI
framework that supports the same middleware shape.

Usage::

    from tipsy_ab_config import Client
    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

    sdk: Client = ...

    async def user_provider(request) -> tuple[str, dict]:
        uid = request.headers.get("X-User-Id") or "anonymous"
        return uid, {"country": request.headers.get("X-Country", "")}

    app = FastAPI()
    app.add_middleware(AbtestMiddleware, sdk=sdk, user_provider=user_provider)

The middleware never auto-prefetches. To warm the default namespace for
specific entry-point routes, opt in with an exact-match URL whitelist::

    app.add_middleware(
        AbtestMiddleware, sdk=sdk, user_provider=user_provider,
        prefetch_paths=["/feed", "/recommend"],
    )

Construction issues no RPC; the first ``get_config`` for a namespace pays the
RPC latency unless that namespace was prefetched.

For Flask / Django sync setups, see ``Client.abtest_scope`` and design §10.4.1.

trace_id (sdk-trace-id §5 / user decision 3): the middleware reuses the
inbound ``X-Trace-Id`` header first, falls back to ``X-Request-Id``, and
otherwise generates a fresh UUID v4. The resolved id is forwarded onto every
GetExperimentResult RPC this request issues so the SDK, the server log line
and any downstream tracing share one identifier.
"""

from __future__ import annotations

import logging
import uuid
from typing import (
    Any,
    Awaitable,
    Callable,
    Optional,
    Sequence,
    Set,
    TYPE_CHECKING,
    Tuple,
)

from .abtest_context import abtest_ctx_var

logger = logging.getLogger("tipsy_ab_config.middleware")


if TYPE_CHECKING:  # pragma: no cover
    from .client import Client

    UserProvider = Callable[[Any], Awaitable[Tuple[str, dict]]]


# Header names checked, in order, when resolving a per-request trace_id.
# The first non-empty match wins; otherwise the middleware generates a fresh
# UUID v4 (user decision 3 in design.md).
_TRACE_HEADER_PRIMARY = "X-Trace-Id"
_TRACE_HEADER_SECONDARY = "X-Request-Id"


def _extract_trace_id(request) -> str:
    """Return the inbound trace_id, falling back to a fresh UUID v4.

    Mirrors the Go SDK's ``extractTraceFromRequest`` helper: prefer
    ``X-Trace-Id``, then ``X-Request-Id``, both case-insensitively (Starlette
    headers are already case-insensitive). Whitespace-only values are treated
    as absent so an upstream proxy that injects ``X-Trace-Id: `` does not pin
    the SDK to an empty string.
    """
    for header in (_TRACE_HEADER_PRIMARY, _TRACE_HEADER_SECONDARY):
        v = request.headers.get(header)
        if v is not None and v.strip():
            return v.strip()
    return str(uuid.uuid4())


class AbtestMiddleware:
    """ASGI middleware that seeds an AbtestContext per request.

    The middleware:

    1. resolves the request's ``trace_id`` (``X-Trace-Id`` > ``X-Request-Id``
       > generated UUID v4);
    2. invokes the async ``user_provider`` (returns ``(uid, attrs)``);
    3. builds the AbtestContext via :meth:`Client.new_abtest_context`
       (synchronous; pure create — NO RPC at construction), passing the
       resolved ``trace_id``; on provider error the ctx is empty but still
       carries the resolved id;
    4. OPTIONALLY warms the default namespace: when ``prefetch_paths`` is
       configured and the request path EXACTLY matches a whitelisted path
       (and the SDK has a default namespace), it calls
       :meth:`AbtestContext.prefetch_config_version_flat_kv_for_namespace`.
       An empty whitelist (the default), a non-matching path, or no default
       namespace ⇒ no prefetch (construction stays RPC-free; the first
       ``get_config`` pays the RPC latency);
    5. stashes the ctx on the :data:`abtest_ctx_var` ContextVar;
    6. calls downstream;
    7. resets the contextvar token in ``finally``.

    The middleware is ASGI 3 compatible; it works with FastAPI, Starlette,
    Quart, and Litestar without any additional adapter.
    """

    def __init__(
        self,
        app,
        sdk: "Client",
        user_provider: Optional[Callable[..., Awaitable[Tuple[str, dict]]]] = None,
        prefetch_paths: Optional[Sequence[str]] = None,
    ) -> None:
        self.app = app
        self.sdk = sdk
        self.user_provider = user_provider
        # Exact-match URL whitelist (design D4): only requests whose
        # ``request.url.path`` is in this set warm the default namespace.
        # Empty (the default) ⇒ no prefetch for any path.
        self.prefetch_paths: Set[str] = set(prefetch_paths or ())

    async def __call__(self, scope, receive, send) -> None:
        if scope.get("type") != "http":
            # Pass-through for lifespan / websocket without context creation.
            await self.app(scope, receive, send)
            return

        from starlette.requests import Request  # local to keep starlette optional at import time

        request = Request(scope, receive=receive)
        trace_id = _extract_trace_id(request)
        real_ctx = False
        try:
            if self.user_provider is None:
                # empty_abtest_context() does not accept trace_id (no RPCs
                # will be issued), but we still want a non-empty trace_id on
                # the ctx so business code observing ctx.trace_id sees the
                # inbound value. Build the empty ctx then overwrite the
                # auto-generated id with the inbound one.
                ctx = self.sdk.empty_abtest_context()
                ctx.trace_id = trace_id
            else:
                uid, attrs = await self.user_provider(request)
                ctx = self.sdk.new_abtest_context(uid, attrs, trace_id=trace_id)
                real_ctx = True
        except Exception:  # noqa: BLE001
            logger.exception("tipsy_ab_config: user provider raised; using empty ctx")
            ctx = self.sdk.empty_abtest_context()
            ctx.trace_id = trace_id
            real_ctx = False

        # URL-whitelisted opt-in prefetch (design D4): only warm the default
        # namespace for a REAL user ctx whose request path EXACTLY matches the
        # whitelist and when a default namespace exists. Empty whitelist / no
        # match / no default ns ⇒ no prefetch (avoids the "every request fires
        # a wasted experiment RPC" anti-pattern).
        if real_ctx and request.url.path in self.prefetch_paths:
            default_ns = self.sdk.default_namespace
            if default_ns:
                ctx.prefetch_config_version_flat_kv_for_namespace(default_ns)

        token = abtest_ctx_var.set(ctx)
        try:
            await self.app(scope, receive, send)
        finally:
            abtest_ctx_var.reset(token)
