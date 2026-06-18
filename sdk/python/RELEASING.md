# Releasing the Tipsy AB-config Python SDK

This SDK is distributed as a public Git-installable release. Each release
ships a sdist + wheel built on CI. Business consumers install via a
`git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@<tag>#subdirectory=sdk/python`
URL pinned to a tag, or via the two-step Release-asset download path.

> **Migration note.** Old installs using
> `git+https://${GH_PAT}@github.com/Lightspeed-Intelligence/tipsy-ab-config.git@python-sdk/v0.2.0#subdirectory=sdk/python`
> continue to work via the legacy tag preserved on the ab-config repo,
> but new releases live here. Do not rewrite that legacy tag's git
> history.

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
     `python-sdk/vexperimental`-style typos), runs the build job and
     publishes the artefacts as a GitHub Release.

5. **Notify business consumers**

   - Post the new tag and the install URL snippet to the relevant
     internal channel. The canonical install line is:
     ```text
     pip install "git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/vX.Y.Z#subdirectory=sdk/python"
     ```
   - Bump the SDK pin in tipsy-studio's `requirements.txt` /
     `pyproject.toml` in a follow-up PR.

## Rollback

- **Soft rollback** (recommended): publish a patch release `X.Y.(Z+1)`
  reverting the breaking change. Consumers move forward; the broken
  release stays on the Releases page for traceability.
- **Hard rollback**: delete the GitHub Release **but keep the tag** — pip
  consumers that already pinned `X.Y.Z` via `git+https` still resolve
  (tags are git refs; deleting the Release only removes the asset bundle).
  Then publish `X.Y.(Z+1)`.

## TODO

- Replace the workflow's `body_path: sdk/python/CHANGELOG.md` (which posts
  the full changelog on every release) with per-version extraction. Use
  `dawidd6/action-get-tag` or a `sed`/`awk` script that slices out the
  `## [X.Y.Z]` block for the tag being released.
