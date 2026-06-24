# tipsy-ab-config (Python SDK)

Tipsy AB-config Python SDK: local config cache + abtest Compute client for the
Tipsy AB-config server.

## What it is

`tipsy-ab-config` is the Python client SDK for the Tipsy AB-config server.
It maintains a process-local config cache (populated by a startup `PullAll`
and a long-lived server-streaming `Subscribe`) and resolves abtest hits via
the server's `AbtestService.GetExperimentResult`. All gRPC traffic is JWT
authenticated.

This package mirrors the Go SDK 1:1. The SDK never talks to the database —
everything goes through `ConfigService.PullAll`/`Subscribe` plus
`AbtestService.GetExperimentResult`.

## Install

> **Releases:** browse all published versions, changelog entries, and
> downloadable wheel/sdist assets at
> https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases
> (look for tags prefixed `python-sdk/v`).

The SDK is distributed from the **public** `tipsy-ab-config-sdk` repository
— `pip install` does not require any credential.

### Find the latest version

The snippets below use the placeholder `python-sdk/vX.Y.Z` — substitute
the latest stable tag at install time. The canonical lookups are:

- **GitHub Releases page** (browser):
  <https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases>
  — top-most non-prerelease entry whose tag begins with `python-sdk/v`.
- **CHANGELOG**: [`sdk/python/CHANGELOG.md`](./CHANGELOG.md) — top
  `[X.Y.Z] - <date>` section.
- **Shell one-liner** (needs `jq`; no auth required):

  ```bash
  LATEST_TAG=$(curl -s \
    https://api.github.com/repos/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases \
    | jq -r '[.[] | select(.prerelease == false) | select(.tag_name | startswith("python-sdk/v")) | .tag_name] | first')
  echo "${LATEST_TAG}"   # e.g. python-sdk/v0.3.0
  ```

  Use `${LATEST_TAG}` inline in the `pip install` commands shown below.
  In CI, exporting a `GITHUB_TOKEN` (or any GitHub PAT) bumps the
  unauthenticated 60-req/hr rate limit to 5000 req/hr — useful but **not
  required** for installation itself.

> Pin the resolved tag in your `requirements.txt` / `pyproject.toml` —
> do **not** ship `python-sdk/vX.Y.Z` literally, and do **not** rely on a
> floating ref (`main`, `HEAD`) in production.

### Consumer onboarding (end-to-end)

If you are a downstream service (e.g. `tipsy-studio`) integrating this SDK
for the first time, follow these steps.

**1. Wire the SDK into your project's dependency list.**

Pick whichever file your project uses. The line is the same in all of
them — install via `git+https` against a published tag:

```text
tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

No credential, no env-var substitution: the repo is public, so both
`pip`/`requirements.txt` and lockfile-style resolvers (`uv`, `poetry`)
fetch the tag directly.

**2. Verify the install.**

```bash
# In your project's venv:
pip install -r requirements.txt
python -c "import tipsy_ab_config as p; print('SDK version:', p.__version__)"
# Expected: SDK version: <the X.Y.Z you pinned in requirements.txt>
```

If this fails, see `## Troubleshooting` below.

**3. Wire the SDK into your application code.**

See `## Quickstart` and `## FastAPI integration` below. The SDK is
fully async; do not call it from sync code paths without first wrapping
in a runtime.

**4. (Recommended) Pin the tag in CI.**

Pin your CI matrix to the exact tag you've validated (e.g.
`python-sdk/v0.3.0`). When a new SDK version ships, review its
`CHANGELOG.md`, then update the tag in `requirements.txt` and re-run
your test suite — do not let the tag drift silently.

### 1) Primary — `git+https` one-liner

```text
tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

Or in `requirements.txt`:

```text
tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

Or in `pyproject.toml`:

```toml
[project]
dependencies = [
    "tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python",
]
```

Tags use the scheme `python-sdk/v<semver>` (e.g. `python-sdk/vX.Y.Z`).

> **Prerelease tags** (`python-sdk/vX.Y.Zrc1`, `…alpha1`, `…beta1`,
> `…-suffix`) exist on GitHub Releases too and are marked as prereleases.
> Do not pin production code to them — they are dry-runs for validating
> the release pipeline.

### 2) Alternative — Release asset download

For air-gapped / vendoring scenarios where you want to mirror the wheel
into an internal artifact store:

```bash
# 0. Resolve the latest tag (see "Find the latest version" above) and
#    URL-encode it (the slash becomes %2F):
LATEST_TAG=$(curl -s \
  https://api.github.com/repos/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases \
  | jq -r '[.[] | select(.prerelease == false) | select(.tag_name | startswith("python-sdk/v")) | .tag_name] | first')
TAG_PATH="${LATEST_TAG/\//%2F}"   # e.g. python-sdk%2Fv0.3.0

# 1. Download the wheel directly from the public Release page:
WHEEL_URL=$(curl -s \
  "https://api.github.com/repos/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases/tags/${TAG_PATH}" \
  | jq -r '.assets[] | select(.name | endswith(".whl")) | .browser_download_url')
curl -L -o tipsy_ab_config.whl "${WHEEL_URL}"

# 2. Install:
pip install tipsy_ab_config.whl
```

Pin `python-sdk/vX.Y.Z` in the URL (replace with the latest tag from
"Find the latest version" above); bump it when you upgrade.

### Extras

```text
tipsy-ab-config[fastapi] @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

Available extras:
- `fastapi` — pulls `starlette` for the FastAPI middleware.
- `http` — pulls `httpx` for the HTTP transport (use when gRPC is impractical).

### tipsy-studio sample (`requirements.txt`)

```text
# Pin the SDK to a published tag; bump the tag to upgrade.
tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python
```

### Legacy install (deprecated, for back-compat)

Pre-v0.3.0 consumers installed from the private `tipsy-ab-config` monorepo:

```text
tipsy-ab-config @ git+https://${GH_PAT}@github.com/Lightspeed-Intelligence/tipsy-ab-config.git@python-sdk/v0.2.0#subdirectory=sdk/python
```

That URL continues to resolve via the preserved `python-sdk/v0.2.0` tag,
but new releases (v0.3.0+) ship from `tipsy-ab-config-sdk` with no PAT —
migrate to the public install form above at your next SDK bump.

## Quickstart

```python
import asyncio
from tipsy_ab_config import init

async def main():
    client = await init(
        endpoint="abconfig.internal:8443",
        project="my-project",
        token="<bearer-jwt>",
    )

    # Pure cache read — no abtest call.
    cfg = client.get_config_static("feature.flags")

    # AbtestContext-aware lookup (resolves abtest hits via the server).
    # `new_abtest_context` is a PURE create: it issues NO RPC at construction.
    # The first `get_config` for a namespace lazily fetches + memoises that
    # namespace's result (at most one GetExperimentResult per namespace per
    # request). Pass `trace_id=` to reuse an upstream trace; omit or pass an
    # empty string to have the SDK auto-generate a UUID v4 for this request.
    ctx = client.new_abtest_context(
        user_id="u-123",
        user_attrs={"country": "US"},
        trace_id="abc-trace-from-upstream",
    )
    value = await client.get_config(ctx, "feature.flags", "key")

    # Optional: warm a namespace up front (e.g. so it overlaps other I/O).
    # Idempotent + non-blocking; a later get_config reuses the same fetch.
    ctx.prefetch_config_version_flat_kv_for_namespace("feature.flags")

    # `get_experiment_result` accepts the same optional kwarg.
    resp = await client.get_experiment_result(
        namespace="my-project",
        user_info=...,
        trace_id="abc-trace-from-upstream",
    )
```

The `trace_id=` kwarg is accepted by `new_abtest_context`,
`abtest_scope`, and `get_experiment_result`. Empty / `None` means
"SDK generates a fresh UUID v4". The id is propagated end-to-end:
into the proto `trace_id` field and the server-side computation logs.
The server enforces a 128-char soft cap (oversize input is truncated with
a one-shot WARN).

> **About the trace_id semantics.** `trace_id` is a correlation token
> — it ties one logical request together across SDK logs, server-side
> experiment-result logs, and upcoming experiment-result reporting. The
> platform does **not** prescribe any particular ID format; pass
> whatever ID already identifies "one logical request" in your system.
> Examples: a search/recommendation service can pass its own
> `request_id`; an OpenTelemetry-enabled service can pass its OTel
> trace id; a service with its own internal tracing system can pass
> that system's trace id. Pass `None` / omit the kwarg when there is
> no upstream id — the SDK / server will fill a UUID v4.

See `example/` for a fully runnable script.

## FastAPI integration

```python
from fastapi import FastAPI
from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

app = FastAPI()
app.add_middleware(AbtestMiddleware, client=client, default_user_extractor=...)
```

The middleware binds an `AbtestContext` into `contextvars` per request, so
deep call sites can call `client.get_config(...)` without threading the
context through.

The middleware never auto-prefetches: building the context issues no RPC, and
the first `get_config` for a namespace pays the lazy-fetch latency. To warm the
default namespace for selected entry-point routes, opt in with an exact-match
URL whitelist (`prefetch_paths`); requests whose path is not in the whitelist
(or any path when the list is empty) are never prefetched, so handlers that
never call `get_config` do not fire wasted experiment RPCs:

```python
app.add_middleware(
    AbtestMiddleware, sdk=client, user_provider=...,
    prefetch_paths=["/feed", "/recommend"],
)
```

Trace propagation is built in: the middleware reads the inbound
`X-Trace-Id` header first, falling back to `X-Request-Id`, and finally
generates a fresh UUID v4 when both are absent. The chosen id is attached
to the request-scoped `AbtestContext` so all `get_config` /
`get_experiment_result` calls inside the request share the same trace.

## Compatibility

- Python: 3.10, 3.11, 3.12, 3.13 (CI-tested matrix). 3.13 is the primary
  target (matches the main downstream, tipsy-studio).
- gRPC: `grpcio>=1.60,<2`.
- Protobuf runtime: `protobuf>=5.29,<7` — shipped stubs are 5.x major.
- Server: see release notes for compatible server tags.

## Versioning and stability

The SDK follows SemVer:

- `0.x` — minor versions may contain breaking changes; patch versions are
  bug-fix-only. Pin a tag.
- `1.0.0+` — backwards-compatible within a major.

`CHANGELOG.md` records every release.

## Known limitations

- Distributed via public Git installation; **not** yet on PyPI.
- No `setuptools-scm`/dynamic version — `pyproject.toml` and
  `__init__.__version__` are the single source of truth.
- No `.pyi` stubs in the current 0.x line (deferred to a future release).

## Troubleshooting

Symptoms and their fixes, in order of how commonly they show up during
first integration.

### `pip install` fails with `Could not find a version that satisfies the requirement tipsy-ab-config`

`pip` couldn't reach the tag URL. Usually:

- The tag (`python-sdk/vX.Y.Z`) does not yet exist (e.g. you pinned a
  prerelease that was deleted, or pinned a tag that's not published yet).
  Check
  `https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases`.
- Corporate proxy blocking `github.com`. Use the two-step Release-asset
  download (`Install § 2`) into an internal artifact store.
- Transient GitHub outage or network failure; retry.

### `pip install` succeeds but `import tipsy_ab_config` raises `ModuleNotFoundError: No module named 'tipsy'`

You have a stale build of the SDK (probably from before the proto stubs
were rewritten with proper imports). Force a clean reinstall:
`pip install --force-reinstall --no-deps 'tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python'`.

### `RuntimeError: Detected mismatch between protobuf gencode and runtime versions`

Your project pins `protobuf<5.29`. The SDK requires `protobuf>=5.29,<7`
because the shipped stubs were generated by `grpcio-tools==1.66.2`
(protobuf 5.27 generator family). Two options:

- Upgrade your project's `protobuf` pin to `>=5.29,<7`. Recommended.
- If you can't, pin to a future SDK release rebuilt against an older
  generator — file an issue.

### `grpcio` wheel missing on macOS arm64 + Python 3.13

Occasionally `grpcio` lags on a new Python release. Fallback to Python
3.12 locally; CI runs on `ubuntu-latest` where this is rarely an issue.

### My CI suddenly broke on a tag bump

The SDK is `0.x`; minor bumps may include breaking changes per SemVer's
0.y semantics. Read `CHANGELOG.md` for the bumped version, then either
update your call sites or pin back to the previous tag.

## License

MIT. See `LICENSE`.

## Contact

- Releases (wheels + sdists + changelog):
  https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases
- Issues:
  https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/issues
- Or contact the Tipsy AB-config team directly.
