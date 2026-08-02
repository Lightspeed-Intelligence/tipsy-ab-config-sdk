# combo/holdout SDK data-plane e2e (ST9)

Verifies the **combo / holdout-opt** data-plane semantics of the platform through
the three released SDKs. Covers the five assertions in the platform repo's
`docs/combo/design-testing.md` §「测试与验收」#2.

Combo has **no wire-protocol surface** (`api/proto/**` contains no combo/holdout
message), so nothing here tests SDK features — it tests *server* semantics through
real SDK clients. The SDKs are consumed unmodified, as a business consumer would.

## Why this lives here and not in the platform repo

The platform repo has its own `test/dev-e2e/`, but that copy is a **stale fork**
(last touched 2026-07-20): it has no `clients/java`, its `bucketfind` `allClients`
lists 6 tags instead of 8, and each driver is not its own module. Porting this
suite there would create a *third* diverging copy. The platform repo's
`docs/combo/design-testing.md` carries a pointer here instead.

## The five assertions

| # | Assertion | Where | Mutation that must kill it |
|---|---|---|---|
| 1 | combo split is proportional | server | group width 50/50 → 70/30 ⇒ 7.85σ |
| 2 | held key → frozen value; unheld key → live value | **3 SDKs** | group params follow live version ⇒ 4 cases red per language |
| 3 | group membership survives layer migration, **per-uid on both arms** | server | salt changed ⇒ 23/60 change group; holdout-opt paused ⇒ 12 sliced users lose their group; ho slot 10%→99% ⇒ 47 stayed-users misplaced |
| 4 | zero-slot group is never hit | server | give it real width ⇒ 5/400 hits |
| 5 | layer-whitelist pin routes correctly | **Go SDK** | delete route target row ⇒ pin falls back to hashing |

Assertions 1/3/4 are server-only **by mechanism, not by convenience**: bucketing is
decided entirely server-side and the SDK forwards the uid, so running them through
three SDKs would test one server conclusion three times.

Assertion 2 **requires** a real SDK: combo managed keys are permanently
`has_dynamic_resolution=true` (the holdout-opt experiment is always `running`, so
GPV always reports their versions), and **only the SDK consumes that flag** — the
server's `/config/dynamic` never reads it (`configservice/server.go` has zero
references).

Precisely what a server-side test cannot see: it is *not* that the server would
compute a wrong value. The server resolves the frozen value correctly either way,
because it calls `GetExperimentResult` unconditionally. What only an SDK client
exercises is the **decision to make that call at all**. If the flag were wrong, the
SDK would skip the abtest RPC and fall through to the live full-release value —
so the frozen semantics break on the client while the server's own answer stays
right. That step has no server-side representation to assert against.

## Non-degeneracy requirements (do not "simplify" these)

Each is load-bearing; removing one makes an assertion pass without testing anything.

1. **Frozen must differ from live.** The fixture publishes v1, freezes the group at
   v1, then publishes v2 and makes *that* the full release. If frozen == live,
   "took the frozen value" and "took the live value" are indistinguishable.
2. **The fallback delta must be asserted, and it must be a delta.** A correct value
   can be delivered by the ab→full fallback arm rather than the abtest main path.
   That arm is intentional and works, so it *masks* what #2 tests. Absolute-zero
   would be red for unrelated reasons; the delta isolates these calls.
3. **Do not use symmetric group shares for bucket assertions.** A wrong-salt
   mutation lands in the same group by luck at a rate set by group width — measured
   on this topology: 50% width → **50.8%** kill rate, 10% → 91.1%, 1% → 98.7%,
   0.1% → 100%. Under 50/50, half of all such mutations survive and look like
   "this assertion has no discriminating power".
4. **Pre-state anchors must be recorded from the server, never re-predicted.** For
   #3 and #5 the "before" value is read from the live server and written to a file.
   Re-predicting with the solver would derive the expectation from the same
   salt-fallback rule the code under test uses — a tautology that is *always* green
   regardless of the implementation.
5. **Assert that the observed thing was observed.** Every driver prints
   `OBSERVATION-COUNT`, and the server-side checks require both arms non-empty
   (e.g. #4 needs h_live/o_live to have real hits, otherwise "zero-slot got 0 hits"
   is satisfied by observing nothing at all).
6. **#3 must account for all 60 users, not just the ones that stayed.** An earlier
   version asserted only that users still in `E_mig` kept their group, and treated
   the 12 sliced-out users as "absent from `E_mig`". That made *"moved into
   holdout-opt"* and *"vanished"* share one observed value: a bug routing those
   users nowhere would have passed. The check now also resolves each of those 12
   against c2mig's holdout-opt experiment and requires a real group, so
   `48 + 12 = 60` is full accounting. Verified by pausing that experiment: the old
   arm stayed green (`same=48, changed=0`) while the new arm reported 12
   unaccounted — i.e. the gap was real, not theoretical.

### A known observation-granularity limit (not laziness)

For #1 and #3 the "user is in the simple world" verdict is expressed as *absence*
of a combo group in the response (`NONE`). The server has no positive "this user is
in simple" field, so there is no more direct observation available. `NONE` could in
principle also arise from an unrelated failure (topology load error, ns mismatch),
so it is fenced in two directions: the other arms must be non-empty (proving the
observation path works at all) and the counts must add up to N with nothing
unclassified. If either fence breaks, those would go red first. Do not "simplify"
by dropping the arm checks or the coverage total — they are what makes `NONE`
meaningful.

## Tolerance for #1 (derived, not guessed)

Binomial: σ = √(p(1−p)/N). At N=4000, p=0.10 → σ=0.474pp, 4σ = ±1.897pp.
4σ is used (not 2σ) because five assertions run together; per-assertion two-sided
false-positive is 6.3e-05, joint ≈3e-04. Observed deviations were 0.47σ / 1.11σ /
0.47σ — comfortably inside, and *not* hugging the bound, which is what rules out a
systematic bias hiding under a loose tolerance.

**Measured detection floor: between 2pp and 3pp.** An injected 2pp shift reached only
3.27σ and survived; 3pp and above is caught. So the flip side of "observed deviation
is far below tolerance" is that **the smallest error this check can detect is higher
than the tolerance number suggests** — a sub-2pp systematic bias would pass. Raising
N is the lever if that ever matters (σ shrinks as √N); at N=4000 a 1pp error is
simply below the noise floor. Recorded here so nobody reads "0.47σ" as "this
assertion is sensitive to any error".

## Prerequisites — the traps, in the order you will hit them

Every one of these cost real time; none is guessable from the code.

1. **`test/dev-e2e/` in the platform repo is a stale fork.** See above. Also, the
   platform repo's `go.work` lists `../tipsy-ab-config-sdk`, which does not exist
   inside a git worktree ⇒ **all Go builds there need `GOWORK=off`**.
2. **Server port env vars are `HTTP_ADDR` / `GRPC_ADDR` / `METRICS_ADDR` /
   `PPROF_ADDR`** — not `*_PORT`. Setting `HTTP_PORT` is silently ignored and the
   server binds :8080.
3. **Ports 9090 / 9091 / 6060 are commonly already taken** on a dev box (other
   servers, metrics, pprof). Verify with `lsof` before starting; this suite uses
   8081 / 50052 / 9099 / 6099.
4. **Two different tokens, two different auth schemes.**
   - Admin API: `cmd/devtoken --sub <uid>` (RS256 session), sent as
     `Authorization: Bearer <t>`. The cookie name is `token`, but the Bearer header
     is what the API accepts.
   - SDK / data plane: `cmd/servicetoken --sub s --namespaces <ns>` (HS256).
   Requires an existing `console_admin` row for the uid; `roles=["bootstrap"]` is
   needed to read unredacted combo node ids (see 6).
5. **`display_type` enum is `RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP`.** There is
   no `..._GROUP_LIST`. A wrong name does not error — the server returns an empty
   `groups` array, which reads exactly like "no user matched".
6. **Combo node ids are redacted for non-bootstrap admins.** `GET /api/v1/combos`
   omits `combo_layer_id` and `holdout_opt_*` unless the caller's `console_admin`
   row has `roles=["bootstrap"]`. The reverse-solver needs those ids.
7. **A layer cannot be re-parented via the API.** `PATCH /api/v1/layers/{id}`
   supports only `status=deleted`; it returns 400 for a `domain_id` change. The
   only migration path is `migrate_layer_ids` at combo-create time.
8. **Python SDK API has moved.** At 0.14.0,
   `new_abtest_context(user_id, user_attrs, *, trace_id)` takes **no ns positional
   arg**, and the close method is **`aclose()`**, not `close()`. The stale driver in
   the platform repo predates both.
9. **Experiments start in `draft`** and serve no traffic until
   `POST /api/v1/experiments/{id}/start`.
10. **The cache reloads every 10s.** Sleep ≥11s after any DB or API write before
    asserting, or you will read pre-write state.
11. **Java offline build fails** (`mvn -o`) on a missing `maven-shade-plugin:3.6.0`;
    first build needs network. JDK 21+ required.
12. **Use a dedicated database.** This suite creates its own (`tipsy_sdk_e2e`).
    Never point it at a shared dev DB: the platform's dev database holds real
    business rows, and `TRUNCATE`-style cleanup there is prohibited.
13. **`GROUPS` is a bash builtin array** (the caller's unix group ids). Assigning
    `GROUPS="a,b"` in bash silently writes element 0 and reads back as a number.
    This cost four wrong fixes: the accounting script reported "12 users
    unaccounted" against a completely healthy server, which is indistinguishable
    from a real routing regression. zsh has no such builtin, so it only reproduces
    under bash. `server/migration_accounting.sh` now uses `HO_GROUPS_CSV` and
    validates that each parsed element is a 36-char uuid — if the variable is ever
    clobbered again, it fails loudly instead of silently mis-classifying users.

14. **Bare SQL on the config plane does not reach the SDK.** Editing
    `config_version` / `release_full` directly does **not** advance
    `namespace_snapshot_seq`, and the SDK's snapshot refresh is gated on that seq —
    so the server's `/config/dynamic` shows your change within 10s while the SDK
    keeps serving the old value indefinitely. Consequence: a config-plane mutation
    **silently does nothing**, the drivers stay green, and it reads as "this
    assertion has no discriminating power" — after which every later mutation gets
    rubber-stamped as covered. Verified here: mutating `unheld_key`'s value gave
    server `UNHELD_MUTATED` but SDK `UNHELD_v1` = PASS; after
    `UPDATE namespace_snapshot_seq SET seq = seq + 1 WHERE namespace = '<ns>'` the
    drivers turned red as intended. The existing mutations in this suite all target
    `experiment_group` / `experiment` (abtest plane, not seq-gated), which is why
    they were unaffected — but anything touching the config plane must bump the seq.
    **Rule: the 10s cache reload and the SDK snapshot path are two different
    mechanisms; only the former is time-based.**

The last two generalise: **when a check reports a failure that would be a serious
regression, first prove the check itself is sound** — three of the four
self-inflicted bugs here (wrong enum name, `UID` readonly in zsh, `GROUPS` builtin)
produced output that looked exactly like a broken platform. And the mirror image:
**when a mutation produces no failure, first prove the mutation actually reached the
code under test** (trap 14 is that failure mode).

## Setup

```sh
# 1. dedicated DB + migrations (platform repo)
docker exec <pg-container> psql -U tipsy -d postgres -c 'CREATE DATABASE tipsy_sdk_e2e OWNER tipsy;'
DATABASE_URL="postgres://tipsy:tipsy@localhost:15432/tipsy_sdk_e2e?sslmode=disable" \
  GOWORK=off go run ./cmd/server migrate up

# 2. admin rows (100001 plain, 100002 bootstrap)
docker exec <pg-container> psql -U tipsy -d tipsy_sdk_e2e -c \
  "INSERT INTO console_admin(user_id,note) VALUES (100001,'e2e'),(100002,'e2e bootstrap') ON CONFLICT DO NOTHING;
   UPDATE console_admin SET roles='[\"bootstrap\"]'::jsonb WHERE user_id=100002;"

# 3. server on isolated ports (platform repo, combo branch)
HTTP_ADDR=:8081 GRPC_ADDR=:50052 METRICS_ADDR=:9099 PPROF_ADDR=127.0.0.1:6099 \
TIPSY_SERVICE_SECRET=devsecret \
TIPSY_CHAT_JWT_PUBLIC_KEY_PEM="$(cat scripts/fixtures/dev-jwt-public.pem)" \
TIPSY_CHAT_JWT_AUDIENCE=dev TIPSY_CHAT_JWT_ISSUER=tipsy-backend \
TIPSY_BACKEND_LOGIN_URL=http://localhost:8081/login \
TIPSY_BACKEND_LOGOUT_URL=http://localhost:8081/logout \
DATABASE_URL="postgres://tipsy:tipsy@localhost:15432/tipsy_sdk_e2e?sslmode=disable" \
GOWORK=off go run ./cmd/server

# 4. tokens
SESSION=$(GOWORK=off go run -tags devtools ./cmd/devtoken --sub 100001)
BOOTSTRAP=$(GOWORK=off go run -tags devtools ./cmd/devtoken --sub 100002)
export AB_CONFIG_TOKEN=$(TIPSY_SERVICE_SECRET=devsecret GOWORK=off \
  go run ./cmd/servicetoken --sub st9-sdk-e2e --namespaces st9_combo --ttl 6h)
```

Then build the `st9_combo` fixture (ns → keys → drafts → publish → release_full →
combos → claim), which `fixture.md` records step by step, and run:

```sh
# #2 — all three SDKs, both transports (20/20 each)
(cd clients/go   && GOWORK=off go run .)
AB_CONFIG_TOKEN=$AB_CONFIG_TOKEN <py312-venv>/bin/python clients/py/run.py
(cd clients/java && mvn -q -DskipTests package && java -jar target/st9-combo-java.jar)

# #5 — pin routing end-to-end (4/4)
(cd clients/go && GOWORK=off go run ./pin)

# #3 — migration accounting, per-uid on both arms (48 stayed + 12 sliced = 60)
bash server/migration_accounting.sh --anchor server/anchor.tsv \
  --emig <E_mig id> --ho-exp <c2mig holdout-opt experiment id>

# reverse-solver: uids that land in a given combo arm
(cd tools/solve && go run . <combo-layer-id> <holdout-opt-exp-id> h1|o1|simple)
```

## Known limitations of the evidence itself

Recorded so a later reader does not over-trust these results:

- **#1 and #4 "byte-identical after restore" is single-observation.** The restore
  check compares the same field the assertion reads (`groups[]` membership). There is
  no independent second observation to cross-check against, so it confirms
  reproducibility, not correctness-by-agreement. This is the same `NONE`
  observation-granularity ceiling described above.
- **The Java jar is not rebuilt on every run.** `clients/java/target/` may hold a jar
  older than `ComboMain.java`. Run `mvn -q -DskipTests package` before trusting a
  Java result, or you may be testing a stale build.
- **The sliced-arm anchor for #3 was recorded post-migration.** c2mig does not exist
  before the migration, so its holdout-opt group ids cannot be observed beforehand.
  The anchor therefore pins *which* group each sliced uid is in (catching a changed
  split, as the 10%→99% mutation shows) but cannot independently prove that the
  original slicing was itself correct. The pre-migration `E_mig` column, which *is* a
  true before-state, is what covers that direction.

## Last green run

All five assertions green simultaneously against the combo branch
(platform `b91742b`): #2 Go 20/20, Python 20/20 (SDK 0.14.0), Java 20/20
(SDK 0.6.0), each over HTTP + plaintext gRPC; #5 4/4; #3 48 stayed + 12 sliced = 60
per-uid; #1 N=4000 within 4σ; #4 0/400. Every assertion was mutation-verified:
broken → red → restored → byte-identical output (4000-line and 400-line result files
diffed exactly).

Assertions #1 and #3 were each strengthened after independent review found a
surviving mutation — see the mutation column above. The lesson worth keeping: both
survivors were cases where the assertion *looked* complete (counts added up, nothing
was unaccounted for) but one arm had no violable expectation.
