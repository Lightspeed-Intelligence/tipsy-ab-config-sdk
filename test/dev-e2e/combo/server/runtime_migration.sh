#!/usr/bin/env bash
# Assertion #3b — RUNTIME layer migration (走查项 29): migrate-out / migrate-in of a
# plain layer on a RUNNING combo preserves per-uid group membership, and the value
# semantics of held keys switch exactly with the topology.
#
# Relation to assertion #3 (migration_accounting.sh): #3 proves the CREATE-TIME
# migration (migrate_layer_ids at POST /combos). This proves the RUNTIME entry
# points POST /api/v1/combos/{id}/layers/migrate-out and /migrate-in. Both paths
# share UpdateLayerDomain underneath (layer id/salt never change), but "shares the
# implementation" is an argument, not evidence — this script is the evidence.
#
# What one round trip asserts, in order:
#   A  preflight — the layer sits on the combo's Simple Domain; the CREATE-TIME
#      accounting (#3) still holds, so a corrupted starting state fails loudly
#      here instead of masquerading as a migration bug later.
#   B  value-plane setup — move the combo-managed key's live full release to a
#      NEW version so frozen != live (README non-degeneracy #1: while they are
#      equal, "kept the frozen value" is indistinguishable from "took live").
#   C  premig evidence — full per-uid membership snapshot + SDK value matrix.
#   D  migrate-out (Simple Domain → host domain, layer takes FULL host traffic):
#        - IMMEDIATE (no cache wait) admin read: layer.domain_id already the host
#          domain, is_combo_layer still false (走查项 13's authoritative bool).
#        - after the cache window, per-uid on both arms: every STAYED uid is in
#          its recorded E_mig group; every SLICED uid is NOW in E_mig too — in
#          the group the anchor's "was" column recorded from the server BEFORE
#          the create-time migration (same layer id/salt + full traffic = the
#          create-time premig state, so that observation IS the expectation;
#          re-deriving it with the solver would be the tautology README #4 bans)
#          — while KEEPING its holdout-opt group (the comboLayer's own split is
#          untouched by moving a sibling layer out of the Simple Domain).
#        - SDK value matrix: sliced uids' experiment key switches default→group
#          version (they can reach the experiment now); their combo-managed key
#          stays FROZEN (migration must not touch the combo's freeze); simple-
#          world uids keep the live value.
#   E  migrate-in (host → Simple Domain, back to the starting topology):
#        - immediate admin read: domain_id is the Simple Domain again.
#        - ROUND-TRIP INVARIANCE, the strongest claim here: the postback
#          snapshot is BYTE-IDENTICAL to the premig snapshot, all uids, all
#          experiments (not just E_mig — c1/c3zero memberships ride along in the
#          same lines, so cross-combo damage would also break the diff).
#        - SDK value matrix identical to premig again.
#   F  restore the value plane (live release back to the original version).
#
# On any FAIL the script stops immediately, leaving the topology as-is for
# inspection, and prints what needs manual restoration.
#
# Usage:
#   ADMIN_SESSION=<devtoken 100001> AB_CONFIG_TOKEN=<service token> \
#   bash runtime_migration.sh \
#     --anchor anchor.tsv --combo-id <c2mig id> --layer-id <L_mig id> \
#     --emig <E_mig exp id> --ho-exp <c2mig holdout-opt exp id> \
#     --workdir /tmp/rtmig_out [--go-dir ../clients/go] [--skip-value-plane]
#
# --skip-value-plane runs the membership legs only (no release moved, no SDK
# value matrix) — for reruns where the value plane is already being inspected.
set -euo pipefail

if [ -z "${BASH_VERSION:-}" ]; then
  echo "FATAL: run with bash; this script is not zsh-compatible" >&2
  exit 2
fi

BASE="${AB_CONFIG_HTTP_BASE:-http://localhost:8081}"
NS="${AB_CONFIG_NS:-st9_combo}"
# Cache window: the resident topology refreshes every 10s; membership reads
# before that observe the pre-migration split (README trap 10).
SLEEP="${AB_CONFIG_CACHE_SLEEP:-12}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ANCHOR=""; COMBO=""; LAYER=""; EMIG=""; HOEXP=""
WORKDIR=""; GO_DIR="$SCRIPT_DIR/../clients/go"; VALUE_PLANE=1
EXP_KEY_NAME="${AB_CONFIG_EXP_KEY:-exp_key}"
MIG_KEY_NAME="${AB_CONFIG_MIG_KEY:-mig_key}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --anchor) ANCHOR="$2"; shift 2 ;;
    --combo-id) COMBO="$2"; shift 2 ;;
    --layer-id) LAYER="$2"; shift 2 ;;
    --emig) EMIG="$2"; shift 2 ;;
    --ho-exp) HOEXP="$2"; shift 2 ;;
    --workdir) WORKDIR="$2"; shift 2 ;;
    --go-dir) GO_DIR="$2"; shift 2 ;;
    --skip-value-plane) VALUE_PLANE=0; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
: "${ADMIN_SESSION:?ADMIN_SESSION required (devtoken session for the admin API)}"
: "${AB_CONFIG_TOKEN:?AB_CONFIG_TOKEN required (service token for the data plane)}"
[[ -f "$ANCHOR" ]] || { echo "FATAL: --anchor file not found: $ANCHOR" >&2; exit 2; }
[[ -n "$COMBO" && -n "$LAYER" && -n "$EMIG" && -n "$HOEXP" && -n "$WORKDIR" ]] \
  || { echo "FATAL: --combo-id --layer-id --emig --ho-exp --workdir all required" >&2; exit 2; }
mkdir -p "$WORKDIR"

fail() { echo "FAIL: $*" >&2; echo "TOPOLOGY MAY BE MID-MIGRATION — inspect $WORKDIR and restore by hand (migrate-in / release full back)." >&2; exit 1; }

admin_get() { curl -s -m 15 -H "Authorization: Bearer $ADMIN_SESSION" "$BASE$1"; }
admin_post() { # path json → body; dies on non-2xx
  local out code body
  out=$(curl -s -m 30 -w $'\n%{http_code}' -X POST \
    -H "Authorization: Bearer $ADMIN_SESSION" -H 'content-type: application/json' \
    -d "$2" "$BASE$1")
  code="${out##*$'\n'}"; body="${out%$'\n'*}"
  [[ "$code" == 2* ]] || fail "POST $1 → $code: $body"
  printf '%s' "$body"
}

# snapshot <outfile> — per-uid FULL membership (every experiment:group pair the
# data plane reports, sorted for a stable byte-diff), uids from the anchor.
snapshot() {
  local out="$1"; : > "$out"
  while IFS=$'\t' read -r uid _rest; do
    [[ -n "$uid" ]] || continue
    curl -s -m 10 -X POST -H "Authorization: Bearer $AB_CONFIG_TOKEN" -H 'content-type: application/json' \
      -d "{\"namespace\":\"$NS\",\"user_id\":\"$uid\",\"experiment_type\":\"EXPERIMENT_TYPE_CONFIG_VERSION\",\"display_type\":\"RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP\"}" \
      "$BASE/api/v1/abtest/experiment_result" | python3 -c "
import json,sys
gs = json.load(sys.stdin).get('groups') or []
pairs = sorted(f\"{g['experiment_id']}:{g['group_id']}\" for g in gs)
print('$uid\t' + (';'.join(pairs) or 'NONE'))" >> "$out"
  done < "$ANCHOR"
  local n; n=$(wc -l < "$out")
  local want; want=$(wc -l < "$ANCHOR")
  [[ "$n" -eq "$want" ]] || fail "snapshot $out has $n lines, want $want (a query died mid-run)"
}

# layer_state → "domain_id<TAB>is_combo_layer" for $LAYER, straight from the admin
# API (DB-backed fields — the point of the immediate check is that NO cache wait
# is needed for these two).
layer_state() {
  admin_get "/api/v1/layers?namespace=$NS" | python3 -c "
import json,sys
for l in json.load(sys.stdin):
    if l.get('id') == '$LAYER':
        print(f\"{l['domain_id']}\t{str(l['is_combo_layer']).lower()}\"); break
else:
    print('MISSING\tMISSING')"
}

combo_version() {
  admin_get "/api/v1/combos?namespace=$NS" | python3 -c "
import json,sys
for c in json.load(sys.stdin):
    if c['id'] == '$COMBO':
        print(c['version']); break
else:
    sys.exit('combo $COMBO not found')"
}

echo "== A. preflight =="
read -r HOST_DOMAIN SIMPLE_DOMAIN < <(admin_get "/api/v1/combos?namespace=$NS" | python3 -c "
import json,sys
for c in json.load(sys.stdin):
    if c['id'] == '$COMBO':
        print(c['host_domain_id'], c['simple_domain_id']); break
else:
    sys.exit('combo $COMBO not found')")
IFS=$'\t' read -r dom iscombo < <(layer_state)
[[ "$dom" == "$SIMPLE_DOMAIN" ]] || fail "preflight: layer $LAYER is on '$dom', not the Simple Domain '$SIMPLE_DOMAIN' — refusing to migrate from an unexpected topology"
[[ "$iscombo" == "false" ]] || fail "preflight: layer $LAYER reports is_combo_layer=$iscombo — this script moves PLAIN layers only"
echo "layer on simple domain $SIMPLE_DOMAIN, host $HOST_DOMAIN, is_combo_layer=false — ok"

# Create-time accounting must still hold before we touch anything.
AB_CONFIG_HTTP_BASE="$BASE" AB_CONFIG_NS="$NS" AB_CONFIG_TOKEN="$AB_CONFIG_TOKEN" \
  bash "$SCRIPT_DIR/migration_accounting.sh" --anchor "$ANCHOR" --emig "$EMIG" --ho-exp "$HOEXP" \
  > "$WORKDIR/preflight_accounting.txt" \
  || fail "preflight: create-time accounting (#3) is already red — see $WORKDIR/preflight_accounting.txt"
tail -2 "$WORKDIR/preflight_accounting.txt"

ORIG_MIG_RELEASE=""
if [[ "$VALUE_PLANE" == 1 ]]; then
  echo "== B. value-plane setup: make ${MIG_KEY_NAME} frozen != live =="
  # Everything below reads ids/values from the server so the expectations in the
  # cases files are server-derived, never hardcoded.
  eval "$(admin_get "/api/v1/configs/keys?namespace=$NS" | python3 -c "
import json,sys
ks = {k['key']: k['id'] for k in json.load(sys.stdin)}
print(f'EXP_KEY_ID={ks[\"$EXP_KEY_NAME\"]}'); print(f'MIG_KEY_ID={ks[\"$MIG_KEY_NAME\"]}')")"
  ORIG_MIG_RELEASE=$(admin_get "/api/v1/configs/release/full?namespace=$NS" | python3 -c "
import json,sys
for r in json.load(sys.stdin):
    if r['key_id'] == '$MIG_KEY_ID': print(r['version_id']); break
else:
    sys.exit('$MIG_KEY_NAME has no active full release')")
  ORIG_MIG_VALUE=$(admin_get "/api/v1/configs/versions?namespace=$NS&key_id=$MIG_KEY_ID" | python3 -c "
import json,sys
print({v['id']: v['value'] for v in json.load(sys.stdin)}['$ORIG_MIG_RELEASE'])")
  LIVE2_VALUE="${ORIG_MIG_VALUE}_rtmig_live"
  # Idempotent on rerun: reuse an existing version with this value.
  LIVE2_ID=$(admin_get "/api/v1/configs/versions?namespace=$NS&key_id=$MIG_KEY_ID" | python3 -c "
import json,sys
for v in json.load(sys.stdin):
    if v['value'] == '$LIVE2_VALUE': print(v['id']); break")
  if [[ -z "$LIVE2_ID" ]]; then
    admin_post "/api/v1/configs/drafts" "{\"namespace\":\"$NS\",\"key_id\":\"$MIG_KEY_ID\",\"value\":\"$LIVE2_VALUE\",\"base_version_id\":\"$ORIG_MIG_RELEASE\"}" > /dev/null
    LIVE2_ID=$(admin_post "/api/v1/configs/publish" "{\"namespace\":\"$NS\",\"key_id\":\"$MIG_KEY_ID\"}" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
  fi
  admin_post "/api/v1/configs/release/full" "{\"namespace\":\"$NS\",\"key_id\":\"$MIG_KEY_ID\",\"version_id\":\"$LIVE2_ID\"}" > /dev/null
  echo "live release: v$ORIG_MIG_RELEASE ('$ORIG_MIG_VALUE', stays FROZEN in the combo groups) → v$LIVE2_ID ('$LIVE2_VALUE')"
  sleep "$SLEEP"

  # Generate the three value-matrix cases files from anchor + admin reads.
  python3 - "$ANCHOR" "$WORKDIR" <<PYEOF
import json, sys, urllib.request
anchor, workdir = sys.argv[1], sys.argv[2]
base, ns = "$BASE", "$NS"
def get(path):
    req = urllib.request.Request(base + path, headers={"Authorization": "Bearer $ADMIN_SESSION"})
    with urllib.request.urlopen(req, timeout=15) as r: return json.load(r)
vals = {}  # version_id -> value, per key
g2v = {}   # key_id -> {group_id: version_id}
for kid in ("$EXP_KEY_ID", "$MIG_KEY_ID"):
    vals[kid] = {v['id']: v['value'] for v in get(f"/api/v1/configs/versions?namespace={ns}&key_id={kid}")}
for k in get(f"/api/v1/combos/key-holders?namespace={ns}")['keys']:
    g2v[k['key_id']] = {h['group_id']: h['version_id'] for h in k['holders']}
expv = lambda g: vals["$EXP_KEY_ID"][g2v["$EXP_KEY_ID"][g]]
migv = lambda g: vals["$MIG_KEY_ID"][g2v["$MIG_KEY_ID"][g]]

stayed, sliced = [], []
for line in open(anchor):
    f = line.rstrip("\n").split("\t")
    if len(f) >= 3 and f[1] == "E_MIG": stayed.append((f[0], f[2]))
    elif len(f) >= 4 and f[1] == "SLICED": sliced.append((f[0], f[2], f[3]))
# One stayed uid per E_mig group (both value arms represented), all sliced uids.
seen, picks = set(), []
for uid, g in stayed:
    if g not in seen: seen.add(g); picks.append((uid, g))
assert len(picks) >= 2, "need stayed uids in >=2 groups"
assert sliced, "no SLICED uids in anchor — the switch arm would be empty"

pre, post = [], []
for uid, g in picks:
    pre.append((uid, "$EXP_KEY_NAME", expv(g), f"stayed: E_mig group holds it"))
    post.append((uid, "$EXP_KEY_NAME", expv(g), f"stayed: still held, value unchanged"))
for uid, ho_g, was in sliced:
    pre.append((uid, "$EXP_KEY_NAME", "<DEFAULT>", "sliced: cannot reach E_mig, key has no full release"))
    post.append((uid, "$EXP_KEY_NAME", expv(was), "sliced uid ENTERS E_mig after migrate-out (anchor 'was' group)"))
    pre.append((uid, "$MIG_KEY_NAME", migv(ho_g), "holdout-opt arm: FROZEN version"))
    post.append((uid, "$MIG_KEY_NAME", migv(ho_g), "still frozen: migration must not touch the combo freeze"))
# A simple-world (stayed) uid tracks the LIVE mig-key value in every phase.
live_uid = picks[0][0]
for arr, why in ((pre, "simple world: live full release"), (post, "still simple world for this key")):
    arr.append((live_uid, "$MIG_KEY_NAME", "$LIVE2_VALUE", why))
for name, rows in (("cases_premig.tsv", pre), ("cases_postout.tsv", post)):
    with open(f"{workdir}/{name}", "w") as f:
        for r in rows: f.write("\t".join(r) + "\n")
print(f"cases: premig={len(pre)} postout={len(post)} (stayed picks: {[u for u,_ in picks]}, sliced: {[s[0] for s in sliced]})")
PYEOF

  run_values() { # label cases → dies on driver failure
    (cd "$GO_DIR" && GOWORK=off AB_CONFIG_TOKEN="$AB_CONFIG_TOKEN" \
      go run ./rtmig --http "$BASE" --ns "$NS" --label "$1" --cases "$WORKDIR/$2") \
      | tee "$WORKDIR/values_$1.txt"
    [[ "${PIPESTATUS[0]}" == 0 ]] || fail "value matrix '$1' red — see $WORKDIR/values_$1.txt"
  }
fi

echo "== C. premig evidence =="
snapshot "$WORKDIR/premig_raw.tsv"
echo "premig snapshot: $(wc -l < "$WORKDIR/premig_raw.tsv") uids"
[[ "$VALUE_PLANE" == 1 ]] && run_values premig cases_premig.tsv

echo "== D. migrate-out (Simple Domain → host, layer takes full host traffic) =="
V=$(combo_version)
OUT=$(admin_post "/api/v1/combos/$COMBO/layers/migrate-out" "{\"combo_version\":\"$V\",\"layer_id\":\"$LAYER\"}")
V=$(printf '%s' "$OUT" | python3 -c "import json,sys; print(json.load(sys.stdin)['version'])")
# IMMEDIATE, deliberately before any sleep: domain_id and is_combo_layer are
# DB-backed (走查项 13), so they must already answer the post-move truth while
# the membership below still needs the cache window.
IFS=$'\t' read -r dom iscombo < <(layer_state)
{ echo "immediate post-out layer state: domain_id=$dom is_combo_layer=$iscombo (want $HOST_DOMAIN / false)"; } | tee "$WORKDIR/immediate_postout.txt"
[[ "$dom" == "$HOST_DOMAIN" ]] || fail "immediate post-out: domain_id=$dom, want host $HOST_DOMAIN"
[[ "$iscombo" == "false" ]] || fail "immediate post-out: is_combo_layer=$iscombo, want false (moving a plain layer must not mint a comboLayer)"
sleep "$SLEEP"

snapshot "$WORKDIR/postout_raw.tsv"
# The migration must be OBSERVABLE before anything finer is asserted: identical
# premig/postout snapshots would mean the migrate-out never reached the data
# plane (the mirror-image of README trap 14 — a mutation that silently does
# nothing rubber-stamps every later assertion). Strictly this is implied by the
# accounting below (preflight requires sliced uids OUT of E_mig, post-out
# requires them IN), but a direct check fails with a clearer message.
if diff -q "$WORKDIR/premig_raw.tsv" "$WORKDIR/postout_raw.tsv" > /dev/null; then
  fail "premig and postout snapshots are identical — the migrate-out had no observable effect on the data plane"
fi
# Per-uid accounting on both arms, expectations all violable:
#   E_MIG uid  → in E_mig in its recorded group; NOT in the holdout-opt exp.
#   SLICED uid → in E_mig in the anchor "was" group; STILL in its recorded
#                holdout-opt group (the comboLayer split is untouched).
python3 - "$ANCHOR" "$WORKDIR/postout_raw.tsv" <<PYEOF | tee "$WORKDIR/postout_accounting.txt"
import sys
anchor, snap = sys.argv[1], sys.argv[2]
obs = {}
for line in open(snap):
    uid, pairs = line.rstrip("\n").split("\t")
    obs[uid] = dict(p.split(":") for p in pairs.split(";")) if pairs != "NONE" else {}
bad = ok_stay = ok_slice = n = 0
for line in open(anchor):
    f = line.rstrip("\n").split("\t")
    if not f or not f[0]: continue
    n += 1
    uid, side = f[0], f[1]
    o = obs.get(uid)
    if o is None: print(f"FAIL {uid}: not in snapshot"); bad += 1; continue
    emig, ho = o.get("$EMIG"), o.get("$HOEXP")
    if side == "E_MIG":
        want = f[2]
        if emig == want and ho is None: ok_stay += 1
        else: print(f"FAIL stayed {uid}: want emig={want} ho=None, got emig={emig} ho={ho}"); bad += 1
    elif side == "SLICED":
        want_ho, want_emig = f[2], f[3]
        if emig == want_emig and ho == want_ho: ok_slice += 1
        else: print(f"FAIL sliced {uid}: want emig={want_emig}(anchor 'was') ho={want_ho}, got emig={emig} ho={ho}"); bad += 1
    else:
        print(f"FAIL {uid}: unknown side {side}"); bad += 1
print(f"----\npost-out accounting: n={n} stayed_ok={ok_stay} entered_ok={ok_slice} bad={bad}")
assert ok_stay and ok_slice, "an arm is empty — the run observed nothing on it"
assert ok_stay + ok_slice == n and bad == 0, "post-out accounting does not close"
print(f"PASS post-out per-uid on both arms ({ok_stay} stayed + {ok_slice} entered = {n})")
PYEOF
[[ "${PIPESTATUS[0]}" == 0 ]] || fail "post-out accounting red"
[[ "$VALUE_PLANE" == 1 ]] && run_values postout cases_postout.tsv

echo "== E. migrate-in (host → Simple Domain, round trip) =="
OUT=$(admin_post "/api/v1/combos/$COMBO/layers/migrate-in" "{\"combo_version\":\"$V\",\"layer_id\":\"$LAYER\"}")
IFS=$'\t' read -r dom iscombo < <(layer_state)
{ echo "immediate post-in layer state: domain_id=$dom is_combo_layer=$iscombo (want $SIMPLE_DOMAIN / false)"; } | tee "$WORKDIR/immediate_postin.txt"
[[ "$dom" == "$SIMPLE_DOMAIN" ]] || fail "immediate post-in: domain_id=$dom, want simple $SIMPLE_DOMAIN"
[[ "$iscombo" == "false" ]] || fail "immediate post-in: is_combo_layer=$iscombo, want false"
sleep "$SLEEP"

snapshot "$WORKDIR/postback_raw.tsv"
if diff "$WORKDIR/premig_raw.tsv" "$WORKDIR/postback_raw.tsv" > "$WORKDIR/roundtrip.diff"; then
  echo "PASS round-trip invariance: postback snapshot is byte-identical to premig ($(wc -l < "$WORKDIR/postback_raw.tsv") uids, all experiments)"
else
  fail "round-trip snapshots differ — see $WORKDIR/roundtrip.diff (THIS IS THE REGRESSION SIGNAL: id/salt-stable migration must not move anyone)"
fi
[[ "$VALUE_PLANE" == 1 ]] && run_values postback cases_premig.tsv

if [[ "$VALUE_PLANE" == 1 ]]; then
  echo "== F. restore value plane =="
  admin_post "/api/v1/configs/release/full" "{\"namespace\":\"$NS\",\"key_id\":\"$MIG_KEY_ID\",\"version_id\":\"$ORIG_MIG_RELEASE\"}" > /dev/null
  echo "live release for $MIG_KEY_NAME restored to v$ORIG_MIG_RELEASE ('$ORIG_MIG_VALUE'); version v$LIVE2_ID remains in the append-only history (harmless: no group holds it, nothing releases it)"
fi

echo "== ALL GREEN: runtime migrate-out/in round trip verified =="
