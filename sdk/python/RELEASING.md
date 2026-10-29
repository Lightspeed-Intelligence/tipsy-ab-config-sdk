# Releasing the Tipsy AB-config Python SDK

This SDK is distributed through [PyPI](https://pypi.org/project/tipsy-ab-config/)
as `tipsy-ab-config`. Each release ships a sdist + wheel built on CI, attached
to the GitHub Release and uploaded to PyPI by Trusted Publishing (OIDC; no API
token is stored). Business consumers pin `tipsy-ab-config==X.Y.Z`.

The GitHub repository is private, so `git+https://…tipsy-ab-config-sdk.git@<tag>`
requirements need GitHub read access and are not a supported install path.

> **Legacy note.** Pre-0.3.0 tags live on the former `tipsy-ab-config`
> monorepo and are reachable only through its credential-bearing Git URL. Do not
> rewrite that legacy tag's git history.

## One-time setup (already done once per repository)

- pypi.org → Publishing: a trusted publisher for project `tipsy-ab-config`,
  owner `Lightspeed-Intelligence`, repository `tipsy-ab-config-sdk`, workflow
  `python-sdk.yml`, environment `pypi`. Keep at least two PyPI owners with 2FA.
- GitHub → Settings → Environments → `pypi`: deployment tags/branches limited to
  `python-sdk/v*` and `main` (the backfill job runs on `main`).

## Release steps

1. **Prepare**

   - Decide the new version `X.Y.Z` (SemVer).
   - Update three places in lock-step:
     - `sdk/python/pyproject.toml` — `version = "X.Y.Z"`.
     - `sdk/python/tipsy_ab_config/__init__.py` — `__version__ = "X.Y.Z"`.
     - `sdk/python/CHANGELOG.md` — prepend a `## [X.Y.Z] - YYYY-MM-DD`
       section with the changes since the previous tag.
   - Local sanity:
     ```bash
     cd sdk/python
     pytest -q                                  # non-packaging suite
     pytest -m packaging --override-ini="addopts=" -q   # packaging suite
     ```
   - Local build dry-run:
     ```bash
     make python-build
     ```

2. **Open a release PR**

   - PR title: `release(python-sdk): vX.Y.Z`.
   - Diff scope: only `pyproject.toml`, `__init__.py`, `CHANGELOG.md`.
   - Get review approval. Merge to `main`.

3. **Tag and push**

   ```bash
   git checkout main && git pull
   git tag python-sdk/vX.Y.Z
   git push origin python-sdk/vX.Y.Z
   ```

   For a release-candidate / pre-release, use a `-` suffix
   (`python-sdk/vX.Y.Z-rc1`); the CI release job auto-marks `-`, `rc`,
   `alpha`, `beta` tags as GitHub `prerelease` (not "latest").

4. **CI auto-publishes**

   The `python-sdk` workflow:
   - Runs the 3.10–3.13 matrix tests + proto-drift check on push to `main`
     and on PRs.
   - On a `python-sdk/v*` tag with a `.` in it (filters out
     `python-sdk/vexperimental`-style typos), runs the build job, publishes
     the artefacts as a GitHub Release, and uploads the same files to PyPI.

   Verify the upload: `pip download tipsy-ab-config==X.Y.Z --no-deps` and a
   clean-venv `pip install "tipsy-ab-config[http]==X.Y.Z"` that imports.

   **Backfill** (a release whose Release assets exist but which is missing on
   PyPI, e.g. versions released before the PyPI move): run the workflow
   manually from `main` with the tag as input. It downloads the Release's
   original wheel + sdist and uploads them unchanged; an already-present
   version is skipped.

   ```bash
   gh workflow run python-sdk.yml --ref main -f tag=python-sdk/vX.Y.Z
   ```

5. **Notify business consumers**

   - Post the new version and the install line to the relevant internal
     channel. The canonical install line is:
     ```text
     pip install "tipsy-ab-config==X.Y.Z"
     ```
   - Bump the SDK pin in tipsy-studio's `requirements.txt` /
     `pyproject.toml` in a follow-up PR.

## Rollback

- **Soft rollback** (recommended): publish a patch release `X.Y.(Z+1)`
  reverting the breaking change. Consumers move forward; the broken
  release stays on the Releases page for traceability.
- **Hard rollback**: on PyPI, **yank** `X.Y.Z` (never delete). A yanked
  version is skipped by resolvers unless pinned exactly, so existing `==X.Y.Z`
  pins keep installing. Keep the git tag and GitHub Release. Then publish
  `X.Y.(Z+1)`.
- PyPI versions are immutable: a deleted `X.Y.Z` can never be uploaded again,
  so a fix always ships as a new version.

## TODO

- Replace the workflow's `body_path: sdk/python/CHANGELOG.md` (which posts
  the full changelog on every release) with per-version extraction. Use
  `dawidd6/action-get-tag` or a `sed`/`awk` script that slices out the
  `## [X.Y.Z]` block for the tag being released.
