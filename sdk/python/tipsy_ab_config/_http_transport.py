"""HTTP transport for the Python SDK (design §5, ST3).

Mirrors the Go SDK's ``transport_http.go``: a thin protojson-over-HTTP client
for the two RPCs the SDK actually uses on the read path:

- ``ConfigService.PullAll``  → ``POST {base}/api/v1/config/pull_all``
- ``AbtestService.GetExperimentResult`` → ``POST {base}/api/v1/abtest/experiment_result``

Wire encoding is protojson via ``google.protobuf.json_format`` (already a base
dependency — zero new deps):

- Request bodies use :func:`MessageToJson` (camelCase output). The server's
  ``decodeProto`` uses ``DiscardUnknown`` and accepts both camelCase and
  snake_case field names, so the request direction needs no special option.
- Responses are parsed with :func:`Parse` and ``ignore_unknown_fields=True``.
  The server emits snake_case (``UseProtoNames: true``); ``Parse`` accepts both
  field-name forms, so decoding is robust either way.

Auth: ``Authorization: Bearer <token>`` where the token is read synchronously
from :meth:`_TokenCache.current` (design Important Details — the Python path
uses the cached value and never awaits the provider per request, matching the
existing gRPC ``_AuthInterceptor``).

These classes are internal implementation detail; only :func:`init` constructs
them and only the gRPC/HTTP-uniform transport interface is used at the call
sites (``_pull_once`` / ``get_experiment_result`` / ``_get_experiment_result_for_ns``).
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Callable

from google.protobuf import json_format

from ._proto.tipsy.config.v1 import config_pb2
from ._proto.tipsy.abtest.v1 import abtest_pb2

if TYPE_CHECKING:  # pragma: no cover - typing only
    import httpx


# Fixed sub-paths appended to the configured base URLs (design §3).
_PULL_ALL_PATH = "/api/v1/config/pull_all"
_EXPERIMENT_RESULT_PATH = "/api/v1/abtest/experiment_result"


class HttpTransportError(Exception):
    """Raised when an HTTP transport request fails.

    Carries the HTTP status code (when available) and any server-provided
    ``{"error": msg}`` body so callers can log it. The SDK call sites treat
    *any* exception as "degrade / retry / log" (design Important Details —
    HTTP errors are not mapped back to gRPC status codes), so this is a plain
    Exception subclass rather than something modelled on grpc status.
    """

    def __init__(self, message: str, *, status_code: int | None = None) -> None:
        super().__init__(message)
        self.status_code = status_code


def _extract_error_message(body: bytes) -> str:
    """Best-effort parse of a non-2xx body's ``{"error": msg}`` payload."""
    import json

    try:
        parsed = json.loads(body)
    except Exception:  # noqa: BLE001 - body may not be JSON
        return body.decode("utf-8", errors="replace").strip()
    if isinstance(parsed, dict):
        err = parsed.get("error")
        if isinstance(err, str) and err:
            return err
    return body.decode("utf-8", errors="replace").strip()


class _HttpTransport:
    """Shared protojson-over-HTTP request machinery for the two endpoints."""

    def __init__(
        self,
        base_url: str,
        client: "httpx.AsyncClient",
        token_fn: Callable[[], str],
        max_recv_message_size: int,
    ) -> None:
        # ``base_url`` is already validated (http(s):// prefix) and trailing
        # ``/`` stripped by ``init``; the fixed sub-path begins with ``/``.
        self._base_url = base_url
        self._client = client
        self._token_fn = token_fn
        self._max_recv = max_recv_message_size

    async def _post_proto(self, path: str, req, response_cls):
        """POST ``req`` as protojson to ``base+path`` and decode the response.

        ``timeout`` is intentionally NOT applied here: the call sites wrap every
        transport call in ``asyncio.wait_for(..., timeout=...)`` exactly as they
        do for the gRPC stub, keeping timeout semantics uniform across both
        transports. ``asyncio.wait_for`` cancelling the coroutine propagates a
        cancel into the in-flight httpx request.
        """
        body = json_format.MessageToJson(req).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer " + self._token_fn(),
        }
        resp = await self._client.post(
            self._base_url + path,
            content=body,
            headers=headers,
        )
        content = resp.content
        if resp.status_code < 200 or resp.status_code >= 300:
            raise HttpTransportError(
                f"tipsy_ab_config: HTTP {resp.status_code}: "
                f"{_extract_error_message(content)}",
                status_code=resp.status_code,
            )
        # Enforce the same response-size ceiling as the gRPC transport
        # (max_recv_message_size, default 512MB). httpx has already buffered the
        # body; we reject oversize responses with a clear error rather than
        # surfacing an obscure decode failure (parity with the Go limit+1 check).
        if len(content) > self._max_recv:
            raise HttpTransportError(
                "tipsy_ab_config: HTTP response exceeds max_recv_message_size "
                f"({len(content)} > {self._max_recv} bytes)",
                status_code=resp.status_code,
            )
        out = response_cls()
        json_format.Parse(content, out, ignore_unknown_fields=True)
        return out


class HttpConfigTransport(_HttpTransport):
    """HTTP implementation of the config transport interface."""

    async def pull_all(
        self, req: "config_pb2.PullAllRequest"
    ) -> "config_pb2.PullAllResponse":
        return await self._post_proto(
            _PULL_ALL_PATH, req, config_pb2.PullAllResponse
        )


class HttpAbtestTransport(_HttpTransport):
    """HTTP implementation of the abtest transport interface."""

    async def get_experiment_result(
        self, req: "abtest_pb2.GetExperimentResultRequest"
    ) -> "abtest_pb2.GetExperimentResultResponse":
        return await self._post_proto(
            _EXPERIMENT_RESULT_PATH, req, abtest_pb2.GetExperimentResultResponse
        )
