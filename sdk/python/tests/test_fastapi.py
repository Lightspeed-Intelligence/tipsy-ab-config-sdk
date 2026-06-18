"""FastAPI/ASGI middleware tests. Skipped if Starlette is not installed."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Config, abtest_ctx_var, init

from .conftest import FakeAbtestServicer, FakeConfigServicer, issue_test_token, make_snapshot


starlette = pytest.importorskip("starlette")
httpx = pytest.importorskip("httpx")


async def _send_through_app(app, method: str, path: str, headers: dict | None = None):
    """Drive an ASGI app via httpx without TestClient (avoids the
    sync-in-async deadlock TestClient triggers when called from an
    already-running event loop)."""
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        return await client.request(method, path, headers=headers or {})


async def test_middleware_attaches_abtest_context(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    from starlette.applications import Starlette
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

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

    async def user_provider(request):
        uid = request.headers.get("X-User-Id") or ""
        return uid, {"country": request.headers.get("X-Country", "")}

    async def handler(request):
        ctx = abtest_ctx_var.get()
        return JSONResponse({"uid": ctx.user_id if ctx else None})

    app = Starlette(routes=[Route("/u", handler)])
    app.add_middleware(AbtestMiddleware, sdk=cli, user_provider=user_provider)

    try:
        r = await _send_through_app(app, "GET", "/u", headers={"X-User-Id": "u1"})
        assert r.status_code == 200
        assert r.json()["uid"] == "u1"
    finally:
        await cli.aclose()


async def test_middleware_no_user_provider_uses_empty_ctx(
    cfg_servicer, ab_servicer, running_servers
):
    from starlette.applications import Starlette
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

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

    async def handler(request):
        ctx = abtest_ctx_var.get()
        return JSONResponse({"has_ctx": ctx is not None, "uid": ctx.user_id if ctx else ""})

    app = Starlette(routes=[Route("/u", handler)])
    app.add_middleware(AbtestMiddleware, sdk=cli, user_provider=None)
    before_calls = ab_servicer.calls
    try:
        r = await _send_through_app(app, "GET", "/u")
        assert r.status_code == 200
        assert r.json()["has_ctx"] is True
        assert r.json()["uid"] == ""
        # No user provider -> empty ctx -> no Compute calls.
        assert ab_servicer.calls == before_calls
    finally:
        await cli.aclose()


async def test_middleware_user_provider_error_falls_back_to_empty(
    cfg_servicer, ab_servicer, running_servers
):
    from starlette.applications import Starlette
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

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

    async def bad_provider(request):
        raise RuntimeError("no auth")

    async def handler(request):
        ctx = abtest_ctx_var.get()
        return JSONResponse({"uid": ctx.user_id if ctx else None})

    app = Starlette(routes=[Route("/u", handler)])
    app.add_middleware(AbtestMiddleware, sdk=cli, user_provider=bad_provider)
    try:
        r = await _send_through_app(app, "GET", "/u")
        assert r.status_code == 200
        assert r.json()["uid"] == ""  # empty ctx UID
    finally:
        await cli.aclose()


async def test_middleware_passthrough_non_http_scope(
    cfg_servicer, ab_servicer, running_servers
):
    """lifespan / websocket scopes must pass through without touching ctx."""
    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

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

    called = {"n": 0}

    async def downstream(scope, receive, send):
        called["n"] += 1

    mw = AbtestMiddleware(downstream, sdk=cli, user_provider=None)
    try:
        # Simulate a lifespan scope — must not touch starlette.requests.
        async def recv():
            return {"type": "lifespan.startup"}

        async def snd(msg):
            return None

        await mw({"type": "lifespan"}, recv, snd)
        assert called["n"] == 1
    finally:
        await cli.aclose()

