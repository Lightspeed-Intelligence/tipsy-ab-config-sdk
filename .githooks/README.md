# Git hooks

This directory holds the repo's git hooks. Enable them once with:

```bash
git config core.hooksPath .githooks
```

(CI sets `CI` / `GITHUB_ACTIONS`, which the hooks detect and skip, so this is
safe to enable locally without affecting automation.)

## `pre-commit`

A **fast, staged-only** lint gate. It checks just the files staged for the
current commit (`git diff --cached`), so unrelated in-tree violations never
block or slow down an isolated commit.

| Files | Tool | Check |
| --- | --- | --- |
| `*.go` | `gofmt` | `gofmt -l` (lists unformatted files) |
| `*.py` | `ruff` | `ruff check` (only when installed) |

Java (`*.java`) is intentionally not checked here — the Maven build carries no
checkstyle/spotless config, so there is no file-level formatter to run. Any
violation aborts the commit and prints the exact `fix:` command to run. A tool
that isn't installed is skipped silently, and CI short-circuits the whole hook.
The heavier project-wide checks (`go vet`) run at push time — see `pre-push`.

## `pre-push`

Two responsibilities, in order:

1. **Lint gate.** Runs `gofmt -l` + `go vet` per workspace module (the module
   list is parsed from the committed `go.work`, mirroring
   `.github/workflows/go-test.yml`), plus `ruff check sdk/python/` when ruff is
   installed. Any failure aborts the push.

2. **Post-push CI watch (self-wrapping).** git has no native `post-push` hook,
   so this one emulates it. After lint passes it performs the *real* push
   itself (the "inner" push, marked with the `GIT_POSTPUSH` env var so the
   re-entered hook short-circuits), then blocks on the remote PR's CI via
   `scripts/check-pr-ci.sh` and prints a ✓/✗ report. Once the agentic PR-review
   bot posts feedback or CI fails, the hook exits non-zero so a local Claude
   Code loop can detect it and address the feedback automatically.

### ⚠️ The `git push` exit code is NOT trustworthy with this hook enabled

Because the hook performs the real push *inside* itself, the **outer**
`git push` you typed is guaranteed to fail afterwards — either:

- git's atomic ref protection rejects it (`remote rejected` / `cannot lock
  ref`), because the inner push already advanced the ref, or
- the connection drops during the long CI watch (`Connection closed` /
  SIGPIPE).

**Both are expected and harmless** — the push already succeeded via the inner
push. Do **not** interpret the outer push's non-zero exit code, the
`remote rejected` line, or the `connection closed` line as a real failure.

**Rely on the hook's own report instead:**

- `post-push: ✔ push succeeded` — the branch was pushed.
- The boxed `✓ all CI checks passed` / `✗ CI FAILED` block — the real CI
  verdict, printed once CI finishes. Failing jobs are listed with links.
- The review-status reminder line — check it before merging.

If you script around `git push` (CI, automation, `set -e` wrappers), either
disable this hook in that context or ignore the push exit code and parse the
report.

### Hook exit-code policy (after push has succeeded)

| Situation | Hook exit | Why |
| --- | --- | --- |
| Lint failed before pushing | `1` | Push aborted; nothing reached the remote. |
| Inner push failed | non-zero | Real push failure; surface verbatim. |
| CI passed, no new review activity, no unresolved threads | `0` | Terminal state — nothing left to do. |
| CI passed, but new review activity or unresolved threads exist | `1` | The hook printed a Claude-targeted instruction block on stderr describing exactly what to read and fix; **returning non-zero forces that stderr to be read by tooling that only inspects exit status** (agent loops, `set -e` drivers). |
| CI failed | `1` | Failing jobs were listed on stderr. |
| `gh` / `jq` unavailable | non-fatal pass-through | Push happened; CI watch was skipped. |

The "review activity remains" path uses an internal `75` from
`check-pr-ci.sh` so it can be told apart from a CI failure (`1`); the
caller hook collapses both to `1` for the final exit status.

### Configuration (env vars)

CI registration can lag a few seconds after a fresh push, so
`scripts/check-pr-ci.sh` polls until at least one check appears. Tune the poll
with:

| Env var | Default | Meaning |
| --- | --- | --- |
| `PINE_PREPUSH_CHECK_POLL_TRIES` | `24` | Max poll attempts before giving up. |
| `PINE_PREPUSH_CHECK_POLL_INTERVAL` | `5` | Seconds between attempts. |

### Requirements

- `gh` (GitHub CLI), authenticated — used to resolve the PR and watch checks.
  Absent → CI watch is skipped (non-fatal; the push still happens).
- `jq` — used to parse the check list. Absent → the script fails safe (treats
  it as a CI failure) rather than risking a false green.

### Refspec handling

The inner push mirrors the exact refspecs git hands the hook on stdin, so
force-pushes (`--force-with-lease` is applied automatically for
non-fast-forward updates), tag pushes, multi-ref pushes (`git push --all`) and
ref deletions (`git push origin :branch`) all work. Deletion-only pushes skip
the CI watch entirely.
