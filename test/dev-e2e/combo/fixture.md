# `st9_combo` fixture contract

This document defines the topology required by the combo E2E drivers. Admin
writes use the platform API at `http://localhost:8081`, with
`Authorization: Bearer $ADMIN_SESSION` and JSON content type. Read every
generated id and current `row_version` from API responses before a dependent
write.

The checked-in candidate users and `server/anchor.tsv` are tied to one exact
seeded topology. Current combo creation appends a random 10-character suffix to
each structural node id, and the full id is an effective bucketing salt. A new
combo therefore changes membership even when its name and shares are identical.
Fresh construction is not a drop-in reproduction of the checked-in suite: it
requires regenerating candidate users, recording a new live anchor, and
updating every client expectation together. Until that workflow is automated,
use this document as the topology contract and preserve the matching database,
or treat the coordinated fixture refresh as a separate reviewed change.

After a write, wait at least 11 seconds before asserting through the data plane.

## Namespace and keys

Create namespace `st9_combo` and these string keys:

| Key | Purpose |
|---|---|
| `managed_a` | Frozen-versus-live SDK assertions. |
| `exp_key` | The independently migrated experiment. |
| `zero_key` | Zero-width group observations. |
| `unheld_key` | Live full-release control not claimed by a combo. |
| `mig_key` | Makes the migration combo groups observable without conflicting with `exp_key`. |

Use the platform endpoints `/api/v1/namespaces`, `/api/v1/configs/keys`,
`/api/v1/configs/drafts`, `/api/v1/configs/publish`, and
`/api/v1/configs/release/full`.

## Frozen/live values

1. Publish `managed_a=LIVE_v1` and set it as the full release.
2. Create combo `c1` in `root:st9_combo`:

   ```json
   {
     "namespace": "st9_combo",
     "domain_id": "root:st9_combo",
     "name": "c1",
     "simple_share_bps": 9000,
     "groups": [
       {"name": "h1", "role": "holdout", "share_bps": 500},
       {"name": "o1", "role": "opt", "share_bps": 500}
     ]
   }
   ```

3. With the bootstrap token, read `combo_layer_id`,
   `holdout_opt_experiment_id`, and `holdout_opt_domain_id` from
   `GET /api/v1/combos?namespace=st9_combo`.
4. Claim `managed_a` at its v1 id into `c1` through
   `POST /api/v1/combos/claim`; verify both group params hold that version id.
5. Publish `managed_a=LIVE_v2` and move the full release to v2.
6. Publish and full-release `unheld_key=UNHELD_v1`; do not claim it.

The required distinction is now:

- `c1` holdout/opt users resolve `managed_a` to frozen `LIVE_v1`;
- users whose matched groups do not hold `managed_a` resolve live `LIVE_v2`;
- every user resolves `unheld_key` to `UNHELD_v1`.

Do not reorder claim and v2 full release. Frozen and live being equal would make
the client assertion non-discriminating.

## Migrated layer

Publish `exp_key=EXP_A` and `exp_key=EXP_B`. Create layer `L_mig` in the root
domain with traffic total 10,000. Create `E_mig`, then explicitly allocate its
full slot before starting it:

1. `POST /api/v1/experiments` and retain the returned experiment id and
   `version`.
2. `PATCH /api/v1/experiments/{id}/layer` with
   `new_layer_id=<L_mig id>`, `traffic_range_lo=0`, `traffic_range_hi=9999`, and
   the current `row_version` encoded as a JSON string.
3. Retain the bumped version returned by the transfer, then
   `POST /api/v1/experiments/{id}/start` with that version as `row_version`.

Configure these groups and held values:

| Group | Range | Held value |
|---|---:|---|
| `gA` | 0–4999 | `exp_key=EXP_A` |
| `gB` | 5000–9999 | `exp_key=EXP_B` |

Before migrating the layer, query `mig-u1` through `mig-u60` from the live
server and store their observed group ids in `server/anchor.tsv`. Never derive
this anchor from the reverse solver.

Create combo `c2mig` with `migrate_layer_ids=[L_mig]`:

```json
{
  "namespace": "st9_combo",
  "domain_id": "root:st9_combo",
  "name": "c2mig",
  "simple_share_bps": 9000,
  "groups": [
    {"name": "h_narrow", "role": "holdout", "share_bps": 100},
    {"name": "o_wide", "role": "opt", "share_bps": 900}
  ],
  "migrate_layer_ids": ["<L_mig id>"]
}
```

The asymmetric 10/90 holdout-opt split is intentional and increases detection
of a wrong salt. Publish `mig_key` with distinct frozen and live versions, then
claim its frozen version into `c2mig`. Do not reuse `exp_key`; the holder
relationship conflicts with the migrated experiment and must be rejected.

Expected accounting is 48 users still in their recorded `E_mig` groups plus 12
users in a real `c2mig` holdout-opt group, totaling all 60.

## Zero-width group

Publish and full-release `zero_key=ZERO_LIVE`. Create `c3zero`:

```json
{
  "namespace": "st9_combo",
  "domain_id": "root:st9_combo",
  "name": "c3zero",
  "simple_share_bps": 8000,
  "groups": [
    {"name": "h_zero", "role": "holdout", "share_bps": 0},
    {"name": "h_live", "role": "holdout", "share_bps": 1000},
    {"name": "o_live", "role": "opt", "share_bps": 1000}
  ]
}
```

Claim `zero_key` into `c3zero`. The zero group materializes as an empty interval
and must never be returned. No checked-in runner samples this property. Use the
executable manual check in [README.md](./README.md#manual-distribution-checks),
which samples 400 `z-*` users and requires real observations from both non-zero
arms. Because the command reads the current structural ids, it can inspect a
newly randomized topology, but it does not make the checked-in hard-coded SDK
expectations reproducible on that topology.

## Route pin

Read the candidate user's pre-pin result from the service, then create a domain
route whitelist for the `c1` holdout-opt domain:

```json
{
  "entity_kind": "domain",
  "entity_id": "<c1 holdout_opt_domain_id>",
  "uid": "st9-probe-0"
}
```

The pin moves from the 90% simple side to the 10% holdout-opt side. Exclude
`st9-probe-0` from the general frozen/live cases after the pin because its
expected arm has intentionally changed.

## Candidate users

The checked-in clients expect this exact fixture membership:

| User | `c1` | `c2mig` | `c3zero` | `E_mig` | Use |
|---|---|---|---|---|---|
| `st9-probe-0` | `h1` | none | none | `gA` | route pin |
| `st9-probe-1` | none | `o_wide` | none | none | live `managed_a` |
| `st9-probe-2` | none | none | none | `gA` | live `managed_a` |
| `st9-probe-4` | `o1` | none | none | `gA` | frozen `managed_a` |
| `st9-probe-9` | `h1` | none | none | `gB` | frozen `managed_a` |
| `st9-probe-10` | `o1` | none | `o_live` | `gB` | frozen `managed_a` |
| `st9-probe-24` | `h1` | none | none | `gB` | frozen `managed_a` |
| `st9-probe-27` | `h1` | `o_wide` | none | none | solver check |
| `st9-probe-39` | `o1` | none | none | `gB` | solver check |

Membership is relative to each independent combo or experiment. For example,
`st9-probe-1` is in `c2mig/o_wide` but that group does not hold `managed_a`, so
the user must receive `LIVE_v2`. This is stronger than a globally group-less
case.

After changing any share, claim, pin, id or salt, regenerate candidate users and
verify every membership through `GetExperimentResult` before editing expected
values. Claiming `managed_a` into `c2mig` or `c3zero` invalidates the current
client fixture.
