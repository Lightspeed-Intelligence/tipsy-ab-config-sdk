"""FastAPI example using the tipsy_ab_config Python SDK.

Run with:

    pip install -e ../[fastapi]
    pip install fastapi uvicorn
    TIPSY_TOKEN=... CONFIG_ADDR=cfg:50051 ABTEST_ADDR=ab:50052 \\
        python -m uvicorn main:app --port 8080

The example exposes:

  GET /static -> demonstrates ``get_config_static`` (no user context).
  GET /user   -> demonstrates ``get_config`` + AbtestMiddleware injection.

Both routes resolve ``rerank.threshold`` in the ``tipsy-chat`` namespace.
"""

from __future__ import annotations

import os
from contextlib import asynccontextmanager
from typing import Optional, Tuple

from fastapi import FastAPI, Request

from tipsy_ab_config import Client, Config, init, abtest_ctx_var
from tipsy_ab_config.fastapi_middleware import AbtestMiddleware


_sdk: Optional[Client] = None


async def _user_provider(request: Request) -> Tuple[str, dict]:
    uid = request.headers.get("X-User-Id", "anonymous")
    attrs = {
        "country": request.headers.get("X-Country", ""),
    }
    return uid, attrs


@asynccontextmanager
async def lifespan(app: FastAPI):
    global _sdk
    namespaces = [
        ns.strip()
        for ns in os.environ.get("NAMESPACES", "tipsy-chat").split(",")
        if ns.strip()
    ]
    cfg = Config(
        namespaces=namespaces,
        config_service_addr=os.environ.get("CONFIG_ADDR", "localhost:50051"),
        abtest_service_addr=os.environ.get("ABTEST_ADDR", "localhost:50051"),
        token=os.environ.get("TIPSY_TOKEN", ""),
    )
    _sdk = await init(cfg)
    app.state.sdk = _sdk
    # add_middleware can't be called once the app is running; instead the
    # middleware is registered at module level via ``app.add_middleware``
    # below after the FastAPI() factory line. We do it inside lifespan only
    # for symmetry with manual cases. Real apps add it before starting.
    try:
        yield
    finally:
        await _sdk.aclose()


app = FastAPI(lifespan=lifespan)


@app.middleware("http")
async def _ctx_middleware(request: Request, call_next):
    # Equivalent to AbtestMiddleware; we inline here because lifespan-time
    # add_middleware isn't allowed. For a more idiomatic setup pre-create
    # the SDK and register AbtestMiddleware before ``app = FastAPI(...)``.
    if _sdk is None:
        return await call_next(request)
    # trace_id (sdk-trace-id §5): reuse the inbound X-Trace-Id / X-Request-Id
    # header if present, otherwise let new_abtest_context generate a fresh
    # UUID v4. This demonstrates the explicit trace_id kwarg; the
    # AbtestMiddleware class does the same automatically.
    trace_id = (
        request.headers.get("X-Trace-Id")
        or request.headers.get("X-Request-Id")
        or None
    )
    try:
        uid, attrs = await _user_provider(request)
        ctx = _sdk.new_abtest_context(uid, attrs, trace_id=trace_id)
    except Exception:
        ctx = _sdk.empty_abtest_context()
    token = abtest_ctx_var.set(ctx)
    try:
        return await call_next(request)
    finally:
        abtest_ctx_var.reset(token)


@app.get("/static")
async def static_handler():
    sdk: Client = app.state.sdk  # type: ignore[attr-defined]
    value = sdk.get_config_static("tipsy-chat", "rerank.threshold", "0.5")
    return {"key": "rerank.threshold", "value": value}


@app.get("/user")
async def user_handler():
    sdk: Client = app.state.sdk  # type: ignore[attr-defined]
    ctx = abtest_ctx_var.get()
    value = await sdk.get_config(ctx, "tipsy-chat", "rerank.threshold", "0.5")
    return {
        "uid": ctx.user_id if ctx is not None else "",
        "trace_id": ctx.trace_id if ctx is not None else "",
        "key": "rerank.threshold",
        "value": value,
    }
