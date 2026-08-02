# `st9_combo` fixture — exact build steps

Every step below was executed against the combo branch; the ids shown are from
that run and **will differ on yours** (combo ids embed a creation timestamp, and
node ids are UUIDs). Read back the ids you get and substitute.

All admin calls need `-H "Authorization: Bearer $SESSION"` and
`-H 'content-type: application/json'` against `http://localhost:8081`.
Wait **≥11s** after any write before asserting (10s cache reload).

## 1. Namespace and config keys

```sh
POST /api/v1/namespaces        {"namespace":"st9_combo","description":"ST9 combo e2e"}
POST /api/v1/configs/keys      {"namespace":"st9_combo","key":"managed_a", ...,"value_type":"string"}  # → id 1
POST /api/v1/configs/keys      {"namespace":"st9_combo","key":"exp_key",   ...}                        # → id 2
POST /api/v1/configs/keys      {"namespace":"st9_combo","key":"zero_key",  ...}                        # → id 3
POST /api/v1/configs/keys      {"namespace":"st9_combo","key":"unheld_key",...}                        # → id 4
```

## 2. Versions — the frozen/live split that makes #2 non-degenerate

```sh
# managed_a: v1 first, and it is what the combo groups will freeze on
POST /api/v1/configs/drafts    {"namespace":"st9_combo","key_id":"1","value":"LIVE_v1","base_version_id":null}
POST /api/v1/configs/publish   {"namespace":"st9_combo","key_id":"1"}      # → version_id 1
POST /api/v1/configs/release/full {"namespace":"st9_combo","key_id":"1","version_id":"1"}

# exp_key: two versions for the pre-existing experiment's two groups
#   → version_id 2 ("EXP_A"), version_id 3 ("EXP_B")
# zero_key: one version → version_id 4 ("ZERO_LIVE"), released full
```

**Order matters**: claim `managed_a` into the combo *while the full release is
still v1* (step 4), then move the full release to v2 (step 6). That is what makes
frozen ≠ live.

## 3. combo `c1` — the frozen/live subject (assertion #2)

```sh
POST /api/v1/combos {"namespace":"st9_combo","domain_id":"root:st9_combo","name":"c1",
  "simple_share_bps":9000,
  "groups":[{"name":"h1","role":"holdout","share_bps":500},
            {"name":"o1","role":"opt","share_bps":500}]}
```
Yields h1 `[0,44999]`, o1 `[45000,89999]` (= `halfUp(90000×500/1000)`), combo-layer
slots holdout-opt `[0,999]` / simple `[1000,9999]`.

Read the node ids back **with the bootstrap token** (they are redacted otherwise):
```sh
GET /api/v1/combos?namespace=st9_combo   -H "Authorization: Bearer $BOOTSTRAP"
# → combo_layer_id, holdout_opt_experiment_id, holdout_opt_domain_id
```

## 4. Claim `managed_a` at v1 → groups freeze on v1

```sh
POST /api/v1/combos/claim {"namespace":"st9_combo","key_id":"1","version_id":"1",
  "targets":[{"combo_id":"<c1 id>"}]}
```
Verify both groups now show `params = {"1": "1"}`.

## 5. Reverse-solve uids

```sh
(cd tools/solve && go run . <combo_layer_id> <holdout_opt_experiment_id> h1)      # → st9-probe-9, 24, 27
(cd tools/solve && go run . <combo_layer_id> <holdout_opt_experiment_id> o1)      # → st9-probe-4, 10, 39
(cd tools/solve && go run . <combo_layer_id> <holdout_opt_experiment_id> simple)  # → st9-probe-0, 1, 2
```
**Self-verify before trusting it**: query each uid against
`/api/v1/abtest/experiment_result` (display_type
`RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP`) and confirm the predicted group is the
observed one, *and* that the h1/o1 arms return non-empty groups. If everything
returns `NONE`, the `simple` cases pass vacuously — that is a degenerate pass, not
a working solver.

## 6. Move the live release to v2 (after the claim)

```sh
POST /api/v1/configs/drafts    {"namespace":"st9_combo","key_id":"1","value":"LIVE_v2","base_version_id":"1"}
POST /api/v1/configs/publish   {"namespace":"st9_combo","key_id":"1"}                       # → version_id 5
POST /api/v1/configs/release/full {"namespace":"st9_combo","key_id":"1","version_id":"5"}
# unheld_key: publish + release full (→ version_id 6). Claimed by NO combo.
```
Now: h1/o1 members resolve `managed_a`→`LIVE_v1` (frozen), simple→`LIVE_v2` (live),
and `unheld_key`→`UNHELD_v1` for everyone.

## 7. Pre-existing layer + experiment, then migrate it (assertion #3)

```sh
POST /api/v1/layers      {"namespace":"st9_combo","domain_id":"root:st9_combo","name":"L_mig","traffic_total":10000}
POST /api/v1/experiments {... "layer_id":"<L_mig>","type":"config_version","name":"E_mig",
  "slot":{"traffic_range_lo":0,"traffic_range_hi":9999},
  "groups":[{"name":"gA","traffic_range_lo":0,"traffic_range_hi":4999,"key_versions":[{"key_id":"2","version_id":"2"}]},
            {"name":"gB","traffic_range_lo":5000,"traffic_range_hi":9999,"key_versions":[{"key_id":"2","version_id":"3"}]}]}
POST /api/v1/experiments/<E_mig>/start {"version":"1"}
```

**Record the anchor from the server, not from the solver** — 60 uids `mig-u1..60`,
storing the observed `group_id` per uid. This file is the expectation for the
post-migration check; re-predicting instead would be a tautology.

```sh
# migrate: the ONLY way to re-parent a layer (PATCH /layers/{id} rejects domain_id)
POST /api/v1/combos {"namespace":"st9_combo","domain_id":"root:st9_combo","name":"c2mig",
  "simple_share_bps":9000,
  "groups":[{"name":"h_narrow","role":"holdout","share_bps":100},
            {"name":"o_wide","role":"opt","share_bps":900}],
  "migrate_layer_ids":["<L_mig>"]}
```
Asymmetric on purpose (h_narrow → `[0,8999]`, 10% of holdout-opt) per the kill-rate
finding in README §3.

Claim a **fresh** key into c2mig (`mig_key`) so its holdout/opt groups appear in
`experiment_result` at all — groups with empty `params` produce no output. Do not
reuse `exp_key`: `E_mig` sits inside c2mig's simple domain, so the two holders are
in an ancestor-descendant relation and the key-claim guard rejects it with 409.

Expect **full accounting of all 60 uids**: 48 still in `E_mig` *each in its
recorded group*, and 12 resolving to a real group of **c2mig's holdout-opt
experiment**. Asserting only "48 unchanged, 12 absent from `E_mig`" is not enough —
absence cannot distinguish "moved into holdout-opt" from "routed nowhere".

## 8. combo `c3zero` — zero-slot group (assertions #1 and #4)

```sh
POST /api/v1/combos {"namespace":"st9_combo","domain_id":"root:st9_combo","name":"c3zero",
  "simple_share_bps":8000,
  "groups":[{"name":"h_zero","role":"holdout","share_bps":0},
            {"name":"h_live","role":"holdout","share_bps":1000},
            {"name":"o_live","role":"opt","share_bps":1000}]}
POST /api/v1/combos/claim {"namespace":"st9_combo","key_id":"3","version_id":"4","targets":[{"combo_id":"<c3 id>"}]}
```
`h_zero` materializes as `[0,-1]` — the `hi = lo - 1` empty interval — and **shares
boundary point 0 with `h_live` `[0,44999]`**. That collision is the point: it is the
shape an implementation that reverse-derives group order from intervals would get
wrong.

Use a **separate key** (`zero_key`) here: claiming `managed_a` again returns 409
`not mutually exclusive across holders`, because orthogonal combos must hold
disjoint key sets. That 409 is the key-claim guard working correctly.

#1 samples N=4000 (`p-1..4000`); #4 samples 400 (`z-1..400`).

## 9. Pin for assertion #5

Record the pre-pin group of the candidate uids **from the server** (expect `NONE`
= simple), then:
```sh
POST /api/v1/route-whitelists {"entity_kind":"domain","entity_id":"<c1 holdout_opt_domain_id>","uid":"st9-probe-0"}
```
Writes both `route_whitelist_uid` (fact source) and `layer_whitelist_uid_unique`
(per-layer sentinel). Pin direction is wide→narrow (simple 90% → holdout-opt 10%)
so a silently-ineffective pin lands elsewhere with ~90% probability.

`st9-probe-0` is then **excluded from the #2 fixture** in all three drivers: once
pinned, its expected value is ambiguous between the frozen and live arms.

## 10. Full uid membership table — read this before editing any fixture

Every uid sits in **several orthogonal groups at once** (that is the whole point of
orthogonal combos), so "uid X is a simple-world user" is never true globally — it is
only true *relative to one experiment*. An earlier version of the drivers annotated
`st9-probe-1` as "no combo group", which was wrong: it is in c2mig's `o_wide`, and
the case only passed because that group does not hold `managed_a`. Adding a
`managed_a` claim to c2mig turns it red.

Observed memberships (`combo/role-group [keys held]`):

| uid | c1 | c2mig | c3zero | E_mig | used by |
|---|---|---|---|---|---|
| `st9-probe-0` | holdout h1 [managed_a] | — | — | gA [exp_key] | #5 pinned target |
| `st9-probe-1` | — | **opt o_wide [mig_key]** | — | — | #2 "live" case |
| `st9-probe-2` | — | — | — | gA [exp_key] | #2 "live" case |
| `st9-probe-4` | opt o1 [managed_a] | — | — | gA [exp_key] | #2 frozen case |
| `st9-probe-9` | holdout h1 [managed_a] | — | — | gB [exp_key] | #2 frozen case |
| `st9-probe-10` | opt o1 [managed_a] | — | **opt o_live [zero_key]** | gB [exp_key] | #2 frozen case |
| `st9-probe-24` | holdout h1 [managed_a] | — | — | gB [exp_key] | #2 frozen case |
| `st9-probe-27` | holdout h1 [managed_a] | **opt o_wide [mig_key]** | — | — | solver output only |
| `st9-probe-39` | opt o1 [managed_a] | — | — | gB [exp_key] | solver output only |

**Consequences for editing:**

- The #2 "live full value" cases (`probe-1`, `probe-2`) depend on **no combo group
  holding `managed_a` for them**. `probe-1` being in c2mig's `o_wide` is fine *and
  is a stronger test* than a truly group-less uid: it proves "in a combo group, but
  that group does not hold this key ⇒ live value", which is the second half of
  design-testing.md's frozen/live requirement. Do not "fix" it by swapping in a
  group-less uid — that would weaken the case.
- **Claiming `managed_a` into c2mig or c3zero will turn #2 red.** The drivers share
  namespace `st9_combo` with the #3 and #4 fixtures. Any new claim must be checked
  against this table.
- `probe-2` is in `E_mig/gA`, which holds `exp_key` — harmless for `managed_a`
  assertions, but it means `probe-2` is *not* a blank-slate uid either.
- Regenerate this table (rather than trusting it) after changing any share, claim,
  or pin — it is a snapshot of a specific fixture state, not an invariant.
