#!/bin/bash
# test-hooks.sh — hermetic regression tests for the git hooks + CI watcher.
#
# Everything runs in a throwaway $TMP sandbox: a fake `gh` (and, where needed,
# a fake `jq` shim is NOT used — we require the real jq, matching runtime) on
# PATH drives check-pr-ci.sh through its state matrix, and a real local bare
# remote exercises the pre-push self-wrap over fast-forward / non-fast-forward /
# multi-ref / force scenarios. No network, no real GitHub, no mutation of the
# working repo.
#
# Usage: bash scripts/test-hooks.sh   (exit 0 = all pass, 1 = a case failed)
# Explicitly DISABLE errexit. Many cases below intentionally run commands that
# exit non-zero (a rejected push, a blocked commit) as part of setup or as the
# thing under test; each is checked explicitly via its captured rc. Under `set
# -e` — which GitHub Actions applies to `run:` steps by default (`bash -e`) —
# the first such non-zero would abort the whole script mid-setup and produce
# spurious failures. `-u`/pipefail stay on.
set +e
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PRE_PUSH="${REPO_ROOT}/.githooks/pre-push"
PRE_COMMIT="${REPO_ROOT}/.githooks/pre-commit"
CHECK_CI="${REPO_ROOT}/scripts/check-pr-ci.sh"

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
command -v git >/dev/null 2>&1 || { echo "SKIP: git not installed"; exit 0; }

pass=0 fail=0
ok()   { pass=$((pass+1)); echo "  ✓ $1"; }
bad()  { fail=$((fail+1)); echo "  ✗ $1"; }
check(){ # check <desc> <actual> <expected>
  if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1 (got '$2', want '$3')"; fi
}

SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT

# ── fake gh factory ─────────────────────────────────────────────────────────
# Writes a $1/bin/gh whose `pr checks` output is driven by a JSON file the
# individual test points at via GH_CHECKS_JSON. `--watch` just exits per
# GH_WATCH_RC. Everything else returns benign stubs so check-pr-ci.sh proceeds.
make_fake_gh() {
  local dir="$1"; mkdir -p "$dir/bin"
  cat > "$dir/bin/gh" <<'GH'
#!/bin/bash
args="$*"
case "$args" in
  *"pr view"*"number"*)        echo "${GH_PR_NUMBER:-42}" ;;
  *"pr checks"*"--watch"*)     exit "${GH_WATCH_RC:-0}" ;;
  *"pr checks"*"--json state"*) jq 'length' "${GH_CHECKS_JSON:-/dev/null}" 2>/dev/null || echo 0 ;;
  *"pr checks"*"--json"*)      cat "${GH_CHECKS_JSON:-/dev/null}" 2>/dev/null || echo "[]" ;;
  *"pr view"*"reviewDecision"*) echo "${GH_REVIEW_DECISION:-APPROVED}" ;;
  *"repo view"*"owner"*)       echo "acme" ;;
  *"repo view"*"name"*)        echo "repo" ;;
  *"api graphql"*)             echo '{"data":{"repository":{"pullRequest":{"comments":{"nodes":[]},"reviews":{"nodes":[]},"reviewThreads":{"nodes":[]}}}}}' ;;
  *)                           echo "" ;;
esac
GH
  chmod +x "$dir/bin/gh"
}

run_check_ci() { # run_check_ci <checks_json_literal> <watch_rc> ; echo rc
  local d; d="$(mktemp -d "$SANDBOX/ci.XXXX")"; make_fake_gh "$d"
  printf '%s' "$1" > "$d/checks.json"
  ( cd "$d" && git init -q r && cd r && git commit -qm init --allow-empty \
    && PATH="$d/bin:$PATH" GH_CHECKS_JSON="$d/checks.json" GH_WATCH_RC="$2" \
       PINE_PREPUSH_CHECK_POLL_TRIES=2 PINE_PREPUSH_CHECK_POLL_INTERVAL=1 \
       bash "$CHECK_CI" >/dev/null 2>&1; echo $? )
}

echo "── check-pr-ci.sh state matrix ──"
check "empty check set → fail"        "$(run_check_ci '[]' 0)" 1
check "all pass → success"            "$(run_check_ci '[{"name":"t","state":"SUCCESS","bucket":"pass","link":""}]' 0)" 0
check "a failing check → fail"        "$(run_check_ci '[{"name":"t","state":"FAILURE","bucket":"fail","link":"x"}]' 1)" 1
check "pending survives watch → fail" "$(run_check_ci '[{"name":"t","state":"IN_PROGRESS","bucket":"pending","link":""}]' 8)" 1
check "pass+pending mix → fail"       "$(run_check_ci '[{"name":"a","state":"SUCCESS","bucket":"pass","link":""},{"name":"b","state":"IN_PROGRESS","bucket":"pending","link":""}]' 8)" 1
check "pass+skipping → success"       "$(run_check_ci '[{"name":"a","state":"SUCCESS","bucket":"pass","link":""},{"name":"b","state":"SKIPPED","bucket":"skipping","link":""}]' 0)" 0

echo "── pre-push self-wrap (real local bare remote) ──"
# Build a work repo whose pre-push we exercise. check-pr-ci is stubbed to exit 2
# (no PR) so we test the PUSH mechanics, not the CI watch.
setup_pp() {
  local d; d="$(mktemp -d "$SANDBOX/pp.XXXX")"
  git init -q --bare "$d/remote.git"
  git init -q "$d/work"
  ( cd "$d/work"
    git config user.email t@t; git config user.name t; git branch -m main
    mkdir -p .githooks scripts
    cp "$PRE_PUSH" .githooks/pre-push
    printf '#!/bin/bash\nexit 2\n' > scripts/check-pr-ci.sh
    chmod +x .githooks/pre-push scripts/check-pr-ci.sh
    git config core.hooksPath .githooks
    git remote add origin ../remote.git
    echo A > f; git add -A; git commit -qm A ) >/dev/null 2>&1
  echo "$d"
}
rsha() { git --git-dir="$1/remote.git" rev-parse "${2:-main}" 2>/dev/null; }

# fast-forward push updates the remote via the inner push
d="$(setup_pp)"
( cd "$d/work" && git push origin main ) >/dev/null 2>&1
check "ff push updates remote" "$(rsha "$d")" "$(cd "$d/work" && git rev-parse main)"

# Create a divergence: remote has A,B; local has A,C (non-fast-forward).
d="$(setup_pp)"
( cd "$d/work" && git push origin main >/dev/null 2>&1
  echo B >> f; git add -A; git commit -qm B
  git -c core.hooksPath=/dev/null push origin main -q
  git reset -q --hard HEAD~1; echo C > f; git add -A; git commit -qm C ) >/dev/null 2>&1
before="$(rsha "$d")"

# non-fast-forward WITHOUT force intent: remote must be untouched. (A plain
# `git push` is pre-rejected by git before the hook even runs; the hook adds no
# force either. Both layers agree the remote stays put.)
( cd "$d/work" && git push origin main ) >/dev/null 2>&1
check "non-ff plain push leaves remote intact" "$(rsha "$d")" "$before"

# WITH intent: real forced pushes use `git push --force`, which lets the hook
# run and receive the refspec on stdin; GIT_POSTPUSH_FORCE=1 then opts the inner
# push into --force-with-lease. Assert the remote advances to local HEAD.
( cd "$d/work" && GIT_POSTPUSH_FORCE=1 git push --force origin main ) >/dev/null 2>&1
check "non-ff force=1 rewrites remote" "$(rsha "$d")" "$(cd "$d/work" && git rev-parse main)"

# Direct hook-level check of the force-decision logic, independent of git's
# own pre-rejection: feed the pre-push stdin protocol line by hand for a
# non-fast-forward update and confirm the inner push forces only when opted in.
d2="$(setup_pp)"
( cd "$d2/work" && git push origin main >/dev/null 2>&1
  echo B >> f; git add -A; git commit -qm B
  git -c core.hooksPath=/dev/null push origin main -q
  git reset -q --hard HEAD~1; echo C > f; git add -A; git commit -qm C ) >/dev/null 2>&1
lsha="$(cd "$d2/work" && git rev-parse main)"; rem="$(rsha "$d2")"
# opted OUT → hook's inner push is plain, remote rejects it, remote unchanged
( cd "$d2/work" && printf 'refs/heads/main %s refs/heads/main %s\n' "$lsha" "$rem" \
    | bash .githooks/pre-push origin ) >/dev/null 2>&1
check "hook stdin: no force → remote intact" "$(rsha "$d2")" "$rem"
# opted IN → hook forces, remote advances to local
( cd "$d2/work" && printf 'refs/heads/main %s refs/heads/main %s\n' "$lsha" "$rem" \
    | GIT_POSTPUSH_FORCE=1 bash .githooks/pre-push origin ) >/dev/null 2>&1
check "hook stdin: force=1 → remote advances" "$(rsha "$d2")" "$lsha"

# Multi-ref push → inner push must add --atomic (preserve all-or-nothing). We
# capture the inner push argv with a `git` shim on PATH: it logs args when the
# GIT_POSTPUSH marker is set (the inner call) and otherwise delegates to real
# git. The hook only reaches the push line for a fast-forward, so build two
# fast-forward refs and feed both stdin lines.
d3="$(setup_pp)"
realgit="$(command -v git)"
shimdir="$d3/shim"; mkdir -p "$shimdir"
cat > "$shimdir/git" <<SHIM
#!/bin/bash
if [ -n "\${GIT_POSTPUSH:-}" ] && [ "\$1" = push ]; then
  echo "\$*" >> "$d3/inner-argv.log"
fi
exec "$realgit" "\$@"
SHIM
chmod +x "$shimdir/git"
( cd "$d3/work"
  git push origin main >/dev/null 2>&1
  git branch feature2 >/dev/null 2>&1
  m="$(git rev-parse main)"; f2="$(git rev-parse feature2)"
  z=0000000000000000000000000000000000000000
  printf 'refs/heads/main %s refs/heads/main %s\nrefs/heads/feature2 %s refs/heads/feature2 %s\n' \
    "$m" "$m" "$f2" "$z" | PATH="$shimdir:$PATH" bash .githooks/pre-push origin ) >/dev/null 2>&1
if grep -q -- '--atomic' "$d3/inner-argv.log" 2>/dev/null; then
  ok "multi-ref inner push uses --atomic"
else
  bad "multi-ref inner push uses --atomic (argv: $(cat "$d3/inner-argv.log" 2>/dev/null))"
fi

echo "── pre-commit staged-only (index blob, not worktree) ──"
setup_pc() {
  local d; d="$(mktemp -d "$SANDBOX/pc.XXXX")"
  git init -q "$d/r"
  ( cd "$d/r" && git config user.email t@t && git config user.name t
    cp "$PRE_COMMIT" .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit
    git commit -qm init --allow-empty ) >/dev/null 2>&1
  echo "$d/r"
}
# staged content bad-formatted, worktree fixed → must BLOCK (judge the index)
r="$(setup_pc)"
( cd "$r" && printf 'package x\nfunc  Bad( ){}\n' > f.go && git add f.go
  printf 'package x\n\nfunc Bad() {}\n' > f.go ) >/dev/null 2>&1
rc="$(cd "$r" && git commit -m x >/dev/null 2>&1; echo $?)"
check "staged bad / worktree good → blocked" "$rc" 1
# staged content well-formatted, worktree dirtied → must PASS
r="$(setup_pc)"
( cd "$r" && printf 'package x\n\nfunc Good() {}\n' > f.go && git add f.go
  printf 'package x\nfunc  Good( ){}\n' > f.go ) >/dev/null 2>&1
rc="$(cd "$r" && git commit -m x >/dev/null 2>&1; echo $?)"
check "staged good / worktree bad → passes" "$rc" 0

echo ""
echo "hooks tests: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
