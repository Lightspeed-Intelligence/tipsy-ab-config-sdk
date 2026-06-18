"""FastAPI / Starlette middleware trace_id header extraction (design §5, decision #3).

The middleware must, on every inbound HTTP request:

1. Look for ``X-Trace-Id`` first; if non-empty, use it as the request's
   trace_id verbatim.
2. Else look for ``X-Request-Id`` (common upstream gateway header); if
   non-empty, use it as the request's trace_id verbatim.
3. Else generate a canonical 36-char dashed UUID.

The resulting trace_id is then forwarded to ``Client.new_abtest_context(...,
trace_id=...)`` so the AbtestContext stashed on the contextvar carries the
request-scoped id. Downstream handlers read the ctx via the
``abtest_ctx_var`` contextvar or (more conveniently) via
``request.state.abtest_ctx`` if the middleware also exposes it there — we
test through the contextvar because that's the only public seam already
documented in ``test_fastapi.py``.

This test file mirrors the pytest-asyncio + httpx ASGI transport pattern used
by the sibling ``test_fastapi.py`` to avoid TestClient's sync-in-async
deadlock.
"""

from __future__ import annotations

import pytest

from tipsy_ab_config import Config, abtest_ctx_var, init

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_snapshot,
)


starlette = pytest.importorskip("starlette")
httpx = pytest.importorskip("httpx")


def _looks_like_uuid_v4(s: str) -> bool:
    if not isinstance(s, str) or len(s) != 36:
        return False
    if s[8] != "-" or s[13] != "-" or s[18] != "-" or s[23] != "-":
        return False
    try:
        int(s.replace("-", ""), 16)
    except ValueError:
        return False
    return True


async def _drive(app, headers: dict | None = None, path: str = "/u"):
    """Drive an ASGI app via httpx.ASGITransport (sibling pattern)."""
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test"
    ) as client:
        return await client.get(path, headers=headers or {})


def _build_app(cli):
    """Build a tiny Starlette app whose handler echoes the per-request trace_id.

    The handler reads :data:`abtest_ctx_var` directly (the seam already used
    by ``test_fastapi.py``); the middleware must have set the ctx with the
    correct trace_id before downstream runs.
    """
    from starlette.applications import Starlette
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

    async def user_provider(request):
        # Always return a fixed uid so the test focuses on trace_id and not
        # user-provider behaviour. attrs is empty — middleware decides
        # trace_id from request headers, not from the user provider.
        return "u1", {}

    async def handler(request):
        ctx = abtest_ctx_var.get()
        return JSONResponse(
            {
                "has_ctx": ctx is not None,
                "trace_id": ctx.trace_id if ctx else None,
            }
        )

    app = Starlette(routes=[Route("/u", handler)])
    app.add_middleware(AbtestMiddleware, sdk=cli, user_provider=user_provider)
    return app


async def test_middleware_uses_x_trace_id_header(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """``X-Trace-Id`` wins over generation when present and non-empty."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        app = _build_app(cli)
        r = await _drive(app, headers={"X-Trace-Id": "trace-A"})
        assert r.status_code == 200
        body = r.json()
        assert body["has_ctx"] is True
        assert body["trace_id"] == "trace-A"
    finally:
        await cli.aclose()


async def test_middleware_prefers_x_trace_id_over_x_request_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """When both headers are present, ``X-Trace-Id`` wins (decision #3 ordering)."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        app = _build_app(cli)
        r = await _drive(
            app,
            headers={"X-Trace-Id": "trace-A", "X-Request-Id": "req-B"},
        )
        assert r.status_code == 200
        assert r.json()["trace_id"] == "trace-A"
    finally:
        await cli.aclose()


async def test_middleware_falls_back_to_x_request_id(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Without ``X-Trace-Id``, ``X-Request-Id`` is reused."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        app = _build_app(cli)
        r = await _drive(app, headers={"X-Request-Id": "req-B"})
        assert r.status_code == 200
        assert r.json()["trace_id"] == "req-B"
    finally:
        await cli.aclose()


async def test_middleware_generates_uuid_when_no_headers(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Neither header ⇒ middleware generates a canonical 36-char dashed UUID."""
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        app = _build_app(cli)
        r = await _drive(app)  # no headers
        assert r.status_code == 200
        sent = r.json()["trace_id"]
        assert sent, "middleware must auto-fill trace_id"
        assert _looks_like_uuid_v4(sent)
    finally:
        await cli.aclose()


async def test_middleware_treats_empty_header_as_missing(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Empty/whitespace-only ``X-Trace-Id`` must not be propagated verbatim.

    The design (decision #3) says "reuse if header is present". An empty
    header is operationally indistinguishable from "no header" — propagating
    "" would produce a request whose every log line shows ``trace_id=""``
    which defeats the purpose. Middleware should fall through to
    ``X-Request-Id`` or UUID generation.
    """
    cfg_addr, ab_addr = running_servers
    cfg_servicer.set_pull_snapshot(make_snapshot("ns1", 1, 1))
    cli = await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            pull_interval=10.0,
            pull_retries=1,
        )
    )
    try:
        app = _build_app(cli)
        r = await _drive(
            app,
            headers={"X-Trace-Id": "", "X-Request-Id": "req-C"},
        )
        assert r.status_code == 200
        # Either falls through to X-Request-Id verbatim, or generates a UUID.
        # Both behaviours are acceptable per design — we only forbid
        # propagating the empty string itself.
        sent = r.json()["trace_id"]
        assert sent, "empty X-Trace-Id must not propagate as empty trace_id"
        assert sent == "req-C" or _looks_like_uuid_v4(sent)
    finally:
        await cli.aclose()
