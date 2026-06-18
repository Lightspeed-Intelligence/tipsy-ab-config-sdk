-- ============================================================================
-- dev e2e teardown.sql — remove THIS BATCH's seeded rows in FK-safe order.
--
-- HOW TO RUN (user executes against the dev DATABASE_URL):
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/teardown.sql
--
-- IDEMPOTENT: every DELETE is scoped by namespace IN ('demo-test',
-- 'for_dev_agent_test') and/or the fixed high-band ids this batch uses, so
-- re-running (or running when already clean) is a harmless no-op.
--
-- SCOPE: deletes the seeded layers / slots / experiments / groups / configs /
-- releases / derived rows ONLY. It deliberately does NOT delete:
--   - namespace_registry rows (the namespaces stay usable), or
--   - the auto-created root domain ("root:<ns>") — leaving the namespaces in
--     their post-EnsureExperimentNamespace state.
-- It also leaves the snapshot_seq rows in place (monotonic counters; harmless).
--
-- After teardown WAIT >= 5s for the dev cache to reload before asserting the
-- namespaces are empty again (regression check).
-- ============================================================================

BEGIN;

-- ----------------------------------------------------------------------------
-- Derived / materialized abtest tables first (no inbound FKs; scoped by ns).
-- ----------------------------------------------------------------------------
DELETE FROM experiment_config_param
 WHERE business_namespace IN ('demo-test', 'for_dev_agent_test');

DELETE FROM layer_key_claim
 WHERE business_namespace IN ('demo-test', 'for_dev_agent_test');

DELETE FROM experiment_group_whitelist_uid
 WHERE group_id LIKE 'demo-test:grp:%'
    OR group_id LIKE 'for_dev_agent_test:grp:%';

-- Sticky rows: server-side compute may write these during testing. Clean any
-- this batch's experiments produced so a re-seed/re-test starts fresh.
DELETE FROM experiment_sticky_assignment
 WHERE business_namespace IN ('demo-test', 'for_dev_agent_test')
   AND (experiment_id LIKE 'demo-test:exp:%'
        OR experiment_id LIKE 'for_dev_agent_test:exp:%');

-- ----------------------------------------------------------------------------
-- experiment_group → experiment → layer_slot → layer (parent order).
-- ----------------------------------------------------------------------------
DELETE FROM experiment_group
 WHERE id LIKE 'demo-test:grp:%'
    OR id LIKE 'for_dev_agent_test:grp:%';

DELETE FROM experiment
 WHERE id LIKE 'demo-test:exp:%'
    OR id LIKE 'for_dev_agent_test:exp:%';

DELETE FROM layer_slot
 WHERE id LIKE 'demo-test:slot:%'
    OR id LIKE 'for_dev_agent_test:slot:%';

DELETE FROM layer
 WHERE id LIKE 'demo-test:layer:%'
    OR id LIKE 'for_dev_agent_test:layer:%';

-- NOTE: root domain ("root:<ns>") is intentionally NOT deleted.

-- ----------------------------------------------------------------------------
-- Gray-release tables (scoped by this batch's fixed id bands).
-- ----------------------------------------------------------------------------
DELETE FROM release_whitelist_uid_active
 WHERE namespace IN ('demo-test', 'for_dev_agent_test')
   AND key_id IN (900100001, 900300002);

DELETE FROM release_whitelist_uid
 WHERE id IN (900600101, 900600102, 900600111)
    OR release_id IN (900600001, 900600011);

DELETE FROM release_whitelist
 WHERE id IN (900600001, 900600011);

-- ----------------------------------------------------------------------------
-- config-module tables: release_full → config_version → config_key.
-- ----------------------------------------------------------------------------
DELETE FROM release_full
 WHERE id IN (900500001, 900500002, 900500003, 900500004, 900500011, 900500012);

DELETE FROM config_version
 WHERE id BETWEEN 900200001 AND 900200999
    OR id BETWEEN 900400001 AND 900400999;

DELETE FROM config_key
 WHERE id IN (900100001, 900100002, 900100003, 900100004, 900300001, 900300002);

COMMIT;

-- ============================================================================
-- REGRESSION CHECK (run AFTER COMMIT + >=5s cache reload). Expect ZERO rows:
-- the two namespaces should be back to empty (no keys / experiments / layers).
-- ============================================================================
SELECT 'LEFTOVER config_key' AS check_name, id::text AS id, namespace, key
  FROM config_key
 WHERE namespace IN ('demo-test', 'for_dev_agent_test')
UNION ALL
SELECT 'LEFTOVER experiment', id::text, business_namespace, name
  FROM experiment
 WHERE business_namespace IN ('demo-test', 'for_dev_agent_test')
UNION ALL
SELECT 'LEFTOVER layer', id::text, business_namespace, name
  FROM layer
 WHERE business_namespace IN ('demo-test', 'for_dev_agent_test');
