"""Tipsy AB-config Python SDK client.

Mirrors ``sdk/go/tipsyabconfig/sdk.go`` with asyncio semantics:

- :func:`init` is an async factory that constructs the gRPC channels,
  performs the startup PullAll-per-ns sweep, and starts background tasks
  for ``Subscribe`` + fallback ``PullAll`` + exposure draining. It also
  resolves the project default namespace once (Config override or the
  ``PROJECT_DEFAULT_NAMESPACE`` env var; decision A-3).
- ``Client.get_config_static`` is synchronous (pure cache read).
- ``Client.get_config`` is async; it resolves the namespace (explicit >
  default > NamespaceRequired), awaits the per-ns memoised
  ``GetExperimentResult`` result, and fires the exposure (non-blocking).
- ``Client.new_abtest_context`` is synchronous: per design 04 §B.2 it eagerly
  pre-fetches ONLY the prefetch namespace (explicit-or-default) via one
  ``GetExperimentResult`` task; other namespaces are fetched lazily and
  memoised at-most-once on first dynamic ``get_config``.
- ``Client.get_experiment_result`` is the thin client that exposes every
  wire parameter (namespace / user_info / layer_ids / type / display) for
  custom_params results (design 04 §B.6).
- ``Client.close`` cancels background tasks, drains the exposure queue,
  and closes the gRPC channels.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import uuid
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from typing import (
    Any,
    Awaitable,
    Callable,
    Dict,
    Iterable,
    List,
    Mapping,
    NamedTuple,
    Optional,
    Sequence,
    Tuple,
)
from urllib.parse import parse_qs, urlparse

import grpc
import grpc.aio

from .abtest_context import (
    AbtestContext,
    UserInfo,
    _ComputeResult,
    _EMPTY_RESULT,
    _ensure_ctx,
    abtest_ctx_var,
)
from .cache import ConfigCache
from .exceptions import (
    NamespaceNotSubscribed,
    NamespaceRequired,
    SDKClosed,
    StartupPullFailed,
)
from .exposure import ExposureEmitter, ExposureSink
from .metrics import Metrics

# defaultNamespaceEnvVar is the environment variable the SDK reads ONCE at init
# to discover the project default namespace (decision A-3 / design 04 §B.1).
# The SDK never hard-codes a default; if this env is empty/unset the default
# namespace stays "" and ns-optional entry points raise NamespaceRequired.
DEFAULT_NAMESPACE_ENV_VAR = "PROJECT_DEFAULT_NAMESPACE"

# TRANSPORT_ENV_VAR selects the SDK transport when ``Config.transport`` is empty
# (design §2). Values are case-insensitive and whitespace-trimmed; unset/empty
# falls back to gRPC.
TRANSPORT_ENV_VAR = "TIPSY_SDK_TRANSPORT"
TRANSPORT_GRPC = "grpc"
TRANSPORT_HTTP = "http"

# Generated proto bindings.
from ._proto.tipsy.config.v1 import config_pb2 as config_pb2
from ._proto.tipsy.config.v1 import config_pb2_grpc as config_pb2_grpc
from ._proto.tipsy.abtest.v1 import abtest_pb2 as abtest_pb2
from ._proto.tipsy.abtest.v1 import abtest_pb2_grpc as abtest_pb2_grpc

logger = logging.getLogger("tipsy_ab_config")


@dataclass
class Config:
    """Startup parameters for :func:`init`."""

    namespaces: Sequence[str] = field(default_factory=list)
    # In gRPC mode (``transport`` == "grpc"/default) ``config_service_addr`` /
    # ``abtest_service_addr`` follow the 方案 Y scheme grammar (mirrors the Go
    # SDK):
    #   - bare "host:port" (e.g. "ab-config-grpc:50051") → plaintext h2c
    #     (backward compatible);
    #   - native grpc target ("dns:///host", "unix:path", "passthrough:///...",
    #     "xds:///...") → passed through verbatim, plaintext;
    #   - "grpc://host:port" → plaintext h2c (query parameters rejected);
    #   - "grpcs://host:port[?authority=<domain>&insecure=true]" → TLS;
    #     ``authority`` overrides :authority + SNI/cert-name target. grpcio
    #     has no native InsecureSkipVerify; ``insecure=true`` only triggers a
    #     warning unless ``tls_root_certificates`` is supplied to trust a private
    #     CA. Never use it in production; missing port is a parameter error;
    #   - "http://"/"https://" → parameter error in gRPC mode (use the HTTP
    #     transport for base URLs).
    # In HTTP mode (``transport`` == "http") these are http(s):// base URLs.
    config_service_addr: str = ""
    abtest_service_addr: str = ""
    pull_interval: float = 10.0  # seconds
    pull_timeout: float = 5.0
    pull_retries: int = 3
    abtest_timeout: float = 1.5
    startup_fail_open: bool = False
    token: str = ""
    token_provider: Optional[Callable[[], Awaitable[str]]] = None
    exposure_sink: Optional[ExposureSink] = None
    exposure_dedup_ttl: float = 300.0
    max_recv_message_size: int = 512 * 1024 * 1024
    max_send_message_size: int = 512 * 1024 * 1024
    # ``default_namespace``, when non-empty, overrides the value read from the
    # ``PROJECT_DEFAULT_NAMESPACE`` environment variable at init. The normal
    # production path leaves this empty and relies on the env var (decision
    # A-3); the override exists mainly for tests and hosts that prefer to
    # inject the default namespace programmatically. An empty
    # ``default_namespace`` AND an empty env var leaves the SDK with no default
    # namespace, in which case ns-optional entry points raise NamespaceRequired.
    default_namespace: str = ""
    # When ``channel_options`` is non-empty it is appended to the SDK's own
    # grpc.aio options (later entries override earlier).
    channel_options: Optional[List[Tuple[str, Any]]] = None
    # ``channel_factory`` is the seam tests use to inject in-memory
    # grpc.aio channels. When set, it is called for each address and the
    # returned channel is used as-is (no other dial-time options).
    channel_factory: Optional[Callable[[str], "grpc.aio.Channel"]] = None
    # ``tls_root_certificates`` is the PEM-encoded trust anchor (bytes) used as
    # ``root_certificates`` for the ``grpcs://`` TLS path. Use it when the server
    # certificate chains to a CA that is NOT in the system trust store —
    # e.g. a Cloudflare Origin CA, a private/self-signed CA. Only takes effect
    # on ``grpcs://`` targets; ignored on the plaintext path. Unlike
    # ``channel_factory`` (which bypasses the SDK entirely and therefore does NOT
    # attach the bearer ``_AuthInterceptor``), this knob keeps the SDK's own
    # secure-channel build, so the token is still auto-attached on every RPC.
    # This is the recommended Python path for Dev direct-IP + Origin Cert: pair
    # it with ``grpcs://...?authority=<gRPC domain>`` so SNI / cert-name match.
    tls_root_certificates: Optional[bytes] = None
    # ``transport`` selects the wire protocol (design §2): "grpc" (default) or
    # "http" (case-insensitive, whitespace-trimmed). Empty means "consult the
    # ``TIPSY_SDK_TRANSPORT`` env var, else default to grpc". In HTTP mode the
    # SDK polls PullAll over HTTP instead of opening a Subscribe stream, so
    # ``channel_factory`` / ``channel_options`` are ignored and configuration
    # change-detection latency is bounded by ``pull_interval`` (default 10s).
    transport: str = ""
    # ``http_client`` is the HTTP-mode test/customisation seam (mirrors
    # ``channel_factory`` for gRPC). When set in HTTP mode it is used as-is and
    # its lifecycle is owned by the caller (``aclose`` does NOT close it). When
    # unset the SDK builds and owns a default ``httpx.AsyncClient``. The type is
    # a string annotation so importing the SDK never requires httpx — it is only
    # imported when HTTP mode is actually selected.
    http_client: Optional["httpx.AsyncClient"] = None


class _TokenCache:
    """Holds the current bearer token + an optional async refresh provider.

    Used by ``_AuthInterceptor`` to inject ``Authorization: Bearer <token>``
    on every outgoing RPC.  For static-token deployments the cached value
    never changes; for dynamic tokens the host periodically calls
    :meth:`refresh` to update the cache (the interceptor never blocks).
    """

    def __init__(
        self,
        static_token: str,
        token_provider: Optional[Callable[[], Awaitable[str]]],
    ) -> None:
        self._static = static_token
        self._provider = token_provider
        self._cached: Optional[str] = static_token or None
        self._lock = asyncio.Lock()

    def current(self) -> str:
        return self._cached or self._static or ""

    async def refresh(self) -> None:
        """Refresh the cached token from the async provider."""
        if self._provider is None:
            return
        token = await self._provider()
        async with self._lock:
            self._cached = token


class _GrpcConfigTransport:
    """gRPC implementation of the config transport interface.

    A zero-logic wrapper over the generated stub so the call sites
    (``_pull_once``) can be transport-agnostic. The timeout wrapping
    (``asyncio.wait_for``) stays at the call site for both transports, so this
    is byte-for-byte the same RPC the SDK issued before the abstraction.
    """

    def __init__(self, stub: "config_pb2_grpc.ConfigServiceStub") -> None:
        self._stub = stub

    async def pull_all(
        self, req: "config_pb2.PullAllRequest"
    ) -> "config_pb2.PullAllResponse":
        return await self._stub.PullAll(req)


class _GrpcAbtestTransport:
    """gRPC implementation of the abtest transport interface (see above)."""

    def __init__(self, stub: "abtest_pb2_grpc.AbtestServiceStub") -> None:
        self._stub = stub

    async def get_experiment_result(
        self, req: "abtest_pb2.GetExperimentResultRequest"
    ) -> "abtest_pb2.GetExperimentResultResponse":
        return await self._stub.GetExperimentResult(req)


class Client:
    """Tipsy AB-config Python SDK handle.

    Construct via :func:`init`; tear down via :func:`aclose` (or use as an
    async context manager).
    """

    def __init__(
        self,
        cfg: Config,
        cache: ConfigCache,
        metrics: Metrics,
        config_transport: Any,
        abtest_transport: Optional[Any],
        exposure: ExposureEmitter,
        auth_plugin: Optional[_TokenCache],
        config_channel: Optional["grpc.aio.Channel"] = None,
        abtest_channel: Optional["grpc.aio.Channel"] = None,
        owned_http_client: Optional[Any] = None,
        config_stub: Optional["config_pb2_grpc.ConfigServiceStub"] = None,
    ) -> None:
        self._cfg = cfg
        self._cache = cache
        self._metrics = metrics
        # Transport abstraction (design §4/§5): the call sites talk to these
        # uniform interfaces; gRPC mode wraps the stubs, HTTP mode wraps httpx.
        self._config_tr = config_transport
        self._abtest_tr = abtest_transport
        # gRPC channels are kept only so ``aclose`` can close them; they are
        # ``None`` in HTTP mode (no channel exists).
        self._config_channel = config_channel
        self._abtest_channel = abtest_channel
        # The raw ConfigService stub is retained ONLY for the gRPC-exclusive
        # Subscribe stream (``_subscribe_once``); it is ``None`` in HTTP mode,
        # where Subscribe is replaced by periodic PullAll polling.
        self._config_stub = config_stub
        # In HTTP mode, the httpx client the SDK built itself (and therefore
        # owns). An injected ``Config.http_client`` is NOT stored here so
        # ``aclose`` leaves its lifecycle to the caller.
        self._owned_http_client = owned_http_client
        self._exposure = exposure
        self._auth = auth_plugin
        self._tasks: List[asyncio.Task] = []
        self._closed = False
        self._namespaces: List[str] = sorted({ns for ns in cfg.namespaces if ns})
        self._namespace_set = set(self._namespaces)

        # Project default namespace (decision A-3 / design 04 §B.1): explicit
        # Config.default_namespace override wins; otherwise read the env var
        # ONCE here. Either may be empty, in which case the SDK has no default
        # namespace and ns-optional entry points raise NamespaceRequired.
        default_ns = cfg.default_namespace or os.getenv(DEFAULT_NAMESPACE_ENV_VAR, "")
        self._default_namespace = default_ns
        self._default_ns_subscribed = bool(default_ns) and default_ns in self._namespace_set
        if default_ns and not self._default_ns_subscribed:
            # Default ns is not subscribed: warn so misconfiguration is visible.
            # ns-optional get_config will still resolve to it and then surface
            # NamespaceNotSubscribed from resolve_namespace.
            logger.warning(
                "tipsy_ab_config: project default namespace is not in subscribed namespaces; "
                "eager pre-request disabled",
                extra={"default_namespace": default_ns, "namespaces": self._namespaces},
            )

    # ---- accessors ----

    @property
    def metrics(self) -> Metrics:
        return self._metrics

    @property
    def cache(self) -> ConfigCache:
        return self._cache

    @property
    def namespaces(self) -> List[str]:
        return list(self._namespaces)

    @property
    def default_namespace(self) -> str:
        """Project default namespace resolved once at init.

        ``Config.default_namespace`` override > ``PROJECT_DEFAULT_NAMESPACE``
        env var > ``""`` (decision A-3 — the SDK never hard-codes one).
        """
        return self._default_namespace

    def is_subscribed(self, namespace: str) -> bool:
        """Report whether ``namespace`` is one this client subscribed to."""
        return namespace in self._namespace_set

    def resolve_namespace(self, namespace: Optional[str]) -> str:
        """Apply the design 04 §B.1 namespace-resolution rules.

        explicit ns argument > ``default_namespace`` > raise
        :class:`NamespaceRequired`. The resolved ns is then validated against
        the subscription set; an unsubscribed ns raises
        :class:`NamespaceNotSubscribed` (the SDK only consumes subscribed
        namespaces).
        """
        ns = namespace or ""
        if not ns:
            ns = self._default_namespace
        if not ns:
            raise NamespaceRequired(
                "tipsy_ab_config: namespace required (no explicit ns and no "
                "project default namespace configured)"
            )
        if ns not in self._namespace_set:
            raise NamespaceNotSubscribed(
                f"tipsy_ab_config: namespace not subscribed: {ns!r}"
            )
        return ns

    # ---- get_config_static + get_config ----

    def get_config_static(
        self,
        namespace: str,
        key: str,
        default: Optional[str] = None,
    ) -> Optional[str]:
        """Return the active full-release value for ``(ns, key)``.

        Synchronous; no abtest call; no exposure event. Use for service-
        level / no-user-context paths.  Returns ``default`` on cache miss.
        """
        if self._closed:
            return default
        v = self._cache.full_release_version(namespace, key)
        if v is None:
            logger.debug(
                "get_config_static miss (no full release)",
                extra={"ns": namespace, "key": key},
            )
            return default
        value = self._cache.value_of(namespace, key, v)
        if value is None:
            logger.debug(
                "get_config_static miss (no value)",
                extra={"ns": namespace, "key": key, "version": v},
            )
            return default
        logger.debug(
            "get_config_static hit",
            extra={
                "ns": namespace,
                "key": key,
                "version": v,
                "source": "full_static",
            },
        )
        return value

    async def get_config(
        self,
        ctx: Optional[AbtestContext],
        namespace: Optional[str],
        key: str,
        default: Optional[str] = None,
    ) -> Optional[str]:
        """Resolve dynamic ``(ns, key)`` honoring abtest hits (whitelist > experiment > full).

        ns resolution (design 04 §B.1, decision A-3): an empty/None ``namespace``
        falls back to the project default namespace (``Config.default_namespace``
        override or the ``PROJECT_DEFAULT_NAMESPACE`` env var read once at init).
        If neither is set, raises :class:`NamespaceRequired`. A
        resolved-but-unsubscribed ns raises :class:`NamespaceNotSubscribed`.

        The per-ns abtest result is memoised into ``ctx`` on first access so the
        whole request link issues AT MOST ONE GetExperimentResult RPC per ns
        (design 04 §B.3). When abtest is unavailable or the per-ns call failed,
        ``get_config`` falls back to the full-release version silently.

        M6 (design 04 §B.3): after obtaining ``config_flat_kv`` the SDK ALWAYS
        preserves the full-release fallback. A key absent from the map is the
        common "no experiment hit" case and resolves to the full-release
        version, NOT the default. The default is only returned when neither an
        abtest hit nor a full-release version exists.
        """
        if self._closed:
            raise SDKClosed("client closed")
        if ctx is None:
            # Fallback: maybe the FastAPI middleware stashed one.
            ctx = abtest_ctx_var.get()
        ctx = _ensure_ctx(ctx)
        resolved_ns = self.resolve_namespace(namespace)

        # Per-ns memoised abtest result (at-most-once RPC per request link).
        abresult: _ComputeResult = await ctx.wait_for_abtest(resolved_ns)

        # abtest hit path: key present in config_flat_kv with a non-zero version.
        ab_version = abresult.key_versions.get(key) if abresult else None
        if ab_version is not None and ab_version != 0:
            value = self._cache.value_of(resolved_ns, key, ab_version)
            if value is not None:
                self._exposure.emit(
                    ctx.user_id,
                    resolved_ns,
                    key,
                    ab_version,
                    abresult.exposures,
                )
                logger.debug(
                    "get_config hit (abtest)",
                    extra={
                        "ns": resolved_ns,
                        "key": key,
                        "version": ab_version,
                        "uid": ctx.user_id,
                    },
                )
                return value
            # ab → full fallback (no exposure; design §B.3 / M6).
            self._metrics.inc_abtest_fallback(resolved_ns)
            logger.warning(
                "tipsy_ab_config: ab version missing in local cache; falling back to full",
                extra={"ns": resolved_ns, "key": key, "ab_version": ab_version},
            )

        # Full-release fallback (M6): key not in config_flat_kv, or ab→full.
        v = self._cache.full_release_version(resolved_ns, key)
        if v is None:
            return default
        value = self._cache.value_of(resolved_ns, key, v)
        if value is None:
            return default
        logger.debug(
            "get_config hit (full)",
            extra={
                "ns": resolved_ns,
                "key": key,
                "version": v,
                "uid": ctx.user_id,
            },
        )
        return value

    async def get_config_default(
        self,
        ctx: Optional[AbtestContext],
        key: str,
        default: Optional[str] = None,
    ) -> Optional[str]:
        """ns-optional convenience form of :meth:`get_config` (design 04 §B.5).

        Resolves the namespace from the project default namespace. Exactly
        :meth:`get_config` with ``namespace=None``, so it raises
        :class:`NamespaceRequired` when no default namespace is configured.
        """
        return await self.get_config(ctx, None, key, default)

    # ---- AbtestContext factory ----

    def new_abtest_context(
        self,
        user_id: str,
        user_attrs: Optional[Mapping[str, Any]] = None,
        namespace: Optional[str] = None,
        *,
        trace_id: Optional[str] = None,
    ) -> AbtestContext:
        """Synchronously create an AbtestContext and eagerly pre-fetch ONE ns.

        v2 semantics (design 04 §B.2): construction does NOT fan out to every
        subscribed namespace. It eagerly pre-fetches AT MOST the prefetch
        namespace — the explicit ``namespace`` argument, else the client
        :attr:`default_namespace` — via one ``GetExperimentResult`` task
        (type=config_version, display=flat_kv). When the prefetch ns is empty
        or not subscribed, NO eager RPC is issued and every ns is fetched
        lazily on first dynamic ``get_config`` (design 04 §B.3).

        ``trace_id`` (sdk-trace-id §5): the request-scoped identifier shared
        by every RPC this ctx issues. ``None`` / ``""`` ⇒ the SDK generates
        a fresh UUID v4 (36-char with-dashes). Any other string is preserved
        verbatim and forwarded to the server.

        Must be called from within a running asyncio event loop (the eager
        pre-request spawns an ``asyncio`` task).
        """
        ctx = AbtestContext(
            user_id=user_id,
            user_attrs=user_attrs,
            owner=self,
            trace_id=trace_id,
        )
        prefetch_ns = namespace or self._default_namespace
        if prefetch_ns and self.is_subscribed(prefetch_ns):
            ctx._spawn_prefetch(prefetch_ns)
        return ctx

    def empty_abtest_context(self) -> AbtestContext:
        """Return an identity-less AbtestContext that never issues an RPC.

        Use this on non-user paths (cron jobs, internal pipelines) so
        ``get_config`` still works: every not-yet-resolved ns short-circuits to
        the empty result without a GetExperimentResult RPC (design 04 §B.2).
        """
        return AbtestContext(user_id="", owner=self, empty=True)

    @asynccontextmanager
    async def abtest_scope(
        self,
        user_id: str,
        user_attrs: Optional[Mapping[str, Any]] = None,
        namespace: Optional[str] = None,
        *,
        trace_id: Optional[str] = None,
    ):
        """Async context manager that sets the ContextVar.

        Used by non-web pipelines so ``get_config`` calls inside the block
        automatically see the per-user AbtestContext. ``trace_id`` follows
        the same rule as :meth:`new_abtest_context` (None/"" ⇒ SDK-generated
        UUID v4).
        """
        ctx = self.new_abtest_context(
            user_id, user_attrs, namespace, trace_id=trace_id
        )
        token = abtest_ctx_var.set(ctx)
        try:
            yield ctx
        finally:
            abtest_ctx_var.reset(token)

    def mock_abtest_context(
        self,
        user_id: str,
        key_versions_by_ns: Mapping[str, Mapping[str, int]],
    ) -> AbtestContext:
        """Test helper from abtest-platform-sdk.md §9.4.

        Each entry in ``key_versions_by_ns`` pre-resolves the abtest result for
        that namespace; namespaces NOT in the map resolve to the empty result
        (the ctx is marked empty, so the lazy path short-circuits without an
        RPC), matching the Go ``MockAbtestContext`` behaviour.
        """
        ctx = AbtestContext(user_id=user_id, owner=self, empty=True)
        for ns, kv in key_versions_by_ns.items():
            ctx._seed_result(
                ns,
                _ComputeResult(
                    key_versions={str(k): int(v) for k, v in kv.items()},
                    exposures=[],
                ),
            )
        return ctx

    # ---- GetExperimentResult client ----

    async def get_experiment_result(
        self,
        namespace: Optional[str],
        user_info: Optional[UserInfo] = None,
        layer_ids: Optional[Sequence[str]] = None,
        experiment_type: int = abtest_pb2.ExperimentType.EXPERIMENT_TYPE_UNSPECIFIED,
        display_type: int = abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_UNSPECIFIED,
        *,
        trace_id: Optional[str] = None,
    ) -> "abtest_pb2.GetExperimentResultResponse":
        """Thin exported wrapper over ``AbtestService.GetExperimentResult``.

        Mirrors Go's ``GetExperimentResult`` (design 04 §B.6). Exposes every
        wire parameter (namespace / user_info / layer_ids / experiment_type /
        display_type) so business code can fetch custom_params results (or
        per-group results) directly. Unlike :meth:`get_config` it does NOT
        memoise into an AbtestContext, does NOT touch the local config cache,
        and does NOT emit exposures — it returns the raw proto response.

        Namespace resolution mirrors :meth:`get_config` (explicit > default >
        :class:`NamespaceRequired`; unsubscribed > :class:`NamespaceNotSubscribed`).
        Raises when the abtest service was not configured at init.

        ``trace_id`` (sdk-trace-id §5): ``None`` / ``""`` ⇒ the SDK generates
        a fresh UUID v4 locally and forwards it on the wire so the request
        is still trace-identifiable end-to-end. Any other string is preserved
        verbatim.
        """
        if self._closed:
            raise SDKClosed("client closed")
        ns = self.resolve_namespace(namespace)
        if self._abtest_tr is None:
            raise SDKClosed("tipsy_ab_config: abtest service not configured")
        ui = user_info or UserInfo()
        tid = trace_id if trace_id else str(uuid.uuid4())
        req = abtest_pb2.GetExperimentResultRequest(
            namespace=ns,
            user_id=ui.uid,
            user_attrs=_encode_user_attrs(ui.attrs),
            layer_ids=list(layer_ids or []),
            experiment_type=experiment_type,
            display_type=display_type,
            trace_id=tid,
        )
        return await asyncio.wait_for(
            self._abtest_tr.get_experiment_result(req),
            timeout=self._cfg.abtest_timeout,
        )

    async def _get_experiment_result_for_ns(
        self,
        ns: str,
        user_id: str,
        user_attrs: Mapping[str, Any],
        trace_id: str,
    ) -> _ComputeResult:
        """Fetch the config_version flat_kv result the get_config fast path uses.

        On any error (missing abtest connection, timeout, RPC failure) it bumps
        the per-ns fallback counter and returns the empty result so the caller
        degrades to full release silently (design 04 §B.3). Mirrors Go's
        ``getExperimentResultForNamespace``.

        ``trace_id`` is forwarded verbatim onto the proto request so the SDK
        log line, the server log line and any downstream exposure all carry
        the same identifier (sdk-trace-id §5).
        """
        if self._abtest_tr is None:
            self._metrics.inc_abtest_fallback(ns)
            return _EMPTY_RESULT
        req = abtest_pb2.GetExperimentResultRequest(
            namespace=ns,
            user_id=user_id,
            user_attrs=_encode_user_attrs(user_attrs),
            experiment_type=abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION,
            display_type=abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_FLAT_KV,
            trace_id=trace_id,
        )
        try:
            resp = await asyncio.wait_for(
                self._abtest_tr.get_experiment_result(req),
                timeout=self._cfg.abtest_timeout,
            )
        except asyncio.TimeoutError:
            self._metrics.inc_abtest_fallback(ns)
            logger.warning(
                "tipsy_ab_config: AbtestService.GetExperimentResult timeout; "
                "falling back to full release",
                extra={"ns": ns, "timeout_s": self._cfg.abtest_timeout, "trace_id": trace_id},
            )
            return _EMPTY_RESULT
        except asyncio.CancelledError:
            raise
        except Exception:  # noqa: BLE001
            self._metrics.inc_abtest_fallback(ns)
            logger.exception(
                "tipsy_ab_config: AbtestService.GetExperimentResult failed; "
                "falling back to full release",
                extra={"ns": ns, "trace_id": trace_id},
            )
            return _EMPTY_RESULT
        return _ComputeResult(
            key_versions={str(k): int(v) for k, v in resp.config_flat_kv.items()},
            exposures=list(resp.exposures),
        )

    # ---- background loops ----

    async def _startup_pull_all(self) -> None:
        failed: List[Tuple[str, BaseException]] = []
        for ns in self._namespaces:
            try:
                await self._pull_once_with_retries(ns)
            except Exception as e:  # noqa: BLE001
                self._metrics.inc_pull_failure(ns)
                logger.error(
                    "tipsy_ab_config: startup PullAll failed",
                    extra={"ns": ns, "err": str(e)},
                )
                failed.append((ns, e))
        if failed:
            details = ", ".join(f"{ns}={err}" for ns, err in failed)
            raise StartupPullFailed(f"startup PullAll failed: {details}")

    async def _pull_once_with_retries(self, ns: str) -> None:
        backoff = 0.2
        last_err: Optional[BaseException] = None
        for attempt in range(self._cfg.pull_retries):
            if attempt > 0:
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 5.0)
            try:
                await self._pull_once(ns)
                return
            except Exception as e:  # noqa: BLE001
                last_err = e
        if last_err is not None:
            raise last_err

    async def _pull_once(self, ns: str) -> None:
        # sdk-trace-id §5: each background PullAll generates a fresh trace_id
        # so the server can correlate "which SDK pull triggered which log line"
        # even though the SDK instance is the same. Mirrors the Go SDK pattern.
        trace_id = str(uuid.uuid4())
        logger.debug(
            "tipsy_ab_config: PullAll",
            extra={"ns": ns, "trace_id": trace_id},
        )
        req = config_pb2.PullAllRequest(namespaces=[ns], trace_id=trace_id)
        try:
            resp = await asyncio.wait_for(
                self._config_tr.pull_all(req),
                timeout=self._cfg.pull_timeout,
            )
        except asyncio.TimeoutError as e:
            raise TimeoutError(f"PullAll({ns}) timed out") from e
        self._apply_snapshots(resp.snapshots)

    def _apply_snapshots(self, snaps: Iterable) -> None:
        for snap in snaps:
            result = self._cache.apply(snap)
            if result.business_moved:
                self._metrics.inc_business_seq_change(snap.namespace)
            if result.experiment_moved:
                self._metrics.inc_experiment_seq_change(snap.namespace)
            if result.replaced:
                self._metrics.set_local_cache_bytes(
                    snap.namespace,
                    self._cache.snapshot(snap.namespace).cached_bytes if self._cache.snapshot(snap.namespace) else 0,
                )
                logger.debug(
                    "cache replaced",
                    extra={
                        "ns": snap.namespace,
                        "business_seq": snap.business_snapshot_seq,
                        "experiment_seq": snap.experiment_snapshot_seq,
                    },
                )

    async def _run_pull_loop(self) -> None:
        try:
            while not self._closed:
                await asyncio.sleep(self._cfg.pull_interval)
                if self._closed:
                    return
                for ns in self._namespaces:
                    if self._closed:
                        return
                    try:
                        await self._pull_once(ns)
                    except Exception as e:  # noqa: BLE001
                        self._metrics.inc_pull_failure(ns)
                        logger.error(
                            "tipsy_ab_config: periodic PullAll failed",
                            extra={"ns": ns, "err": str(e)},
                        )
        except asyncio.CancelledError:
            return

    async def _run_subscribe(self) -> None:
        backoff = 1.0
        max_backoff = 30.0
        try:
            while not self._closed:
                try:
                    await self._subscribe_once()
                    backoff = 1.0
                except asyncio.CancelledError:
                    return
                except Exception as e:  # noqa: BLE001
                    for ns in self._namespaces:
                        self._metrics.inc_subscribe_disconnect(ns)
                    logger.error(
                        "tipsy_ab_config: Subscribe stream error; reconnecting",
                        extra={"err": str(e), "backoff_s": backoff},
                    )
                    try:
                        await asyncio.sleep(backoff)
                    except asyncio.CancelledError:
                        return
                    backoff = min(backoff * 2, max_backoff)
        except asyncio.CancelledError:
            return

    async def _subscribe_once(self) -> None:
        known = self._cache.known_seqs(self._namespaces)
        # sdk-trace-id §5: trace_id on Subscribe identifies "which stream
        # establishment". The server only logs this id on the entry + stream
        # error paths; per-event server pushes do not reuse it.
        trace_id = str(uuid.uuid4())
        logger.debug(
            "tipsy_ab_config: Subscribe",
            extra={"namespaces": list(self._namespaces), "trace_id": trace_id},
        )
        req = config_pb2.SubscribeRequest(
            namespaces=list(self._namespaces),
            trace_id=trace_id,
        )
        for ns, (biz, exp) in known.items():
            req.known_seqs[ns].business_snapshot_seq = biz
            req.known_seqs[ns].experiment_snapshot_seq = exp
        call = self._config_stub.Subscribe(req)
        async for ev in call:
            self._handle_event(ev)

    def _handle_event(self, ev) -> None:
        if ev is None:
            return
        which = ev.WhichOneof("payload")
        if which == "snapshot":
            snap = ev.snapshot
            self._metrics.inc_subscribe_event(snap.namespace)
            result = self._cache.apply(snap)
            if result.business_moved:
                self._metrics.inc_business_seq_change(snap.namespace)
            if result.experiment_moved:
                self._metrics.inc_experiment_seq_change(snap.namespace)
            if result.replaced:
                cur = self._cache.snapshot(snap.namespace)
                if cur is not None:
                    self._metrics.set_local_cache_bytes(snap.namespace, cur.cached_bytes)
        # Unknown payload branches are silently skipped per design §5.2.

    # ---- lifecycle ----

    async def aclose(self) -> None:
        if self._closed:
            return
        self._closed = True
        for t in self._tasks:
            t.cancel()
        # Wait for tasks to finish (with a small grace period).
        if self._tasks:
            with contextlib.suppress(Exception):
                await asyncio.gather(*self._tasks, return_exceptions=True)
        # Drain exposure.
        with contextlib.suppress(Exception):
            await self._exposure.aclose()
        # Close gRPC channels (gRPC mode only; ``None`` in HTTP mode).
        if self._config_channel is not None:
            with contextlib.suppress(Exception):
                await self._config_channel.close()
        if self._abtest_channel is not None:
            with contextlib.suppress(Exception):
                await self._abtest_channel.close()
        # Close the httpx client the SDK built itself (HTTP mode). An injected
        # ``Config.http_client`` is left untouched — the caller owns it.
        if self._owned_http_client is not None:
            with contextlib.suppress(Exception):
                await self._owned_http_client.aclose()

    async def __aenter__(self) -> "Client":
        return self

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.aclose()


# ----------------------------------------------------------------------
# Public init()
# ----------------------------------------------------------------------


async def init(cfg: Config) -> Client:
    """Construct the SDK, perform startup PullAll, start background tasks.

    Transport (design §2/§5) is resolved here: ``cfg.transport`` (if non-empty)
    > ``TIPSY_SDK_TRANSPORT`` env var > ``"grpc"``. The default (gRPC) path is
    unchanged. In HTTP mode the SDK polls PullAll over HTTP and never opens a
    Subscribe stream.
    """
    if not cfg.namespaces:
        raise ValueError("tipsy_ab_config: cfg.namespaces must be non-empty")

    transport = _resolve_transport(cfg)

    if transport == TRANSPORT_HTTP:
        return await _init_http(cfg)
    return await _init_grpc(cfg)


def _resolve_transport(cfg: Config) -> str:
    """Resolve the transport mode (design §2): Config > env > default grpc."""
    raw = cfg.transport or os.getenv(TRANSPORT_ENV_VAR, "")
    mode = raw.strip().lower()
    if not mode:
        return TRANSPORT_GRPC
    if mode not in (TRANSPORT_GRPC, TRANSPORT_HTTP):
        raise ValueError(
            f"tipsy_ab_config: invalid transport {raw!r}; expected "
            f"{TRANSPORT_GRPC!r} or {TRANSPORT_HTTP!r} "
            f"(set Config.transport or the {TRANSPORT_ENV_VAR} env var)"
        )
    return mode


def _normalize_http_base_url(addr: str, field_name: str) -> str:
    """Validate an HTTP base URL and strip its trailing ``/`` (design §3).

    Validation applies only to non-empty addresses; callers handle the empty
    (degraded) case before calling this.
    """
    if not (addr.startswith("http://") or addr.startswith("https://")):
        raise ValueError(
            f"tipsy_ab_config: HTTP mode {field_name} must start with "
            f"'http://' or 'https://'; got {addr!r}"
        )
    return addr.rstrip("/")


async def _init_grpc(cfg: Config) -> Client:
    """gRPC-mode init (default; behaviour unchanged from before ST3)."""
    if not cfg.config_service_addr and cfg.channel_factory is None:
        raise ValueError("tipsy_ab_config: cfg.config_service_addr must be set")
    if not cfg.token and cfg.token_provider is None and cfg.channel_factory is None:
        raise ValueError("tipsy_ab_config: cfg.token or cfg.token_provider must be set")

    cache = ConfigCache()
    metrics = Metrics()

    auth_plugin: Optional[_TokenCache] = None
    if cfg.channel_factory is None:
        auth_plugin = _TokenCache(cfg.token, cfg.token_provider)
        if cfg.token_provider is not None:
            # Prime the cache.
            try:
                await auth_plugin.refresh()
            except Exception:  # noqa: BLE001
                logger.exception("token_provider failed during init")

    config_channel = _build_channel(cfg, cfg.config_service_addr, auth_plugin)
    abtest_channel = (
        _build_channel(cfg, cfg.abtest_service_addr, auth_plugin)
        if cfg.abtest_service_addr or cfg.channel_factory is not None
        else None
    )

    config_stub = config_pb2_grpc.ConfigServiceStub(config_channel)
    abtest_stub = (
        abtest_pb2_grpc.AbtestServiceStub(abtest_channel)
        if abtest_channel is not None
        else None
    )

    exposure = ExposureEmitter(
        sink=cfg.exposure_sink,
        ttl_seconds=cfg.exposure_dedup_ttl,
    )
    await exposure.start()

    client = Client(
        cfg=cfg,
        cache=cache,
        metrics=metrics,
        config_transport=_GrpcConfigTransport(config_stub),
        abtest_transport=(
            _GrpcAbtestTransport(abtest_stub) if abtest_stub is not None else None
        ),
        exposure=exposure,
        auth_plugin=auth_plugin,
        config_channel=config_channel,
        abtest_channel=abtest_channel,
        config_stub=config_stub,
    )

    await _run_startup_pull(client, cfg, metrics)

    loop = asyncio.get_event_loop()
    client._tasks.append(loop.create_task(client._run_subscribe()))
    client._tasks.append(loop.create_task(client._run_pull_loop()))
    return client


async def _init_http(cfg: Config) -> Client:
    """HTTP-mode init (design §5): protojson-over-HTTP, polling, no Subscribe."""
    if not cfg.config_service_addr:
        raise ValueError(
            "tipsy_ab_config: HTTP mode requires cfg.config_service_addr "
            "(an http(s):// base URL)"
        )
    if not cfg.token and cfg.token_provider is None:
        raise ValueError("tipsy_ab_config: cfg.token or cfg.token_provider must be set")

    config_base = _normalize_http_base_url(
        cfg.config_service_addr, "config_service_addr"
    )
    # Empty abtest base URL keeps the existing degraded semantics (abtest
    # transport stays None); validate only when non-empty (design §3 / F2).
    abtest_base = (
        _normalize_http_base_url(cfg.abtest_service_addr, "abtest_service_addr")
        if cfg.abtest_service_addr
        else ""
    )

    # httpx is an optional dependency; import lazily only when HTTP mode is
    # actually selected so pure-gRPC users never need it installed.
    try:
        import httpx  # noqa: F401
    except ImportError as e:  # pragma: no cover - import guard
        raise ImportError(
            "tipsy_ab_config: HTTP transport requires httpx; install it with "
            "'pip install tipsy-ab-config[http]'"
        ) from e

    from ._http_transport import HttpAbtestTransport, HttpConfigTransport

    cache = ConfigCache()
    metrics = Metrics()

    auth_plugin = _TokenCache(cfg.token, cfg.token_provider)
    if cfg.token_provider is not None:
        try:
            await auth_plugin.refresh()
        except Exception:  # noqa: BLE001
            logger.exception("token_provider failed during init")

    # Use the injected client if provided (test/customisation seam); else build
    # and own a default one (closed by aclose).
    owned_http_client: Optional["httpx.AsyncClient"] = None
    if cfg.http_client is not None:
        http_client = cfg.http_client
    else:
        http_client = httpx.AsyncClient()
        owned_http_client = http_client

    config_tr = HttpConfigTransport(
        base_url=config_base,
        client=http_client,
        token_fn=auth_plugin.current,
        max_recv_message_size=cfg.max_recv_message_size,
    )
    abtest_tr = (
        HttpAbtestTransport(
            base_url=abtest_base,
            client=http_client,
            token_fn=auth_plugin.current,
            max_recv_message_size=cfg.max_recv_message_size,
        )
        if abtest_base
        else None
    )

    exposure = ExposureEmitter(
        sink=cfg.exposure_sink,
        ttl_seconds=cfg.exposure_dedup_ttl,
    )
    await exposure.start()

    client = Client(
        cfg=cfg,
        cache=cache,
        metrics=metrics,
        config_transport=config_tr,
        abtest_transport=abtest_tr,
        exposure=exposure,
        auth_plugin=auth_plugin,
        owned_http_client=owned_http_client,
    )

    await _run_startup_pull(client, cfg, metrics)

    loop = asyncio.get_event_loop()
    # HTTP mode: no Subscribe stream — periodic PullAll polling only.
    client._tasks.append(loop.create_task(client._run_pull_loop()))
    return client


async def _run_startup_pull(client: "Client", cfg: Config, metrics: Metrics) -> None:
    """Shared startup PullAll sweep + fail-open handling for both transports."""
    try:
        await client._startup_pull_all()
    except StartupPullFailed as e:
        if not cfg.startup_fail_open:
            await client.aclose()
            raise
        metrics.inc_cache_empty()
        logger.error(
            "tipsy_ab_config: startup PullAll failed; running with empty cache (fail-open)",
            extra={"err": str(e)},
        )


class _GrpcTarget(NamedTuple):
    """Parsed form of a gRPC-mode address string (方案 Y).

    Mirrors the Go SDK ``grpcTarget``:

    - ``dial_target``: the address handed to grpc.aio (scheme + query stripped
      to a bare "host:port" for grpc/grpcs; the original string verbatim for
      bare host:port and native grpc targets).
    - ``use_tls``: True only for ``grpcs://``; everything else is plaintext.
    - ``authority``: overrides the HTTP/2 :authority and, under TLS, the SNI /
      ServerName (cert-name target). Empty means "do not override".
    - ``insecure_skip_verify``: parsed from ``grpcs://...?insecure=true``. In
      Python this is advisory only because grpcio has no native
      InsecureSkipVerify; without ``Config.tls_root_certificates`` the channel
      still verifies against system roots and logs a warning.
    """

    dial_target: str
    use_tls: bool
    authority: str
    insecure_skip_verify: bool


# Managed scheme prefixes that drive scheme-based parsing. The judgement is a
# LITERAL str.startswith() match on these four strings — NOT a generic urlparse
# or "contains '://'" test. This is load bearing for two reasons:
#   1. native grpc targets such as "passthrough:///bufnet-config" also contain
#      "://" but MUST pass through as plaintext;
#   2. urlparse misclassifies bare "host:port" — e.g.
#      urlparse("ab-config-grpc:50051") yields scheme="ab-config-grpc",
#      path="50051" (a false positive). So we never trust urlparse's scheme;
#      we decide the rule by literal prefix first, and only urlparse the
#      remainder AFTER confirming a grpc:// / grpcs:// prefix.
_SCHEME_GRPC = "grpc://"
_SCHEME_GRPCS = "grpcs://"
_SCHEME_HTTP = "http://"
_SCHEME_HTTPS = "https://"


def _parse_grpc_target(addr: str) -> _GrpcTarget:
    """Resolve a gRPC-mode address into a :class:`_GrpcTarget` (方案 Y rules).

    Mirrors the Go ``parseGRPCTarget``. All errors are parameter errors
    (``ValueError`` with prefix ``tipsy_ab_config:``) raised at init parse time,
    before any channel is built, and are never absorbed by ``startup_fail_open``.

    Rules:
      1. Only addresses literally prefixed with grpc:// / grpcs:// / http:// /
         https:// are parsed by scheme.
      2. Everything else (bare "host:port" like "127.0.0.1:50051"; native grpc
         targets like "passthrough:///...", "dns:///host", "unix:path",
         "xds:///") passes through verbatim as plaintext — the unchanged legacy
         path.
      3. grpc://host:port → plaintext; any ?authority=/?insecure= query is a
         parameter error (Q1: those params are meaningless on plaintext).
      4. grpcs://host:port[?authority=&insecure=] → TLS; parse the query. A
         missing port is a parameter error (Q2: no implicit :443).
      5. http:// / https:// in gRPC mode is a parameter error (use the HTTP
         transport).
    """
    # IMPORTANT (Review F9): decide the rule by literal prefix FIRST. Never run
    # ``urlparse`` on a bare host:port — urlparse("ab-config-grpc:50051") would
    # misread "ab-config-grpc" as the scheme.
    if addr.startswith(_SCHEME_GRPCS):
        return _parse_grpcs_scheme(addr)
    if addr.startswith(_SCHEME_GRPC):
        return _parse_grpc_scheme(addr)
    if addr.startswith(_SCHEME_HTTP) or addr.startswith(_SCHEME_HTTPS):
        raise ValueError(
            f"tipsy_ab_config: {addr!r} is an HTTP base URL; gRPC mode expects a "
            "gRPC target (use the HTTP transport for http(s):// base URLs)"
        )
    # Rule 2: bare host:port and native grpc targets pass through verbatim as
    # plaintext. This keeps bufconn / in-memory / internal-DNS dialing working.
    return _GrpcTarget(
        dial_target=addr,
        use_tls=False,
        authority="",
        insecure_skip_verify=False,
    )


def _parse_grpc_scheme(addr: str) -> _GrpcTarget:
    """Handle grpc:// (plaintext). Query parameters are rejected (Q1)."""
    rest = addr[len(_SCHEME_GRPC):]
    hostport, _, query = rest.partition("?")
    if query:
        raise ValueError(
            f"tipsy_ab_config: query parameters are only valid under grpcs:// "
            f"(got {addr!r} on a plaintext grpc:// target); did you mean grpcs://?"
        )
    if not hostport:
        raise ValueError(
            f"tipsy_ab_config: grpc:// target is missing host:port in {addr!r}"
        )
    return _GrpcTarget(
        dial_target=hostport,
        use_tls=False,
        authority="",
        insecure_skip_verify=False,
    )


def _parse_grpcs_scheme(addr: str) -> _GrpcTarget:
    """Handle grpcs:// (TLS). Requires an explicit port (Q2); parses query.

    Only after confirming the grpcs:// prefix do we ``urlparse`` the remainder —
    safe because the stripped string is a real "//host:port?query" URL form.
    """
    rest = addr[len(_SCHEME_GRPCS):]
    hostport, _, raw_query = rest.partition("?")
    if not hostport:
        raise ValueError(
            f"tipsy_ab_config: grpcs:// target is missing host:port in {addr!r}"
        )
    # Parse host:port from the stripped remainder. We prepend "//" so urlparse
    # treats it as a netloc rather than a path, giving reliable hostname/port.
    parsed = urlparse("//" + hostport)
    try:
        port = parsed.port  # raises ValueError on a malformed port
    except ValueError as e:
        raise ValueError(
            f"tipsy_ab_config: invalid port in grpcs:// target {addr!r}: {e}"
        ) from e
    if port is None or not parsed.hostname:
        raise ValueError(
            f"tipsy_ab_config: grpcs:// target must specify an explicit port "
            f"(e.g. :443) in {addr!r}"
        )

    authority = ""
    insecure_skip_verify = False
    if raw_query:
        params = parse_qs(raw_query, keep_blank_values=True)
        for key in params:
            if key not in ("authority", "insecure"):
                raise ValueError(
                    f"tipsy_ab_config: unknown query parameter {key!r} in grpcs:// "
                    f"target {addr!r} (supported: authority, insecure)"
                )
        if "authority" in params:
            authority = params["authority"][-1]
        if "insecure" in params:
            raw = params["insecure"][-1].strip().lower()
            if raw in ("true", "1"):
                insecure_skip_verify = True
            elif raw in ("false", "0", ""):
                insecure_skip_verify = False
            else:
                raise ValueError(
                    f"tipsy_ab_config: invalid insecure value {params['insecure'][-1]!r} "
                    f"in grpcs:// target {addr!r} (expected true/false)"
                )

    return _GrpcTarget(
        dial_target=hostport,
        use_tls=True,
        authority=authority,
        insecure_skip_verify=insecure_skip_verify,
    )


def _build_channel(
    cfg: Config,
    addr: str,
    auth_plugin: Optional[_TokenCache],
) -> "grpc.aio.Channel":
    # HARD CONSTRAINT (Review F3): the channel_factory short-circuit MUST be the
    # very first thing here, BEFORE any address parsing. When a factory is
    # injected (tests, or a host supplying a custom secure channel with its own
    # root_certificates) the addr is handed through untouched and never parsed.
    if cfg.channel_factory is not None:
        return cfg.channel_factory(addr)

    target = _parse_grpc_target(addr)

    options: List[Tuple[str, Any]] = [
        ("grpc.max_receive_message_length", cfg.max_recv_message_size),
        ("grpc.max_send_message_length", cfg.max_send_message_size),
        ("grpc.keepalive_time_ms", 30_000),
        ("grpc.keepalive_timeout_ms", 5_000),
        ("grpc.keepalive_permit_without_calls", 1),
    ]
    if target.use_tls and target.authority:
        # ssl_target_name_override drives SNI + the certificate-name target;
        # default_authority drives the HTTP/2 :authority. Dev sets both to the
        # gRPC domain (so Traefik SNI-routes to the gRPC backend and, when
        # verification is on, the cert name matches).
        options.append(("grpc.ssl_target_name_override", target.authority))
        options.append(("grpc.default_authority", target.authority))
    if cfg.channel_options:
        options.extend(cfg.channel_options)

    interceptors: List = []
    if auth_plugin is not None:
        interceptors.append(_AuthInterceptor(auth_plugin))

    if not target.use_tls:
        # Plaintext h2c — the unchanged legacy path (bare host:port, grpc://, or
        # a native grpc target). Mirrors the Go SDK's "insecure transport +
        # PerRPCCredentials" path; production deployments terminate TLS at the LB
        # or use grpcs://.
        return grpc.aio.insecure_channel(
            target.dial_target, options=options, interceptors=interceptors
        )

    # TLS path (grpcs://).
    #
    # root_certificates injection (design Q3 / Review F7): grpcio has no
    # Go-style InsecureSkipVerify, and a Cloudflare Origin CA (or any private /
    # self-signed CA) is NOT in the system trust store — so "system roots +
    # authority override" will fail verification against such a server. Two
    # injection seams exist, in order of preference for the Origin-Cert case:
    #   1. ``Config.tls_root_certificates`` (PEM bytes): the SDK still builds its
    #      OWN secure channel and still attaches ``_AuthInterceptor`` (token
    #      auto-on-the-wire). This is the recommended Python path — it is the
    #      only way to get BOTH a custom trust anchor AND automatic bearer auth.
    #   2. ``Config.channel_factory``: full custom channel (handled by the
    #      short-circuit above). NOTE: the factory bypasses the SDK entirely, so
    #      it does NOT attach ``_AuthInterceptor`` — the caller must wire auth
    #      itself. Kept as a generic escape hatch, no longer the Origin-Cert
    #      recommendation.
    root = cfg.tls_root_certificates  # PEM bytes or None (None → system roots).
    if target.insecure_skip_verify and root is None:
        # ``insecure=true`` was requested but no trust anchor was injected.
        # grpcio cannot truly skip certificate verification through the standard
        # ssl_channel_credentials API (there is no InsecureSkipVerify), so this
        # is best-effort: SNI / cert-name override (set above) + system roots.
        # For Dev direct-IP + Cloudflare Origin Cert this will MOST LIKELY still
        # fail verification. The Python recommendation is to set
        # ``Config.tls_root_certificates`` to the Origin CA PEM (which then
        # verifies normally and keeps token auto-auth). WARN so the limitation
        # is visible at runtime; never use in production.
        logger.warning(
            "tipsy_ab_config: insecure=true requested but grpcio has no native "
            "InsecureSkipVerify and no Config.tls_root_certificates was provided; "
            "built a standard secure channel with system roots. For Dev "
            "direct-IP + Origin Cert this will likely fail verification — set "
            "Config.tls_root_certificates to the Origin CA PEM (recommended; "
            "keeps automatic bearer auth) or inject a custom channel via "
            "Config.channel_factory. Never use in production.",
            extra={"dial_target": target.dial_target, "authority": target.authority},
        )
    # When ``tls_root_certificates`` is set the channel verifies against the
    # injected anchor normally, so ``insecure=true`` becomes moot (no real
    # skip-verify is needed — and grpcio can't do one anyway).
    credentials = grpc.ssl_channel_credentials(root_certificates=root)
    return grpc.aio.secure_channel(
        target.dial_target,
        credentials,
        options=options,
        interceptors=interceptors,
    )


class _AuthInterceptor(
    grpc.aio.UnaryUnaryClientInterceptor,
    grpc.aio.UnaryStreamClientInterceptor,
):
    """Fallback bearer interceptor used when secure_channel can't be built.

    Attaches ``authorization: Bearer <token>`` to every outgoing RPC.
    """

    def __init__(self, cache: _TokenCache) -> None:
        self._cache = cache

    def _token(self) -> str:
        return self._cache.current()

    async def intercept_unary_unary(self, continuation, client_call_details, request):
        new_details = _append_auth(client_call_details, self._token())
        return await continuation(new_details, request)

    async def intercept_unary_stream(self, continuation, client_call_details, request):
        new_details = _append_auth(client_call_details, self._token())
        return await continuation(new_details, request)


def _append_auth(details, token: str):
    md = list(details.metadata) if details.metadata is not None else []
    md.append(("authorization", "Bearer " + token))
    return details._replace(metadata=md)


def _encode_user_attrs(attrs: Mapping[str, Any]) -> Mapping[str, Any]:
    """Build the ``map<string, Value>`` proto payload from a plain dict."""
    if not attrs:
        return {}
    out: Dict[str, abtest_pb2.Value] = {}
    for k, v in attrs.items():
        encoded = _encode_value(v)
        if encoded is None:
            logger.warning(
                "tipsy_ab_config: dropping unsupported user_attr value type",
                extra={"key": k, "type": type(v).__name__},
            )
            continue
        out[k] = encoded
    return out


def _encode_value(v: Any) -> Optional[abtest_pb2.Value]:
    if isinstance(v, bool):
        return abtest_pb2.Value(b=v)
    if isinstance(v, int):
        return abtest_pb2.Value(i=v)
    if isinstance(v, float):
        return abtest_pb2.Value(d=v)
    if isinstance(v, str):
        return abtest_pb2.Value(s=v)
    return None
