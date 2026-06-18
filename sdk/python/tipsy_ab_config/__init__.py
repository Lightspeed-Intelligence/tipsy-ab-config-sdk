"""Tipsy AB-config Python SDK.

This package mirrors the Go SDK 1:1:

- :class:`Client` (returned by :func:`init`) maintains a per-process
  :class:`~tipsy_ab_config.cache.ConfigCache` populated by a startup
  ``PullAll``, a long-lived server-streaming ``Subscribe``, and a 10-second
  fallback ``PullAll`` safety net.
- :func:`Client.get_config_static` is a pure cache read — no abtest, no
  exposure.
- :func:`Client.get_config` resolves the namespace (explicit > project
  default > :class:`NamespaceRequired`), awaits the per-request
  :class:`~tipsy_ab_config.abtest_context.AbtestContext`, and resolves the
  value (abtest hit > full release fallback). Exposure events are emitted
  asynchronously with a 5-minute per-process dedup window.
- :func:`Client.new_abtest_context` eagerly pre-fetches ONLY the prefetch
  namespace (explicit-or-default) via one ``GetExperimentResult`` task; other
  namespaces are fetched lazily and memoised at-most-once on first dynamic
  ``get_config`` (design 04 §B.2/§B.3).
- :func:`Client.get_experiment_result` exposes the full wire request
  (namespace / user_info / layer_ids / type / display) for custom_params.
- :class:`tipsy_ab_config.fastapi_middleware.AbtestMiddleware` wires the
  AbtestContext into ``contextvars`` for FastAPI / Starlette apps.

All gRPC traffic is JWT-authenticated via a Bearer token (or async token
provider).  The SDK never talks to the database; everything goes through
ConfigService.PullAll/Subscribe + AbtestService.GetExperimentResult.
"""

from .exceptions import (
    AbtestContextMissing,
    NamespaceNotSubscribed,
    NamespaceRequired,
    StartupPullFailed,
    SDKClosed,
)
from .abtest_context import AbtestContext, UserInfo, abtest_ctx_var
from .client import Client, init, Config
from .exposure import ExposureEvent, ExposureSink

__version__ = "0.3.0"

__all__ = [
    "AbtestContext",
    "AbtestContextMissing",
    "Client",
    "Config",
    "ExposureEvent",
    "ExposureSink",
    "NamespaceNotSubscribed",
    "NamespaceRequired",
    "SDKClosed",
    "StartupPullFailed",
    "UserInfo",
    "__version__",
    "abtest_ctx_var",
    "init",
]
