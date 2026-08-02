#!/usr/bin/env bash
# Assertion #3 — layer migration preserves group membership, PER-UID on both arms.
#
# For every uid the anchor file states which side it belongs on and which group:
#   E_MIG  <group>            -> must still be in E_mig, in exactly that group
#   SLICED <ho_group> <was>   -> must be absent from E_mig and in exactly that
#                               holdout-opt group (<was> records its pre-migration
#                               E_mig group, kept for diagnostics)
#
# Why per-uid on BOTH arms: an earlier version only compared groups on the "stayed"
# arm and merely required the sliced ones to be in *some* holdout-opt group. That
# left the sliced arm with no expectation — review widened the holdout-opt slot from
# 10% to 99% (stayed=1, sliced=59) and the check still PASSED. "Full accounting"
# only proves no sample was lost; it does not prove the split is correct. Every arm
# needs an expectation that can actually be violated.
#
# All expected values are SERVER-OBSERVED and stored in the anchor file. They must
# never be recomputed by tools/solve: that would derive the expectation from the
# same salt-fallback rule the server itself uses, making the assertion a tautology
# that stays green regardless of the implementation.
#
# Usage:
#   AB_CONFIG_TOKEN=<service token> bash migration_accounting.sh \
#     --anchor anchor.tsv --emig <experiment id> --ho-exp <holdout-opt experiment id>
set -euo pipefail

# Must run under bash (uses process substitution and bash-only string tests).
# Running it as `zsh thisfile` fails in confusing ways, so say so plainly.
if [ -z "${BASH_VERSION:-}" ]; then
  echo "FATAL: run with bash (e.g. 'bash $0 ...'); this script is not zsh-compatible" >&2
  exit 2
fi

BASE="${AB_CONFIG_HTTP_BASE:-http://localhost:8081}"
NS="${AB_CONFIG_NS:-st9_combo}"
ANCHOR=""; EMIG=""; HOEXP=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --anchor) ANCHOR="$2"; shift 2 ;;
    --emig) EMIG="$2"; shift 2 ;;
    --ho-exp) HOEXP="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
: "${AB_CONFIG_TOKEN:?AB_CONFIG_TOKEN required}"
[[ -f "$ANCHOR" ]] || { echo "FATAL: --anchor file not found: $ANCHOR" >&2; exit 2; }
[[ -n "$EMIG" && -n "$HOEXP" ]] || { echo "FATAL: --emig and --ho-exp required" >&2; exit 2; }

stay_ok=0; stay_bad=0; slice_ok=0; slice_bad=0; checks=0

while IFS=$'\t' read -r uid side want_group _was; do
  [[ -n "$uid" ]] || continue
  body=$(curl -s -m 10 -X POST \
    -H "Authorization: Bearer $AB_CONFIG_TOKEN" -H 'content-type: application/json' \
    -d "{\"namespace\":\"$NS\",\"user_id\":\"$uid\",\"experiment_type\":\"EXPERIMENT_TYPE_CONFIG_VERSION\",\"display_type\":\"RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP\"}" \
    "$BASE/api/v1/abtest/experiment_result")
  # IFS must be reset here: the `while IFS=$'\t' read` header leaves IFS as TAB for
  # the whole loop body, so a bare `read a b` would not split this space-separated
  # pair and ho_g would come back empty.
  IFS=' ' read -r emig_g ho_g <<< "$(printf '%s' "$body" | python3 -c "
import json,sys
gs = json.load(sys.stdin).get('groups') or []
e = [g['group_id'] for g in gs if g['experiment_id']=='$EMIG']
h = [g['group_id'] for g in gs if g['experiment_id']=='$HOEXP']
print(e[0] if e else 'NONE', h[0] if h else 'NONE')")"
  checks=$((checks+1))
  case "$side" in
    E_MIG)
      if [[ "$emig_g" == "$want_group" ]]; then
        stay_ok=$((stay_ok+1))
      else
        stay_bad=$((stay_bad+1))
        echo "FAIL stayed  $uid want E_mig group $want_group, got emig=$emig_g ho=$ho_g"
      fi
      ;;
    SLICED)
      if [[ "$emig_g" == "NONE" && "$ho_g" == "$want_group" ]]; then
        slice_ok=$((slice_ok+1))
      else
        slice_bad=$((slice_bad+1))
        echo "FAIL sliced  $uid want holdout-opt group $want_group and no E_mig, got emig=$emig_g ho=$ho_g"
      fi
      ;;
    *)
      echo "FAIL anchor  $uid has unknown side '$side' (expected E_MIG or SLICED)"
      slice_bad=$((slice_bad+1))
      ;;
  esac
done < "$ANCHOR"

echo "----"
echo "checks=$checks stayed_ok=$stay_ok stayed_bad=$stay_bad sliced_ok=$slice_ok sliced_bad=$slice_bad"

rc=0
(( stay_bad == 0 ))  || { echo "FAIL: $stay_bad user(s) not in their recorded E_mig group"; rc=1; }
(( slice_bad == 0 )) || { echo "FAIL: $slice_bad user(s) not in their recorded holdout-opt group"; rc=1; }
(( stay_ok + slice_ok == checks )) || { echo "FAIL: accounting does not close ($stay_ok + $slice_ok != $checks)"; rc=1; }
# Both arms must be non-empty, else the run proves nothing: all-stayed would mean
# the migration sliced no traffic, all-sliced would mean E_mig was never observed.
(( stay_ok > 0 ))  || { echo "FAIL: nobody stayed in E_mig — migration or observation is broken"; rc=1; }
(( slice_ok > 0 )) || { echo "FAIL: nobody was sliced into holdout-opt — no traffic split observed"; rc=1; }

(( rc == 0 )) && echo "PASS per-uid on both arms ($stay_ok stayed + $slice_ok sliced = $checks)"
exit $rc
