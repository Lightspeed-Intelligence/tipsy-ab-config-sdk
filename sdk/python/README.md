# tipsy-ab-config Python SDK

The Python SDK keeps a process-local configuration cache and resolves dynamic
configuration through the Tipsy AB-config service. It supports gRPC and HTTP,
uses Bearer authentication, and never connects directly to platform storage.

The package is asynchronous and requires Python 3.10 or newer.

## Install

Published versions are tagged `python-sdk/vX.Y.Z` in
[GitHub Releases](https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases).
Choose a released tag and pin it; do not deploy from `main` or a floating ref.

```text
tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

Optional extras:

- `http`: installs `httpx`, required when `Config.transport="http"`.
- `fastapi`: installs Starlette for the ASGI middleware.

For example:

```text
tipsy-ab-config[http,fastapi] @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

The repository is public, so installation does not require a GitHub token. The
current package version is declared in `pyproject.toml` and
`tipsy_ab_config.__version__`; release changes are recorded in
[CHANGELOG.md](./CHANGELOG.md).

For an internal artifact mirror, download the wheel or sdist attached to the
matching GitHub Release and install that immutable artifact.

### Deprecated private-repository install

Consumers still pinned to a pre-0.3.0 tag in the former private
`tipsy-ab-config` monorepo must migrate to the public URL above when upgrading.
The old credential-bearing URL is a compatibility path, not an installation
method for current releases.

## Quick start

```python
import asyncio
import os

from tipsy_ab_config import Config, init


async def main() -> None:
    async with await init(Config(
        namespaces=["my-project"],
        config_service_addr="grpcs://config.example.com:443",
        abtest_service_addr="grpcs://abtest.example.com:443",
        token=os.environ["TIPSY_TOKEN"],
        default_namespace="my-project",
    )) as client:
        # Synchronous cache read; no experiment RPC.
        static_value = client.get_config_static(
            "my-project", "feature.enabled", "false"
        )

        # Create one context per logical request and reuse it. Construction
        # performs no RPC; the first dynamic lookup fetches the namespace result.
        ctx = client.new_abtest_context(
            "user-123",
            {"country": "US"},
            trace_id="upstream-request-id",
        )
        value = await client.get_config(
            ctx, "my-project", "feature.enabled", "false"
        )
        all_values = await client.get_all_configs(ctx, "my-project")

        print(static_value, value, all_values)


asyncio.run(main())
```

`new_abtest_context` is synchronous and side-effect free. Within one context,
the SDK memoizes at most one `GetExperimentResult` call per namespace. Call
`ctx.prefetch_config_version_flat_kv_for_namespace(ns)` when selected entry
points should start that request early; it is non-blocking and idempotent.

Call `await client.aclose()` when not using `async with`.

## Configuration

`init` accepts one `Config` object. Important fields are:

| Field | Default | Meaning |
|---|---:|---|
| `namespaces` | required | Namespaces loaded into the local cache. |
| `config_service_addr` | required | gRPC target or HTTP base URL. |
| `abtest_service_addr` | empty | Abtest endpoint; empty disables experiment RPCs and uses full-release values. |
| `token` / `token_provider` | required | Static Bearer token or async provider. The provider takes precedence. See the implementation note below before relying on rotation. |
| `transport` | env or `grpc` | `grpc` or `http`; an empty value reads `TIPSY_SDK_TRANSPORT`. |
| `pull_interval` | 10s | Fallback polling interval. It is the only update path in HTTP mode. |
| `pull_timeout` / `pull_retries` | 5s / 3 | Startup and periodic pull controls. |
| `abtest_timeout` | 1.5s | Per-compute timeout; failure falls back to the full release. |
| `startup_fail_open` | `False` | Continue with an empty cache when startup PullAll fails. |
| `default_namespace` | env | Empty reads `PROJECT_DEFAULT_NAMESPACE`. |
| `tls_root_certificates` | unset | PEM trust roots for a private CA on `grpcs://`. |
| `channel_options` / `channel_factory` | unset | gRPC customization and test injection. |
| `http_client` | unset | Caller-owned `httpx.AsyncClient` for HTTP mode. |

A static token and an async `token_provider` are both accepted. Do not embed
tokens in source or documentation; obtain their value and lifetime from the
deployment owner.

> **Pending human verification — dynamic token rotation.** The intended
> cross-language contract is that a provider can supply rotated tokens during
> the client lifetime. The current Python implementation calls
> `token_provider` while initializing `_TokenCache`, while RPC interceptors and
> HTTP requests subsequently read the cached value; `Client` exposes no public
> refresh operation. Until this is adjudicated or corrected, treat the Python
> provider as initialization-time token acquisition and recreate the client
> before that token expires. Evidence: `tipsy_ab_config/client.py`,
> `_TokenCache.refresh`, `_init_grpc`, `_init_http`, and the auth interceptors.

## Transports and addresses

gRPC is the default transport. It performs startup `PullAll`, maintains a
server-streaming `Subscribe`, and retains periodic PullAll as a safety net.
HTTP posts protojson to `/api/v1/config/pull_all` and
`/api/v1/abtest/experiment_result`; it does not open Subscribe, so update
latency is bounded by `pull_interval`.

In gRPC mode:

| Address | Behavior |
|---|---|
| `host:port`, `grpc://host:port` | Plaintext h2c. |
| `grpcs://host:port` | TLS using normal certificate verification. |
| `dns:///service.namespace.svc.cluster.local:50051` | Native DNS resolver and automatic `round_robin`; intended for a Headless Service returning pod IPs. |
| `unix:`, `passthrough:///`, `xds:///` | Passed to gRPC as native targets. |
| `http://` or `https://` | Rejected; select HTTP transport instead. |

`grpcs://` also supports `authority` and private trust roots for controlled
development environments. Certificate-verification bypasses are not production
settings.

## Resolution semantics

- `get_config_static(ns, key, default)` is a local full-release cache read.
- `get_config(ctx, ns, key, default)` resolves experiment/gray hit, then full
  release, then the caller default.
- `get_all_configs(ctx, ns)` applies the same rules to one immutable namespace
  snapshot and returns a new mutable `dict`; keys with no resolved value are
  omitted and an empty string remains a valid value.
- `get_config_default` and `get_all_configs_default` use the configured project
  default namespace.
- An empty user id, `None`, or `"0"` represents no user identity and bypasses
  experiment lookup, returning full-release values only.
- A context reuses one experiment result per namespace across all lookups.

When the server explicitly reports `has_dynamic_resolution=false`, a single
key—or all keys in a namespace for `get_all_configs`—can use the pure-full fast
path without waiting for an experiment RPC. If the field is absent, the SDK
safely follows the dynamic path. Servers must use `api/gen/go` v0.3.0 or newer
to emit the field; older servers remain functionally correct but do not provide
the optimization.

`trace_id` is an opaque correlation identifier. An omitted or empty value is
replaced with a UUID v4. Reuse an upstream request or trace id when available;
the SDK forwards it to the service.

`get_experiment_result(...)` exposes the raw service response for custom
parameters and group inspection. Values in `config_flat_kv`,
`groups[].params_versions`, and `gray_hits[].key_versions` are global
`config_version` primary-key ids, not per-key `version_no` values.

## FastAPI and ASGI

```python
from fastapi import FastAPI
from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

app = FastAPI()


async def user_provider(request):
    return request.headers.get("X-User-Id", ""), {"country": "US"}


app.add_middleware(
    AbtestMiddleware,
    sdk=client,
    user_provider=user_provider,
    prefetch_paths=["/feed", "/recommend"],
)
```

The middleware stores an `AbtestContext` in a request-scoped `ContextVar`.
Prefetching is opt-in and exact-path matched. Trace selection is
`X-Trace-Id`, then `X-Request-Id`, then a generated UUID.

## Compatibility and limitations

- Python 3.10–3.13.
- `grpcio>=1.66.2,<2`.
- `protobuf>=5.29.1,<7`.
- Distribution is currently through public Git tags and GitHub Release assets,
  not PyPI.
- The package ships `py.typed` but no separate `.pyi` stubs.

## Troubleshooting

- Installation cannot find a version: confirm that the `python-sdk/vX.Y.Z` tag
  exists and that the consumer can reach GitHub. Mirror a release asset where
  direct access is unavailable.
- `No module named 'tipsy'`: remove the stale installation and reinstall from a
  current public tag.
- Protobuf runtime mismatch: remove an incompatible application pin and resolve
  `protobuf>=5.29.1,<7`.
- Generated gRPC code requires a newer runtime: resolve
  `grpcio>=1.66.2,<2` and rebuild the lockfile.
- A minor `0.x` upgrade breaks a caller: review [CHANGELOG.md](./CHANGELOG.md).
  The SDK follows SemVer; pin an exact tag and validate upgrades.

For SDK development and publishing, see [RELEASING.md](./RELEASING.md).
The repository is licensed under [MIT](../../LICENSE).
