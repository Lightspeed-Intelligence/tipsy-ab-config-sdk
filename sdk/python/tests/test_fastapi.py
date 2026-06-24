"""FastAPI/ASGI middleware tests. Skipped if Starlette is not installed."""

from __future__ import annotations

import asyncio
import pytest

from tipsy_ab_config import Config, abtest_ctx_var, init
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

from .conftest import (
    FakeAbtestServicer,
    FakeConfigServicer,
    issue_test_token,
    make_exp_result,
    make_snapshot,
)


starlette = pytest.importorskip("starlette")
httpx = pytest.importorskip("httpx")


async def _wait_calls(
    ab_servicer: FakeAbtestServicer,
    ns: str,
    *,
    before_total: int,
    want: int,
    timeout: float = 2.0,
) -> bool:
    """Wait until ``ns`` has been called ``want`` times since ``before_total``.

    The middleware fires prefetch synchronously but does NOT await the spawned
    task, so the RPC may still be in flight when the HTTP response returns. We
    poll the per-ns counter and also assert the total delta did not exceed
    ``want`` (no extra namespaces requested).
    """
    end = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < end:
        if ab_servicer.calls_by_ns.get(ns, 0) >= want:
            break
        await asyncio.sleep(0.01)
    # Settle window to catch any (erroneous) extra RPC.
    await asyncio.sleep(0.02)
    return (
        ab_servicer.calls_by_ns.get(ns, 0) == want
        and ab_servicer.calls - before_total == want
    )


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


# ----------------------------------------------------------------------
# URL-whitelist prefetch (design D4 / G5): the middleware only warms the
# default namespace for a REAL user ctx whose request path EXACTLY matches a
# configured prefetch path. Empty whitelist / no match / no default ns /
# empty-or-error ctx ⇒ no prefetch RPC (avoids the "every request fires a
# wasted experiment RPC" anti-pattern).
# ----------------------------------------------------------------------


async def _whitelist_app(cli, *, user_provider, prefetch_paths=None, route="/x"):
    """Build a Starlette app + AbtestMiddleware with an optional prefetch whitelist.

    The handler does NOT call get_config: any GetExperimentResult RPC observed
    on the servicer is therefore attributable solely to the middleware prefetch.
    """
    from starlette.applications import Starlette
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

    async def handler(request):
        ctx = abtest_ctx_var.get()
        return JSONResponse({"uid": ctx.user_id if ctx else None})

    app = Starlette(routes=[Route(route, handler), Route("/other", handler)])
    app.add_middleware(
        AbtestMiddleware,
        sdk=cli,
        user_provider=user_provider,
        prefetch_paths=prefetch_paths,
    )
    return app


async def _init_with_default_ns(cfg_servicer, cfg_addr, ab_addr, *, default_ns="ns1"):
    cfg_servicer.set_pull_snapshot(
        make_snapshot("ns1", 1, 1, {"k": (1, {1: "full", 2: "ab"})})
    )
    return await init(
        Config(
            namespaces=["ns1"],
            config_service_addr=cfg_addr,
            abtest_service_addr=ab_addr,
            token=issue_test_token(),
            default_namespace=default_ns,
            pull_interval=10.0,
            pull_retries=1,
        )
    )


async def _ok_provider(request):
    return (request.headers.get("X-User-Id") or "u1"), {}


async def test_middleware_no_prefetch_paths_never_prefetches(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Default config (no prefetch_paths): a request triggers NO prefetch RPC."""
    cfg_addr, ab_addr = running_servers
    cli = await _init_with_default_ns(cfg_servicer, cfg_addr, ab_addr)
    app = await _whitelist_app(cli, user_provider=_ok_provider)
    try:
        before = ab_servicer.calls
        r = await _send_through_app(app, "GET", "/x", headers={"X-User-Id": "u1"})
        assert r.status_code == 200
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_middleware_matched_path_prefetches_default_ns_once(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """prefetch_paths=["/x"], request to /x => exactly 1 prefetch RPC for default ns."""
    cfg_addr, ab_addr = running_servers
    ab_servicer.set_response("ns1", make_exp_result({"k": 2}))
    cli = await _init_with_default_ns(cfg_servicer, cfg_addr, ab_addr)
    app = await _whitelist_app(cli, user_provider=_ok_provider, prefetch_paths=["/x"])
    try:
        before = ab_servicer.calls
        r = await _send_through_app(app, "GET", "/x", headers={"X-User-Id": "u1"})
        assert r.status_code == 200
        # The middleware fired prefetch synchronously, but the spawned task may
        # still be in flight when the response returns; wait for it to land.
        assert await _wait_calls(ab_servicer, "ns1", before_total=before, want=1)
        assert ab_servicer.calls_by_ns.get("ns1", 0) == 1
        req = ab_servicer.last_req
        assert req.namespace == "ns1"
        assert (
            req.experiment_type
            == abtest_pb2.ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION
        )
        assert (
            req.display_type
            == abtest_pb2.ResultDisplayType.RESULT_DISPLAY_TYPE_FLAT_KV
        )
    finally:
        await cli.aclose()


async def test_middleware_unmatched_path_does_not_prefetch(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """A request to a path NOT in the whitelist => no prefetch (exact match only)."""
    cfg_addr, ab_addr = running_servers
    cli = await _init_with_default_ns(cfg_servicer, cfg_addr, ab_addr)
    app = await _whitelist_app(cli, user_provider=_ok_provider, prefetch_paths=["/x"])
    try:
        before = ab_servicer.calls
        r = await _send_through_app(app, "GET", "/other", headers={"X-User-Id": "u1"})
        assert r.status_code == 200
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_middleware_no_user_provider_never_prefetches_matched_path(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """No user_provider => empty ctx => no prefetch even on a matched path."""
    cfg_addr, ab_addr = running_servers
    cli = await _init_with_default_ns(cfg_servicer, cfg_addr, ab_addr)
    app = await _whitelist_app(cli, user_provider=None, prefetch_paths=["/x"])
    try:
        before = ab_servicer.calls
        r = await _send_through_app(app, "GET", "/x")
        assert r.status_code == 200
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_middleware_provider_error_never_prefetches_matched_path(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """A raising user_provider => empty ctx => no prefetch even on a matched path."""
    cfg_addr, ab_addr = running_servers
    cli = await _init_with_default_ns(cfg_servicer, cfg_addr, ab_addr)

    async def bad_provider(request):
        raise RuntimeError("no auth")

    app = await _whitelist_app(cli, user_provider=bad_provider, prefetch_paths=["/x"])
    try:
        before = ab_servicer.calls
        r = await _send_through_app(app, "GET", "/x")
        assert r.status_code == 200
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()


async def test_middleware_matched_path_empty_default_ns_noop(
    cfg_servicer: FakeConfigServicer,
    ab_servicer: FakeAbtestServicer,
    running_servers,
):
    """Default ns empty => matched path is a no-op prefetch (0 RPC)."""
    cfg_addr, ab_addr = running_servers
    # No default_namespace configured (and env unset): nothing to warm.
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
    assert cli.default_namespace == ""
    app = await _whitelist_app(cli, user_provider=_ok_provider, prefetch_paths=["/x"])
    try:
        before = ab_servicer.calls
        r = await _send_through_app(app, "GET", "/x", headers={"X-User-Id": "u1"})
        assert r.status_code == 200
        await asyncio.sleep(0.03)
        assert ab_servicer.calls - before == 0
    finally:
        await cli.aclose()

