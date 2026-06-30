-- ============================================================================
-- dev e2e seed.sql — idempotent topology + config + gray + experiments for the
-- demo-test and for_dev_agent_test namespaces.
--
-- HOW TO RUN (user executes against the dev DATABASE_URL; the Agent has no DB
-- access):
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/seed.sql
--
-- After it COMMITs, WAIT >= 5 SECONDS before running any test: the dev abtest
-- cache does an unconditional full LoadAll reload every 5s
-- (internal/abtest/cache/refresher.go), so directly-inserted rows are picked up
-- by the running dev server within <=5s — no restart, no admin write call.
--
-- IDEMPOTENT: every INSERT uses ON CONFLICT ... DO UPDATE / DO NOTHING, and the
-- whole script is wrapped in a single BEGIN; ... COMMIT; transaction. Re-running
-- is safe and converges to the same state. Run teardown.sql to remove this batch.
--
-- SCHEMA: columns verified against migrations 0001 (init), 0005 (v2_topology),
-- 0006 (drop is_control). experiment_group has NO is_control column (dropped by
-- 0006) — it is intentionally never referenced here.
--
-- ID CONVENTIONS (design doc 数据拓扑 §通用 ID 约定):
--   - TEXT primary keys (domain/layer/slot/experiment/group): readable strings
--     prefixed by namespace, e.g. "demo-test:exp:cfg".
--   - BIGINT keys use fixed high bands well above the live BIGSERIAL sequences:
--       demo-test:           config_key 900100xxx, config_version 900200xxx
--       for_dev_agent_test:  config_key 900300xxx, config_version 900400xxx
--       release_full         900500xxx (both ns)
--       release_whitelist    900600xxx (both ns)
--   - Derived BIGSERIAL tables (experiment_config_param / layer_key_claim /
--     experiment_group_whitelist_uid / release_whitelist_uid_active) get no
--     explicit id; they are made idempotent via their natural UNIQUE keys.
--   - root domain id = "root:<ns>" (auto-created by the platform; we backstop
--     it with ON CONFLICT (id) DO NOTHING and leave it intact on teardown).
-- ============================================================================

BEGIN;

-- ----------------------------------------------------------------------------
-- 0. namespace_registry (already created by the user; backstop only).
-- ----------------------------------------------------------------------------
INSERT INTO namespace_registry (namespace, kind, description)
VALUES ('demo-test', 'business', 'dev e2e demo-test'),
       ('for_dev_agent_test', 'business', 'dev e2e for_dev_agent_test')
ON CONFLICT (namespace) DO NOTHING;

-- snapshot seq rows (config + abtest). Bumped so SDK incremental pulls notice
-- the seeded versions; first-snapshot acceptance does not require it, but it
-- keeps the data complete and harmless.
INSERT INTO namespace_snapshot_seq (namespace, seq)
VALUES ('demo-test', 1), ('for_dev_agent_test', 1)
ON CONFLICT (namespace) DO UPDATE SET seq = GREATEST(namespace_snapshot_seq.seq, EXCLUDED.seq);

INSERT INTO namespace_experiment_snapshot_seq (business_namespace, seq)
VALUES ('demo-test', 1), ('for_dev_agent_test', 1)
ON CONFLICT (business_namespace) DO UPDATE
    SET seq = GREATEST(namespace_experiment_snapshot_seq.seq, EXCLUDED.seq);

-- ============================================================================
-- NS-A: demo-test
-- ============================================================================

-- ----------------------------------------------------------------------------
-- A.1 config_key (UNIQUE(namespace,key); last_version_no set to max version_no).
-- ----------------------------------------------------------------------------
INSERT INTO config_key (id, namespace, key, category, sub_category, description, owner, status, last_version_no) VALUES
  (900100001, 'demo-test', 'welcome_text', 'e2e', '', 'dev e2e welcome text', 'e2e', 'active', 19),
  (900100002, 'demo-test', 'banner_color', 'e2e', '', 'dev e2e banner color', 'e2e', 'active', 29),
  (900100003, 'demo-test', 'gap_key',      'e2e', '', 'dev e2e gap key',      'e2e', 'active', 39),
  (900100004, 'demo-test', 'admit_key',    'e2e', '', 'dev e2e admit key',    'e2e', 'active', 49)
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key = EXCLUDED.key, category = EXCLUDED.category,
  description = EXCLUDED.description, owner = EXCLUDED.owner, status = EXCLUDED.status,
  last_version_no = EXCLUDED.last_version_no;

-- ----------------------------------------------------------------------------
-- A.2 config_version (UNIQUE(key_id,version_no); value is the resolved string).
-- version_no chosen = last two digits of the version_id for readability.
-- ----------------------------------------------------------------------------
INSERT INTO config_version (id, namespace, key_id, version_no, value, operator) VALUES
  -- welcome_text
  (900200011, 'demo-test', 900100001, 11, 'welcome-A',    'e2e'),
  (900200012, 'demo-test', 900100001, 12, 'welcome-B',    'e2e'),
  (900200013, 'demo-test', 900100001, 13, 'welcome-GRAY', 'e2e'),
  (900200019, 'demo-test', 900100001, 19, 'welcome-FULL', 'e2e'),
  -- banner_color
  (900200021, 'demo-test', 900100002, 21, 'red',   'e2e'),
  (900200022, 'demo-test', 900100002, 22, 'blue',  'e2e'),
  (900200029, 'demo-test', 900100002, 29, 'green', 'e2e'),
  -- gap_key
  (900200031, 'demo-test', 900100003, 31, 'gap-EXP',  'e2e'),
  (900200039, 'demo-test', 900100003, 39, 'gap-FULL', 'e2e'),
  -- admit_key
  (900200041, 'demo-test', 900100004, 41, 'admit-EXP',  'e2e'),
  (900200049, 'demo-test', 900100004, 49, 'admit-FULL', 'e2e')
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key_id = EXCLUDED.key_id, version_no = EXCLUDED.version_no,
  value = EXCLUDED.value, operator = EXCLUDED.operator;

-- ----------------------------------------------------------------------------
-- A.3 release_full (active full release per key; PARTIAL UNIQUE on
-- (namespace,key_id) WHERE status='active'). Fixed ids 900500xxx.
-- ----------------------------------------------------------------------------
INSERT INTO release_full (id, namespace, key_id, version_id, operator, status) VALUES
  (900500001, 'demo-test', 900100001, 900200019, 'e2e', 'active'),  -- welcome-FULL
  (900500002, 'demo-test', 900100002, 900200029, 'e2e', 'active'),  -- green
  (900500003, 'demo-test', 900100003, 900200039, 'e2e', 'active'),  -- gap-FULL
  (900500004, 'demo-test', 900100004, 900200049, 'e2e', 'active')   -- admit-FULL
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key_id = EXCLUDED.key_id, version_id = EXCLUDED.version_id,
  operator = EXCLUDED.operator, status = EXCLUDED.status;

-- ----------------------------------------------------------------------------
-- A.4 root domain (auto-created by the platform; backstop via PK). admission {}.
-- ----------------------------------------------------------------------------
INSERT INTO domain (id, business_namespace, name, is_root, admission, status)
VALUES ('root:demo-test', 'demo-test', '__root__', TRUE, '{}'::jsonb, 'active')
ON CONFLICT (id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- A.5 layers (each domain_id = root:demo-test).
-- ----------------------------------------------------------------------------
INSERT INTO layer (id, business_namespace, domain_id, name, salt, hash_fn, traffic_total, admission, status) VALUES
  ('demo-test:layer:cfg',    'demo-test', 'root:demo-test', 'cfg',    'L_cfg_demo',    'xxhash', 10000, '{}'::jsonb, 'active'),
  ('demo-test:layer:custom', 'demo-test', 'root:demo-test', 'custom', 'L_custom_demo', 'xxhash', 10000, '{}'::jsonb, 'active'),
  ('demo-test:layer:gap',    'demo-test', 'root:demo-test', 'gap',    'L_gap_demo',    'xxhash', 10000, '{}'::jsonb, 'active'),
  ('demo-test:layer:admit',  'demo-test', 'root:demo-test', 'admit',  'L_admit_demo',  'xxhash', 10000, '{}'::jsonb, 'active')
ON CONFLICT (id) DO UPDATE SET
  business_namespace = EXCLUDED.business_namespace, domain_id = EXCLUDED.domain_id,
  name = EXCLUDED.name, salt = EXCLUDED.salt, hash_fn = EXCLUDED.hash_fn,
  traffic_total = EXCLUDED.traffic_total, admission = EXCLUDED.admission, status = EXCLUDED.status;

-- ----------------------------------------------------------------------------
-- A.6 layer_slot (kind='experiment'; L_gap covers ONLY [0,4999]).
-- ----------------------------------------------------------------------------
INSERT INTO layer_slot (id, layer_id, kind, experiment_id, traffic_range_lo, traffic_range_hi) VALUES
  ('demo-test:slot:cfg',    'demo-test:layer:cfg',    'experiment', 'demo-test:exp:cfg',    0, 9999),
  ('demo-test:slot:custom', 'demo-test:layer:custom', 'experiment', 'demo-test:exp:custom', 0, 9999),
  ('demo-test:slot:gap',    'demo-test:layer:gap',    'experiment', 'demo-test:exp:gap',    0, 4999),
  ('demo-test:slot:admit',  'demo-test:layer:admit',  'experiment', 'demo-test:exp:admit',  0, 9999)
ON CONFLICT (id) DO UPDATE SET
  layer_id = EXCLUDED.layer_id, kind = EXCLUDED.kind, experiment_id = EXCLUDED.experiment_id,
  traffic_range_lo = EXCLUDED.traffic_range_lo, traffic_range_hi = EXCLUDED.traffic_range_hi;

-- ----------------------------------------------------------------------------
-- A.7 experiment (status='running' is required to produce output).
-- ----------------------------------------------------------------------------
INSERT INTO experiment (id, business_namespace, layer_id, type, name, status, salt, hash_fn, traffic_total, admission, sticky_enabled) VALUES
  ('demo-test:exp:cfg',    'demo-test', 'demo-test:layer:cfg',    'config_version', 'E_cfg',    'running', 'E_cfg_demo',    'xxhash', 10000, '{}'::jsonb, TRUE),
  ('demo-test:exp:custom', 'demo-test', 'demo-test:layer:custom', 'custom_params',  'E_custom', 'running', 'E_custom_demo', 'xxhash', 10000, '{}'::jsonb, FALSE),
  ('demo-test:exp:gap',    'demo-test', 'demo-test:layer:gap',    'config_version', 'E_gap',    'running', 'E_gap_demo',    'xxhash', 10000, '{}'::jsonb, FALSE),
  ('demo-test:exp:admit',  'demo-test', 'demo-test:layer:admit',  'config_version', 'E_admit',  'running', 'E_admit_demo',  'xxhash', 10000,
     '{"field":"country","op":"==","value":"US"}'::jsonb, FALSE)
ON CONFLICT (id) DO UPDATE SET
  business_namespace = EXCLUDED.business_namespace, layer_id = EXCLUDED.layer_id, type = EXCLUDED.type,
  name = EXCLUDED.name, status = EXCLUDED.status, salt = EXCLUDED.salt, hash_fn = EXCLUDED.hash_fn,
  traffic_total = EXCLUDED.traffic_total, admission = EXCLUDED.admission, sticky_enabled = EXCLUDED.sticky_enabled;

-- ----------------------------------------------------------------------------
-- A.8 experiment_group (UNIQUE(experiment_id,name); params JSONB.
-- config_version params keys are config_key.id as DECIMAL STRINGS, values are
-- version_id as strings).
-- ----------------------------------------------------------------------------
INSERT INTO experiment_group (id, experiment_id, name, traffic_range_lo, traffic_range_hi, admission, params) VALUES
  -- E_cfg A / B
  ('demo-test:grp:cfg:A', 'demo-test:exp:cfg', 'A', 0,    4999, '{}'::jsonb, '{"900100001":"900200011","900100002":"900200021"}'::jsonb),
  ('demo-test:grp:cfg:B', 'demo-test:exp:cfg', 'B', 5000, 9999, '{}'::jsonb, '{"900100001":"900200012","900100002":"900200022"}'::jsonb),
  -- E_custom C / D (custom_params: arbitrary KV)
  ('demo-test:grp:custom:C', 'demo-test:exp:custom', 'C', 0,    2999, '{}'::jsonb, '{"variant":"A","max_items":10,"enabled":true}'::jsonb),
  ('demo-test:grp:custom:D', 'demo-test:exp:custom', 'D', 3000, 9999, '{}'::jsonb, '{"variant":"B","max_items":20,"enabled":false}'::jsonb),
  -- E_gap single group G
  ('demo-test:grp:gap:G', 'demo-test:exp:gap', 'G', 0, 9999, '{}'::jsonb, '{"900100003":"900200031"}'::jsonb),
  -- E_admit single group A
  ('demo-test:grp:admit:A', 'demo-test:exp:admit', 'A', 0, 9999, '{}'::jsonb, '{"900100004":"900200041"}'::jsonb)
ON CONFLICT (id) DO UPDATE SET
  experiment_id = EXCLUDED.experiment_id, name = EXCLUDED.name,
  traffic_range_lo = EXCLUDED.traffic_range_lo, traffic_range_hi = EXCLUDED.traffic_range_hi,
  admission = EXCLUDED.admission, params = EXCLUDED.params;

-- ----------------------------------------------------------------------------
-- A.9 experiment_group_whitelist_uid: wl-force-B forced into E_cfg group B
-- (UNIQUE(group_id,uid)).
-- ----------------------------------------------------------------------------
INSERT INTO experiment_group_whitelist_uid (group_id, uid)
VALUES ('demo-test:grp:cfg:B', 'wl-force-B')
ON CONFLICT (group_id, uid) DO NOTHING;

-- ----------------------------------------------------------------------------
-- A.10 experiment_config_param: DERIVED index of config_version group params.
-- One row per (group,key); experiment_status MUST equal the experiment status
-- ('running') for the row to enter GPV (and thus the SDK snapshot). custom_params
-- experiments do NOT contribute rows here. (UNIQUE(group_id,key_id)).
-- ----------------------------------------------------------------------------
INSERT INTO experiment_config_param (business_namespace, layer_id, experiment_id, group_id, experiment_status, key_id, version_id) VALUES
  -- E_cfg A
  ('demo-test', 'demo-test:layer:cfg', 'demo-test:exp:cfg', 'demo-test:grp:cfg:A', 'running', 900100001, 900200011),
  ('demo-test', 'demo-test:layer:cfg', 'demo-test:exp:cfg', 'demo-test:grp:cfg:A', 'running', 900100002, 900200021),
  -- E_cfg B
  ('demo-test', 'demo-test:layer:cfg', 'demo-test:exp:cfg', 'demo-test:grp:cfg:B', 'running', 900100001, 900200012),
  ('demo-test', 'demo-test:layer:cfg', 'demo-test:exp:cfg', 'demo-test:grp:cfg:B', 'running', 900100002, 900200022),
  -- E_gap G
  ('demo-test', 'demo-test:layer:gap', 'demo-test:exp:gap', 'demo-test:grp:gap:G', 'running', 900100003, 900200031),
  -- E_admit A
  ('demo-test', 'demo-test:layer:admit', 'demo-test:exp:admit', 'demo-test:grp:admit:A', 'running', 900100004, 900200041)
ON CONFLICT (group_id, key_id) DO UPDATE SET
  business_namespace = EXCLUDED.business_namespace, layer_id = EXCLUDED.layer_id,
  experiment_id = EXCLUDED.experiment_id, experiment_status = EXCLUDED.experiment_status,
  version_id = EXCLUDED.version_id;

-- ----------------------------------------------------------------------------
-- A.11 layer_key_claim: key → owning layer (UNIQUE(business_namespace,key_id),
-- GLOBAL per ns). Only config_version keys are claimed; custom_params claims none.
-- ----------------------------------------------------------------------------
INSERT INTO layer_key_claim (business_namespace, key_id, layer_id) VALUES
  ('demo-test', 900100001, 'demo-test:layer:cfg'),   -- welcome_text → cfg
  ('demo-test', 900100002, 'demo-test:layer:cfg'),   -- banner_color → cfg
  ('demo-test', 900100003, 'demo-test:layer:gap'),   -- gap_key → gap
  ('demo-test', 900100004, 'demo-test:layer:admit')  -- admit_key → admit
ON CONFLICT (business_namespace, key_id) DO UPDATE SET layer_id = EXCLUDED.layer_id;

-- ----------------------------------------------------------------------------
-- A.12 gray release on welcome_text → welcome-GRAY (900200013), uids
-- gray-user-1, gray-user-2. release_whitelist id 900600001.
-- ----------------------------------------------------------------------------
INSERT INTO release_whitelist (id, namespace, key_id, version_id, operator, status)
VALUES (900600001, 'demo-test', 900100001, 900200013, 'e2e', 'active')
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key_id = EXCLUDED.key_id, version_id = EXCLUDED.version_id,
  operator = EXCLUDED.operator, status = EXCLUDED.status;

INSERT INTO release_whitelist_uid (id, release_id, uid) VALUES
  (900600101, 900600001, 'gray-user-1'),
  (900600102, 900600001, 'gray-user-2')
ON CONFLICT (id) DO UPDATE SET release_id = EXCLUDED.release_id, uid = EXCLUDED.uid;

-- release_whitelist_uid_active: functionally optional for the read path
-- (compute/GPV never read it; it is a write-time uniqueness backstop). Inserted
-- for data completeness. UNIQUE(namespace,key_id,uid).
INSERT INTO release_whitelist_uid_active (namespace, key_id, uid, release_id) VALUES
  ('demo-test', 900100001, 'gray-user-1', 900600001),
  ('demo-test', 900100001, 'gray-user-2', 900600001)
ON CONFLICT (namespace, key_id, uid) DO UPDATE SET release_id = EXCLUDED.release_id;

-- ============================================================================
-- NS-B: for_dev_agent_test
-- ============================================================================

-- ----------------------------------------------------------------------------
-- B.1 config_key.
-- ----------------------------------------------------------------------------
INSERT INTO config_key (id, namespace, key, category, sub_category, description, owner, status, last_version_no) VALUES
  (900300001, 'for_dev_agent_test', 'color',    'e2e', '', 'dev e2e color',    'e2e', 'active', 19),
  (900300002, 'for_dev_agent_test', 'greeting', 'e2e', '', 'dev e2e greeting', 'e2e', 'active', 29)
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key = EXCLUDED.key, category = EXCLUDED.category,
  description = EXCLUDED.description, owner = EXCLUDED.owner, status = EXCLUDED.status,
  last_version_no = EXCLUDED.last_version_no;

-- ----------------------------------------------------------------------------
-- B.2 config_version.
-- ----------------------------------------------------------------------------
INSERT INTO config_version (id, namespace, key_id, version_no, value, operator) VALUES
  -- color
  (900400011, 'for_dev_agent_test', 900300001, 11, 'c1',     'e2e'),
  (900400012, 'for_dev_agent_test', 900300001, 12, 'c2',     'e2e'),
  (900400013, 'for_dev_agent_test', 900300001, 13, 'c3',     'e2e'),
  (900400019, 'for_dev_agent_test', 900300001, 19, 'c-FULL', 'e2e'),
  -- greeting
  (900400021, 'for_dev_agent_test', 900300002, 21, 'hi-GRAY', 'e2e'),
  (900400029, 'for_dev_agent_test', 900300002, 29, 'hi-FULL', 'e2e')
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key_id = EXCLUDED.key_id, version_no = EXCLUDED.version_no,
  value = EXCLUDED.value, operator = EXCLUDED.operator;

-- ----------------------------------------------------------------------------
-- B.3 release_full.
-- ----------------------------------------------------------------------------
INSERT INTO release_full (id, namespace, key_id, version_id, operator, status) VALUES
  (900500011, 'for_dev_agent_test', 900300001, 900400019, 'e2e', 'active'),  -- c-FULL
  (900500012, 'for_dev_agent_test', 900300002, 900400029, 'e2e', 'active')   -- hi-FULL
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key_id = EXCLUDED.key_id, version_id = EXCLUDED.version_id,
  operator = EXCLUDED.operator, status = EXCLUDED.status;

-- ----------------------------------------------------------------------------
-- B.4 root domain.
-- ----------------------------------------------------------------------------
INSERT INTO domain (id, business_namespace, name, is_root, admission, status)
VALUES ('root:for_dev_agent_test', 'for_dev_agent_test', '__root__', TRUE, '{}'::jsonb, 'active')
ON CONFLICT (id) DO NOTHING;

-- ----------------------------------------------------------------------------
-- B.5 layers.
-- ----------------------------------------------------------------------------
INSERT INTO layer (id, business_namespace, domain_id, name, salt, hash_fn, traffic_total, admission, status) VALUES
  ('for_dev_agent_test:layer:cfg',    'for_dev_agent_test', 'root:for_dev_agent_test', 'cfg',    'L_cfg_fda',    'xxhash', 10000, '{}'::jsonb, 'active'),
  ('for_dev_agent_test:layer:custom', 'for_dev_agent_test', 'root:for_dev_agent_test', 'custom', 'L_custom_fda', 'xxhash', 10000, '{}'::jsonb, 'active')
ON CONFLICT (id) DO UPDATE SET
  business_namespace = EXCLUDED.business_namespace, domain_id = EXCLUDED.domain_id,
  name = EXCLUDED.name, salt = EXCLUDED.salt, hash_fn = EXCLUDED.hash_fn,
  traffic_total = EXCLUDED.traffic_total, admission = EXCLUDED.admission, status = EXCLUDED.status;

-- ----------------------------------------------------------------------------
-- B.6 layer_slot (both cover [0,9999]).
-- ----------------------------------------------------------------------------
INSERT INTO layer_slot (id, layer_id, kind, experiment_id, traffic_range_lo, traffic_range_hi) VALUES
  ('for_dev_agent_test:slot:cfg',    'for_dev_agent_test:layer:cfg',    'experiment', 'for_dev_agent_test:exp:cfg',    0, 9999),
  ('for_dev_agent_test:slot:custom', 'for_dev_agent_test:layer:custom', 'experiment', 'for_dev_agent_test:exp:custom', 0, 9999)
ON CONFLICT (id) DO UPDATE SET
  layer_id = EXCLUDED.layer_id, kind = EXCLUDED.kind, experiment_id = EXCLUDED.experiment_id,
  traffic_range_lo = EXCLUDED.traffic_range_lo, traffic_range_hi = EXCLUDED.traffic_range_hi;

-- ----------------------------------------------------------------------------
-- B.7 experiment.
-- ----------------------------------------------------------------------------
INSERT INTO experiment (id, business_namespace, layer_id, type, name, status, salt, hash_fn, traffic_total, admission, sticky_enabled) VALUES
  ('for_dev_agent_test:exp:cfg',    'for_dev_agent_test', 'for_dev_agent_test:layer:cfg',    'config_version', 'E_cfg2',    'running', 'E_cfg_fda',    'xxhash', 10000, '{}'::jsonb, FALSE),
  ('for_dev_agent_test:exp:custom', 'for_dev_agent_test', 'for_dev_agent_test:layer:custom', 'custom_params',  'E_custom2', 'running', 'E_custom_fda', 'xxhash', 10000, '{}'::jsonb, FALSE)
ON CONFLICT (id) DO UPDATE SET
  business_namespace = EXCLUDED.business_namespace, layer_id = EXCLUDED.layer_id, type = EXCLUDED.type,
  name = EXCLUDED.name, status = EXCLUDED.status, salt = EXCLUDED.salt, hash_fn = EXCLUDED.hash_fn,
  traffic_total = EXCLUDED.traffic_total, admission = EXCLUDED.admission, sticky_enabled = EXCLUDED.sticky_enabled;

-- ----------------------------------------------------------------------------
-- B.8 experiment_group. E_cfg2 G1/G2/G3 cover [0,9998]; bucket 9999 is an
-- INTENTIONAL experiment-internal gap → color falls back to c-FULL.
-- ----------------------------------------------------------------------------
INSERT INTO experiment_group (id, experiment_id, name, traffic_range_lo, traffic_range_hi, admission, params) VALUES
  ('for_dev_agent_test:grp:cfg:G1', 'for_dev_agent_test:exp:cfg', 'G1', 0,    3332, '{}'::jsonb, '{"900300001":"900400011"}'::jsonb),
  ('for_dev_agent_test:grp:cfg:G2', 'for_dev_agent_test:exp:cfg', 'G2', 3333, 6665, '{}'::jsonb, '{"900300001":"900400012"}'::jsonb),
  ('for_dev_agent_test:grp:cfg:G3', 'for_dev_agent_test:exp:cfg', 'G3', 6666, 9998, '{}'::jsonb, '{"900300001":"900400013"}'::jsonb),
  ('for_dev_agent_test:grp:custom:gold',   'for_dev_agent_test:exp:custom', 'gold',   0,    4999, '{}'::jsonb, '{"tier":"gold","weight":1.5}'::jsonb),
  ('for_dev_agent_test:grp:custom:silver', 'for_dev_agent_test:exp:custom', 'silver', 5000, 9999, '{}'::jsonb, '{"tier":"silver","weight":2.5}'::jsonb)
ON CONFLICT (id) DO UPDATE SET
  experiment_id = EXCLUDED.experiment_id, name = EXCLUDED.name,
  traffic_range_lo = EXCLUDED.traffic_range_lo, traffic_range_hi = EXCLUDED.traffic_range_hi,
  admission = EXCLUDED.admission, params = EXCLUDED.params;

-- ----------------------------------------------------------------------------
-- B.9 experiment_config_param (derived; config_version only).
-- ----------------------------------------------------------------------------
INSERT INTO experiment_config_param (business_namespace, layer_id, experiment_id, group_id, experiment_status, key_id, version_id) VALUES
  ('for_dev_agent_test', 'for_dev_agent_test:layer:cfg', 'for_dev_agent_test:exp:cfg', 'for_dev_agent_test:grp:cfg:G1', 'running', 900300001, 900400011),
  ('for_dev_agent_test', 'for_dev_agent_test:layer:cfg', 'for_dev_agent_test:exp:cfg', 'for_dev_agent_test:grp:cfg:G2', 'running', 900300001, 900400012),
  ('for_dev_agent_test', 'for_dev_agent_test:layer:cfg', 'for_dev_agent_test:exp:cfg', 'for_dev_agent_test:grp:cfg:G3', 'running', 900300001, 900400013)
ON CONFLICT (group_id, key_id) DO UPDATE SET
  business_namespace = EXCLUDED.business_namespace, layer_id = EXCLUDED.layer_id,
  experiment_id = EXCLUDED.experiment_id, experiment_status = EXCLUDED.experiment_status,
  version_id = EXCLUDED.version_id;

-- ----------------------------------------------------------------------------
-- B.10 layer_key_claim (color → cfg; greeting is gray-only, not claimed by a
-- layer/experiment).
-- ----------------------------------------------------------------------------
INSERT INTO layer_key_claim (business_namespace, key_id, layer_id) VALUES
  ('for_dev_agent_test', 900300001, 'for_dev_agent_test:layer:cfg')  -- color → cfg
ON CONFLICT (business_namespace, key_id) DO UPDATE SET layer_id = EXCLUDED.layer_id;

-- ----------------------------------------------------------------------------
-- B.11 gray release on greeting → hi-GRAY (900400021), uid gray-fda-1.
-- ----------------------------------------------------------------------------
INSERT INTO release_whitelist (id, namespace, key_id, version_id, operator, status)
VALUES (900600011, 'for_dev_agent_test', 900300002, 900400021, 'e2e', 'active')
ON CONFLICT (id) DO UPDATE SET
  namespace = EXCLUDED.namespace, key_id = EXCLUDED.key_id, version_id = EXCLUDED.version_id,
  operator = EXCLUDED.operator, status = EXCLUDED.status;

INSERT INTO release_whitelist_uid (id, release_id, uid) VALUES
  (900600111, 900600011, 'gray-fda-1')
ON CONFLICT (id) DO UPDATE SET release_id = EXCLUDED.release_id, uid = EXCLUDED.uid;

INSERT INTO release_whitelist_uid_active (namespace, key_id, uid, release_id) VALUES
  ('for_dev_agent_test', 900300002, 'gray-fda-1', 900600011)
ON CONFLICT (namespace, key_id, uid) DO UPDATE SET release_id = EXCLUDED.release_id;

COMMIT;

-- ============================================================================
-- SELF-CHECK QUERIES (run AFTER COMMIT; these are SELECTs, not mutations).
-- They verify the hand-written derived table (experiment_config_param) matches
-- the experiment_group.params source of truth AND that experiment_status mirrors
-- the experiment.status. Expectation: BOTH queries return ZERO rows.
--
-- Read them with:
--   psql "$DATABASE_URL" -f test/dev-e2e/sql/seed.sql   (runs the checks too)
-- A non-empty result = a hand-written mistake to fix before testing.
-- ============================================================================

-- CHECK 1: every config_version group param has a matching derived row, and
-- vice versa (symmetric difference must be empty). Compares (group_id,key_id,
-- version_id) of the JSONB params against experiment_config_param.
WITH params_expanded AS (
    SELECT g.id AS group_id,
           (kv.key)::bigint           AS key_id,
           (kv.value #>> '{}')::bigint AS version_id
    FROM experiment_group g
    JOIN experiment e ON e.id = g.experiment_id
    CROSS JOIN LATERAL jsonb_each(g.params) AS kv
    WHERE e.type = 'config_version'
      AND e.business_namespace IN ('demo-test', 'for_dev_agent_test')
      AND g.id LIKE ANY (ARRAY['demo-test:grp:%', 'for_dev_agent_test:grp:%'])
),
derived AS (
    SELECT group_id, key_id, version_id
    FROM experiment_config_param
    WHERE business_namespace IN ('demo-test', 'for_dev_agent_test')
)
SELECT 'MISMATCH params<->derived' AS check_name, *
FROM (
    (
        SELECT group_id, key_id, version_id FROM params_expanded
        EXCEPT
        SELECT group_id, key_id, version_id FROM derived
    )
    UNION ALL
    (
        SELECT group_id, key_id, version_id FROM derived
        EXCEPT
        SELECT group_id, key_id, version_id FROM params_expanded
    )
) AS diff;

-- CHECK 2: every derived row's experiment_status equals the live experiment
-- status (must be 'running' to enter GPV). Returns rows on any drift.
SELECT 'STATUS DRIFT' AS check_name, p.group_id, p.experiment_status, e.status AS experiment_status_live
FROM experiment_config_param p
JOIN experiment e ON e.id = p.experiment_id
WHERE p.business_namespace IN ('demo-test', 'for_dev_agent_test')
  AND p.experiment_status <> e.status;
