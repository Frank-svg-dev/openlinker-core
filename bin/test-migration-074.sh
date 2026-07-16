#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="$ROOT_DIR/migrations"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16}"
CONTAINER_NAME="openlinker-migration-074-${PPID}-$$"
DATABASE_NAME="openlinker"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "migration 074 test failed: $*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "docker daemon is not available"

docker run --detach --name "$CONTAINER_NAME" \
  --env POSTGRES_HOST_AUTH_METHOD=trust \
  --env POSTGRES_DB="$DATABASE_NAME" \
  --volume "$MIGRATIONS_DIR:/migrations:ro" \
  "$POSTGRES_IMAGE" >/dev/null

for _ in $(seq 1 60); do
  if docker exec "$CONTAINER_NAME" pg_isready -U postgres -d "$DATABASE_NAME" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$CONTAINER_NAME" pg_isready -U postgres -d "$DATABASE_NAME" >/dev/null 2>&1 \
  || fail "postgres did not become ready"

psql_stdin() {
  docker exec -i --env PGOPTIONS="-c client_min_messages=warning" "$CONTAINER_NAME" \
    psql -X -v ON_ERROR_STOP=1 -U postgres -d "$DATABASE_NAME" "$@"
}

psql_command() {
  docker exec --env PGOPTIONS="-c client_min_messages=warning" "$CONTAINER_NAME" \
    psql -X -v ON_ERROR_STOP=1 -U postgres -d "$DATABASE_NAME" "$@"
}

expect_sql_failure() {
  local expected="$1"
  local sql="$2"
  local output status
  set +e
  output="$(psql_command --quiet -c "$sql" 2>&1)"
  status=$?
  set -e
  if ((status == 0)); then
    fail "SQL unexpectedly succeeded; expected: $expected"
  fi
  [[ "$output" == *"$expected"* ]] \
    || fail "SQL failed for the wrong reason; expected '$expected', got: $output"
}

reset_database() {
  docker exec "$CONTAINER_NAME" \
    psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres --quiet \
      -c "DROP DATABASE IF EXISTS $DATABASE_NAME WITH (FORCE)" \
      -c "CREATE DATABASE $DATABASE_NAME"
}

run_migration() {
  psql_stdin --quiet <"$1"
}

apply_through_073() {
  local migration_path migration_name version
  for migration_path in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.up.sql; do
    migration_name="$(basename "$migration_path")"
    version="${migration_name%%_*}"
    if ((10#$version <= 73)); then
      run_migration "$migration_path" >/dev/null
    fi
  done
}

apply_074() {
  run_migration "$MIGRATIONS_DIR/074_external_execution_boundary.up.sql" >/dev/null
}

revert_074() {
  run_migration "$MIGRATIONS_DIR/074_external_execution_boundary.down.sql" >/dev/null
}

expect_apply_failure() {
  local expected="$1"
  local output status
  set +e
  output="$(apply_074 2>&1)"
  status=$?
  set -e
  if ((status == 0)); then
    fail "074 unexpectedly succeeded; expected: $expected"
  fi
  [[ "$output" == *"$expected"* ]] \
    || fail "074 failed for the wrong reason; expected '$expected', got: $output"
}

expect_revert_failure() {
  local expected="$1"
  local output status
  set +e
  output="$(revert_074 2>&1)"
  status=$?
  set -e
  if ((status == 0)); then
    fail "074 rollback unexpectedly succeeded; expected: $expected"
  fi
  [[ "$output" == *"$expected"* ]] \
    || fail "074 rollback failed for the wrong reason; expected '$expected', got: $output"
}

assert_failed_up_was_atomic() {
  local expected_rows="$1"
  psql_stdin --quiet <<SQL
DO \$\$
BEGIN
  IF to_regclass('hosted_service_executions') IS NULL
     OR to_regclass('external_executions') IS NOT NULL THEN
    RAISE EXCEPTION 'failed 074 changed the legacy table identity';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'hosted_service_executions'
      AND column_name = 'seller_user_id'
  ) OR EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'hosted_service_executions'
      AND column_name = 'caller_service_id'
  ) THEN
    RAISE EXCEPTION 'failed 074 changed legacy columns';
  END IF;
  IF (SELECT count(*) FROM hosted_service_executions) <> $expected_rows THEN
    RAISE EXCEPTION 'failed 074 changed legacy row count';
  END IF;
  IF to_regclass('idx_hosted_service_executions_seller') IS NULL THEN
    RAISE EXCEPTION 'failed 074 changed legacy indexes';
  END IF;
END
\$\$;
SQL
}

assert_failed_down_was_atomic() {
  local expected_rows="$1"
  psql_stdin --quiet <<SQL
DO \$\$
BEGIN
  IF to_regclass('external_executions') IS NULL
     OR to_regclass('hosted_service_executions') IS NOT NULL THEN
    RAISE EXCEPTION 'failed 074 rollback changed the generic table identity';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'external_executions'
      AND column_name = 'caller_service_id'
  ) OR EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'external_executions'
      AND column_name = 'seller_user_id'
  ) THEN
    RAISE EXCEPTION 'failed 074 rollback changed generic columns';
  END IF;
  IF (SELECT count(*) FROM external_executions) <> $expected_rows THEN
    RAISE EXCEPTION 'failed 074 rollback changed generic row count';
  END IF;
  IF to_regclass('idx_external_executions_actor') IS NULL
     OR to_regclass('idx_external_executions_execution') IS NULL THEN
    RAISE EXCEPTION 'failed 074 rollback changed generic indexes';
  END IF;
END
\$\$;
SQL
}

insert_principals() {
  psql_stdin --quiet <<'SQL'
INSERT INTO users (id, email, password_hash, display_name, is_creator)
VALUES
  ('74000000-0000-4000-8000-000000000001', 'migration-074-buyer@example.test', 'test-hash', 'Buyer', FALSE),
  ('74000000-0000-4000-8000-000000000002', 'migration-074-owner@example.test', 'test-hash', 'Owner', TRUE),
  ('74000000-0000-4000-8000-000000000003', 'migration-074-wrong@example.test', 'test-hash', 'Wrong Owner', TRUE);

INSERT INTO agents (
  id, creator_id, slug, name, description, endpoint_url,
  price_per_call_cents, connection_mode
) VALUES (
  '74000000-0000-4000-8000-000000000010',
  '74000000-0000-4000-8000-000000000002',
  'migration-074-agent', 'Migration 074 Agent', 'Owner gate fixture',
  'openlinker-runtime://migration-074-agent', 0, 'runtime'
);

INSERT INTO workflows (id, user_id, name, description)
VALUES (
  '74000000-0000-4000-8000-000000000020',
  '74000000-0000-4000-8000-000000000002',
  'Migration 074 Workflow', 'Owner gate fixture'
);
SQL
}

echo "[074] apply predecessor migrations"
apply_through_073

echo "[074] fail closed outside hard maintenance"
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'normal' WHERE singleton_id = 1" >/dev/null
expect_apply_failure "migration 074 requires hard maintenance"
assert_failed_up_was_atomic 0
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1" >/dev/null

echo "[074] fail closed while a Core member is registered"
psql_command --quiet -c "
  INSERT INTO runtime_cluster_members (
    instance_id, release_version, release_commit, schema_version,
    schema_checksum, runtime_contract_id, runtime_contract_digest
  )
  SELECT
    '74000000-0000-4000-8000-000000000098', 'test', 'test', schema_version,
    repeat('a', 64), runtime_contract_id, runtime_contract_digest
  FROM runtime_schema_contracts
  WHERE is_current" >/dev/null
expect_apply_failure "migration 074 requires zero registered Core cluster members"
assert_failed_up_was_atomic 0
psql_command --quiet -c "DELETE FROM runtime_cluster_members" >/dev/null

insert_principals

echo "[074] fail closed while a Run is still running"
psql_command --quiet -c "
  INSERT INTO runs (
    id, user_id, agent_id, input, status, cost_cents,
    platform_fee_cents, creator_revenue_cents, source,
    idempotency_key_hash, idempotency_fingerprint,
    connection_mode_snapshot, dispatch_deadline_at, run_deadline_at
  ) VALUES (
    '74000000-0000-4000-8000-000000000040',
    '74000000-0000-4000-8000-000000000001',
    '74000000-0000-4000-8000-000000000010',
    '{\"case\":\"migration-074-running\"}', 'running', 0, 0, 0, 'api',
    decode(repeat('71', 32), 'hex'), decode(repeat('72', 32), 'hex'),
    'runtime', clock_timestamp() + interval '2 minutes',
    clock_timestamp() + interval '10 minutes'
  )" >/dev/null
expect_apply_failure "migration 074 requires zero running Runs"
assert_failed_up_was_atomic 0

# Runtime v2 deliberately forbids physical Run deletion. Rebuild the
# predecessor database after exercising the running-Run gate so the successful
# migration path starts from a valid, quiescent snapshot.
reset_database
apply_through_073
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1" >/dev/null
insert_principals

echo "[074] reconcile crash-window rows and preserve historical deleted targets"
psql_stdin --quiet <<'SQL'
INSERT INTO hosted_service_executions (
  external_order_id, buyer_user_id, seller_user_id, target_type,
  target_id, input_fingerprint, trace_id, execution_kind, execution_id
) VALUES (
  '74000000-0000-4000-8000-000000000100',
  '74000000-0000-4000-8000-000000000001',
  '74000000-0000-4000-8000-000000000002',
  'agent', '74000000-0000-4000-8000-000000000099',
  decode(repeat('11', 32), 'hex'), 'migration-074-deleted-target',
  'run', '74000000-0000-4000-8000-000000000110'
), (
  '74000000-0000-4000-8000-000000000102',
  '74000000-0000-4000-8000-000000000001',
  '74000000-0000-4000-8000-000000000002',
  'workflow', '74000000-0000-4000-8000-000000000020',
  decode(repeat('33', 32), 'hex'), 'migration-074-workflow-crash-window',
  NULL, NULL
), (
	'74000000-0000-4000-8000-000000000103',
	'74000000-0000-4000-8000-000000000001',
	'74000000-0000-4000-8000-000000000002',
	'agent', '74000000-0000-4000-8000-000000000010',
	decode(repeat('44', 32), 'hex'), 'migration-074-reservation-only',
	NULL, NULL
), (
	'74000000-0000-4000-8000-000000000104',
	'74000000-0000-4000-8000-000000000001',
	'74000000-0000-4000-8000-000000000002',
	'agent', '74000000-0000-4000-8000-000000000010',
	decode(repeat('55', 32), 'hex'), 'migration-074-colliding-run',
	NULL, NULL
), (
	'74000000-0000-4000-8000-000000000105',
	'74000000-0000-4000-8000-000000000001',
	'74000000-0000-4000-8000-000000000002',
	'agent', '74000000-0000-4000-8000-000000000010',
	decode(repeat('66', 32), 'hex'), 'migration-074-agent-crash-window',
	NULL, NULL
);

INSERT INTO workflow_runs (id, workflow_id, user_id, status, input)
VALUES (
  '74000000-0000-4000-8000-000000000102',
  '74000000-0000-4000-8000-000000000020',
  '74000000-0000-4000-8000-000000000001',
	'success', '{}'::jsonb
);

-- The fixture represents already-terminal Runs written by the historical
-- Runtime service. Build that durable snapshot directly; the live v2 insert
-- trigger intentionally only accepts the initial running/pending shape.
ALTER TABLE runs DISABLE TRIGGER runs_v2_contract_identity;
BEGIN;
SET CONSTRAINTS ALL DEFERRED;

INSERT INTO runs (
	id, user_id, agent_id, input, status,
	cost_cents, platform_fee_cents, creator_revenue_cents, source,
	runtime_contract_id, idempotency_key_hash, idempotency_fingerprint,
	request_metadata, connection_mode_snapshot, dispatch_state,
	dispatch_deadline_at, run_deadline_at, finished_at, terminal_event_id
) VALUES (
	'74000000-0000-4000-8000-000000000114',
	'74000000-0000-4000-8000-000000000001',
	'74000000-0000-4000-8000-000000000010',
	'{}'::jsonb, 'timeout', 0, 0, 0, 'api', 'openlinker.runtime.v2',
	digest('hosted-service-order/74000000-0000-4000-8000-000000000104', 'sha256'),
	decode(repeat('74', 32), 'hex'),
	jsonb_build_object(
		'external_order_id', '74000000-0000-4000-8000-000000000104',
		'seller_user_id', '74000000-0000-4000-8000-000000000002',
		'trace_id', 'migration-074-colliding-run'
	),
	'runtime', 'terminal', clock_timestamp(), clock_timestamp(),
	clock_timestamp(), '74000000-0000-4000-8000-000000000124'
), (
	'74000000-0000-4000-8000-000000000115',
	'74000000-0000-4000-8000-000000000001',
	'74000000-0000-4000-8000-000000000010',
	'{}'::jsonb, 'timeout', 0, 0, 0, 'api', 'openlinker.runtime.v2',
	digest('hosted-service-order/74000000-0000-4000-8000-000000000105', 'sha256'),
	decode(repeat('75', 32), 'hex'),
	jsonb_build_object(
		'external_order_id', '74000000-0000-4000-8000-000000000105',
		'seller_user_id', '74000000-0000-4000-8000-000000000002',
		'trace_id', 'migration-074-agent-crash-window'
	),
	'runtime', 'terminal', clock_timestamp(), clock_timestamp(),
	clock_timestamp(), '74000000-0000-4000-8000-000000000125'
);

INSERT INTO run_events (id, run_id, sequence, event_type, payload)
VALUES
	('74000000-0000-4000-8000-000000000124', '74000000-0000-4000-8000-000000000114', 1, 'run.failed', '{"status":"timeout","terminal":true}'::jsonb),
	('74000000-0000-4000-8000-000000000125', '74000000-0000-4000-8000-000000000115', 1, 'run.failed', '{"status":"timeout","terminal":true}'::jsonb);

INSERT INTO run_accounting_ledger (
	run_id, terminal_event_id, agent_id, success_delta, revenue_delta_cents
) VALUES
	('74000000-0000-4000-8000-000000000114', '74000000-0000-4000-8000-000000000124', '74000000-0000-4000-8000-000000000010', 0, 0),
	('74000000-0000-4000-8000-000000000115', '74000000-0000-4000-8000-000000000125', '74000000-0000-4000-8000-000000000010', 0, 0);
COMMIT;
ALTER TABLE runs ENABLE TRIGGER runs_v2_contract_identity;
SQL
apply_074
psql_stdin --quiet <<'SQL'
DO $$
BEGIN
	IF (SELECT count(*) FROM external_executions) <> 5
     OR EXISTS (
       SELECT 1 FROM external_executions
       WHERE caller_service_id <> 'openlinker-cloud'
          OR request_fingerprint_version <> 1
     ) THEN
    RAISE EXCEPTION '074 up did not preserve the exact legacy rows';
  END IF;
  IF EXISTS (
      SELECT 1 FROM external_executions
      WHERE legacy_rollback_target_owner_id IS DISTINCT FROM
            '74000000-0000-4000-8000-000000000002'::uuid
  ) THEN
    RAISE EXCEPTION '074 did not preserve exact legacy target-owner rollback evidence';
  END IF;
  IF NOT EXISTS (
      SELECT 1 FROM external_executions
      WHERE external_request_id = '74000000-0000-4000-8000-000000000100'
        AND execution_id = '74000000-0000-4000-8000-000000000110'
        AND start_state = 'authorized'
        AND start_token IS NULL
        AND start_lease_until IS NULL
        AND authorized_target_owner_id IS NULL
        AND rejection_code IS NULL
  ) THEN
    RAISE EXCEPTION '074 did not authorize the preserved attachment whose target was deleted';
  END IF;
  IF NOT EXISTS (
      SELECT 1 FROM external_executions
      WHERE external_request_id = '74000000-0000-4000-8000-000000000102'
        AND execution_kind = 'workflow_run'
        AND execution_id = external_request_id
        AND start_state = 'authorized'
        AND start_token IS NULL
        AND start_lease_until IS NULL
        AND authorized_target_owner_id IS NULL
        AND rejection_code IS NULL
  ) THEN
    RAISE EXCEPTION '074 did not authorize the reconciled workflow crash window';
  END IF;
	IF NOT EXISTS (
		SELECT 1 FROM external_executions
		WHERE external_request_id = '74000000-0000-4000-8000-000000000103'
		  AND execution_kind IS NULL AND execution_id IS NULL
		  AND start_state = 'pending'
		  AND start_token IS NULL
		  AND start_lease_until IS NULL
		  AND authorized_target_owner_id IS NULL
		  AND rejection_code IS NULL
	) THEN
		RAISE EXCEPTION '074 did not preserve an unattached reservation as pending';
	END IF;
	IF NOT EXISTS (
		SELECT 1 FROM external_executions
		WHERE external_request_id = '74000000-0000-4000-8000-000000000104'
		  AND execution_kind IS NULL AND execution_id IS NULL
		  AND start_state = 'pending'
		  AND start_token IS NULL
		  AND start_lease_until IS NULL
		  AND authorized_target_owner_id IS NULL
		  AND rejection_code IS NULL
		  AND downstream_replay_identity->>'idempotency_key' =
		      'hosted-service-order/74000000-0000-4000-8000-000000000104'
		  AND downstream_replay_identity->'metadata'->>'seller_user_id' =
		      '74000000-0000-4000-8000-000000000002'
	) THEN
		RAISE EXCEPTION '074 did not preserve collision-safe legacy replay identity';
	END IF;
	IF NOT EXISTS (
		SELECT 1 FROM external_executions
		WHERE external_request_id = '74000000-0000-4000-8000-000000000105'
		  AND execution_kind IS NULL AND execution_id IS NULL
		  AND start_state = 'pending'
		  AND start_token IS NULL
		  AND start_lease_until IS NULL
		  AND authorized_target_owner_id IS NULL
		  AND rejection_code IS NULL
		  AND downstream_replay_identity->>'source' = 'api'
		  AND downstream_replay_identity->>'creation_protocol' = 'hosted'
		  AND downstream_replay_identity->>'creation_method' = 'service-order.execute'
	) THEN
		RAISE EXCEPTION '074 guessed an Agent attachment instead of deferring exact Runtime lookup';
	END IF;
  IF COALESCE((SELECT column_default FROM information_schema.columns
      WHERE table_schema = 'public'
        AND table_name = 'external_executions'
        AND column_name = 'request_fingerprint_version'), '') NOT LIKE '%2%' THEN
    RAISE EXCEPTION '074 did not install request fingerprint version 2 as the new-row default';
  END IF;
  IF COALESCE((SELECT column_default FROM information_schema.columns
      WHERE table_schema = 'public'
        AND table_name = 'external_executions'
        AND column_name = 'start_state'), '') NOT LIKE '%pending%' THEN
    RAISE EXCEPTION '074 did not install pending as the new-row start-state default';
  END IF;
END
$$;
SQL

echo "[074] accept a normal v2 reservation without legacy rollback evidence"
psql_stdin --quiet <<'SQL'
INSERT INTO external_executions (
  caller_service_id, external_request_id, actor_user_id, target_type,
  target_id, input_fingerprint, trace_id, expected_contract_hash,
  input_schema_fingerprint
) VALUES (
  'openlinker-cloud', '74000000-0000-4000-8000-000000000134',
  '74000000-0000-4000-8000-000000000001', 'agent',
  '74000000-0000-4000-8000-000000000010', decode(repeat('84', 32), 'hex'),
  'migration-074-normal-v2-reservation',
  'hct:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
  decode(repeat('85', 32), 'hex')
);

DO $$
BEGIN
  IF NOT EXISTS (
      SELECT 1 FROM external_executions
      WHERE external_request_id = '74000000-0000-4000-8000-000000000134'
        AND request_fingerprint_version = 2
        AND start_state = 'pending'
        AND legacy_rollback_target_owner_id IS NULL
  ) THEN
    RAISE EXCEPTION '074 blocked or polluted a normal v2 reservation';
  END IF;
END
$$;

DELETE FROM external_executions
WHERE external_request_id = '74000000-0000-4000-8000-000000000134';
SQL

echo "[074] reject malformed downstream replay envelopes"
expect_sql_failure "external_executions_downstream_replay_identity_valid" "
  INSERT INTO external_executions (
    caller_service_id, external_request_id, request_fingerprint_version,
    actor_user_id, target_type, target_id, input_fingerprint, trace_id,
    downstream_replay_identity
  ) VALUES (
    'openlinker-cloud', '74000000-0000-4000-8000-000000000107', 1,
    '74000000-0000-4000-8000-000000000001', 'agent',
    '74000000-0000-4000-8000-000000000010', decode(repeat('77', 32), 'hex'),
    'migration-074-null-envelope-field',
    jsonb_build_object(
      'version', 1, 'kind', NULL, 'source', 'api',
      'idempotency_key', 'hosted-service-order/74000000-0000-4000-8000-000000000107',
      'creation_protocol', 'hosted', 'creation_method', 'service-order.execute',
      'metadata', '{}'::jsonb
    )
  )"
expect_sql_failure "external_executions_downstream_replay_identity_valid" "
  INSERT INTO external_executions (
    caller_service_id, external_request_id, request_fingerprint_version,
    actor_user_id, target_type, target_id, input_fingerprint, trace_id,
    downstream_replay_identity
  ) VALUES (
    'openlinker-cloud', '74000000-0000-4000-8000-000000000108', 1,
    '74000000-0000-4000-8000-000000000001', 'agent',
    '74000000-0000-4000-8000-000000000010', decode(repeat('78', 32), 'hex'),
    'migration-074-numeric-envelope-field',
    jsonb_build_object(
      'version', 1, 'kind', 'run', 'source', 'api',
      'idempotency_key', 740108,
      'creation_protocol', 'hosted', 'creation_method', 'service-order.execute',
      'metadata', '{}'::jsonb
    )
  )"

echo "[074] reject malformed durable starter states"
expect_sql_failure "external_executions_start_state_valid" "
  INSERT INTO external_executions (
    caller_service_id, external_request_id, actor_user_id, target_type,
    target_id, input_fingerprint, trace_id, start_state, start_token
  ) VALUES (
    'openlinker-cloud', '74000000-0000-4000-8000-000000000130',
    '74000000-0000-4000-8000-000000000001', 'agent',
    '74000000-0000-4000-8000-000000000010', decode(repeat('80', 32), 'hex'),
    'migration-074-pending-with-token', 'pending',
    '74000000-0000-4000-8000-000000000230'
  )"
expect_sql_failure "external_executions_start_state_valid" "
  INSERT INTO external_executions (
    caller_service_id, external_request_id, actor_user_id, target_type,
    target_id, input_fingerprint, trace_id, start_state, start_token
  ) VALUES (
    'openlinker-cloud', '74000000-0000-4000-8000-000000000131',
    '74000000-0000-4000-8000-000000000001', 'agent',
    '74000000-0000-4000-8000-000000000010', decode(repeat('81', 32), 'hex'),
    'migration-074-evaluating-without-lease', 'evaluating',
    '74000000-0000-4000-8000-000000000231'
  )"
expect_sql_failure "external_executions_start_state_valid" "
  INSERT INTO external_executions (
    caller_service_id, external_request_id, actor_user_id, target_type,
    target_id, input_fingerprint, trace_id, start_state
  ) VALUES (
    'openlinker-cloud', '74000000-0000-4000-8000-000000000132',
    '74000000-0000-4000-8000-000000000001', 'agent',
    '74000000-0000-4000-8000-000000000010', decode(repeat('82', 32), 'hex'),
    'migration-074-authorized-without-owner-or-attachment', 'authorized'
  )"
expect_sql_failure "external_executions_start_state_valid" "
  INSERT INTO external_executions (
    caller_service_id, external_request_id, actor_user_id, target_type,
    target_id, input_fingerprint, trace_id, start_state, rejection_code
  ) VALUES (
    'openlinker-cloud', '74000000-0000-4000-8000-000000000133',
    '74000000-0000-4000-8000-000000000001', 'agent',
    '74000000-0000-4000-8000-000000000010', decode(repeat('83', 32), 'hex'),
    'migration-074-rejected-with-unknown-code', 'rejected', 'UNKNOWN_REJECTION'
  )"

echo "[074] accept every durable starter rejection code"
psql_stdin --quiet <<'SQL'
DO $$
DECLARE
  durable_rejection_code text;
BEGIN
  FOREACH durable_rejection_code IN ARRAY ARRAY[
    'TARGET_UNAVAILABLE',
    'TARGET_CONTRACT_CHANGED',
    'DOWNSTREAM_IDENTITY_CONFLICT'
  ]
  LOOP
    UPDATE external_executions
    SET start_state = 'rejected',
        start_token = NULL,
        start_lease_until = NULL,
        authorized_target_owner_id = NULL,
        rejection_code = durable_rejection_code
    WHERE external_request_id = '74000000-0000-4000-8000-000000000103';

    IF NOT EXISTS (
      SELECT 1
      FROM external_executions
      WHERE external_request_id = '74000000-0000-4000-8000-000000000103'
        AND start_state = 'rejected'
        AND start_token IS NULL
        AND start_lease_until IS NULL
        AND authorized_target_owner_id IS NULL
        AND rejection_code = durable_rejection_code
    ) THEN
      RAISE EXCEPTION '074 rejected durable starter code %', durable_rejection_code;
    END IF;
  END LOOP;

  UPDATE external_executions
  SET start_state = 'pending', rejection_code = NULL
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103';
END
$$;
SQL
assert_failed_down_was_atomic 5

echo "[074] rollback fails closed outside hard maintenance"
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'normal' WHERE singleton_id = 1" >/dev/null
expect_revert_failure "migration 074 rollback requires hard maintenance"
assert_failed_down_was_atomic 5
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1" >/dev/null

echo "[074] rollback fails closed while a Core member is registered"
psql_command --quiet -c "
  INSERT INTO runtime_cluster_members (
    instance_id, release_version, release_commit, schema_version,
    schema_checksum, runtime_contract_id, runtime_contract_digest
  )
  SELECT
    '74000000-0000-4000-8000-000000000097', 'test', 'test', schema_version,
    repeat('b', 64), runtime_contract_id, runtime_contract_digest
  FROM runtime_schema_contracts
  WHERE is_current" >/dev/null
expect_revert_failure "migration 074 rollback requires zero registered Core cluster members"
assert_failed_down_was_atomic 5
psql_command --quiet -c "DELETE FROM runtime_cluster_members" >/dev/null

echo "[074] rollback fails closed while a Run is still running"
psql_command --quiet -c "
  INSERT INTO runs (
    id, user_id, agent_id, input, status, cost_cents,
    platform_fee_cents, creator_revenue_cents, source,
    idempotency_key_hash, idempotency_fingerprint,
    connection_mode_snapshot, dispatch_deadline_at, run_deadline_at
  ) VALUES (
    '74000000-0000-4000-8000-000000000041',
    '74000000-0000-4000-8000-000000000001',
    '74000000-0000-4000-8000-000000000010',
    '{\"case\":\"migration-074-rollback-running\"}', 'running', 0, 0, 0, 'api',
    decode(repeat('73', 32), 'hex'), decode(repeat('74', 32), 'hex'),
    'runtime', clock_timestamp() + interval '2 minutes',
    clock_timestamp() + interval '10 minutes'
  )" >/dev/null
expect_revert_failure "migration 074 rollback requires zero running Runs"
assert_failed_down_was_atomic 5
psql_stdin --quiet <<'SQL'
ALTER TABLE runs DISABLE TRIGGER runs_v2_contract_identity;
DELETE FROM runs WHERE id = '74000000-0000-4000-8000-000000000041';
ALTER TABLE runs ENABLE TRIGGER runs_v2_contract_identity;
SQL

echo "[074] rollback refuses caller identities the legacy schema cannot represent"
psql_stdin --quiet <<'SQL'
INSERT INTO external_executions (
  caller_service_id, external_request_id, request_fingerprint_version,
  actor_user_id, target_type, target_id, input_fingerprint, trace_id,
  start_state, execution_kind, execution_id
) VALUES (
  'other-service', '74000000-0000-4000-8000-000000000109', 1,
  '74000000-0000-4000-8000-000000000001', 'agent',
  '74000000-0000-4000-8000-000000000010', decode(repeat('79', 32), 'hex'),
  'migration-074-other-caller', 'authorized', 'run',
  '74000000-0000-4000-8000-000000000119'
);
SQL
expect_revert_failure "migration 074 rollback cannot represent multiple caller services"
assert_failed_down_was_atomic 6
psql_command --quiet -c "DELETE FROM external_executions WHERE external_request_id = '74000000-0000-4000-8000-000000000109'" >/dev/null

echo "[074] preserve historical owner evidence across target reassignment"
psql_command --quiet -c "
  UPDATE workflows
  SET user_id = '74000000-0000-4000-8000-000000000003'
  WHERE id = '74000000-0000-4000-8000-000000000020'" >/dev/null
psql_command --quiet -c "
  SELECT 1 / CASE
    WHEN legacy_rollback_target_owner_id = '74000000-0000-4000-8000-000000000002'
     AND (SELECT user_id FROM workflows WHERE id = '74000000-0000-4000-8000-000000000020') =
         '74000000-0000-4000-8000-000000000003'
    THEN 1 ELSE 0 END
  FROM external_executions
  WHERE external_request_id = '74000000-0000-4000-8000-000000000102'" >/dev/null

echo "[074] rollback refuses an active start evaluation lease"
psql_command --quiet -c "
  UPDATE external_executions
  SET start_state = 'evaluating',
      start_token = '74000000-0000-4000-8000-000000000240',
      start_lease_until = clock_timestamp() + interval '5 minutes',
      authorized_target_owner_id = NULL,
      rejection_code = NULL
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null
expect_revert_failure "migration 074 rollback cannot represent in-flight or durable external execution start decisions"
assert_failed_down_was_atomic 5
psql_command --quiet -c "
  SELECT 1 / CASE WHEN start_state = 'evaluating'
                        AND start_token = '74000000-0000-4000-8000-000000000240'
                        AND start_lease_until IS NOT NULL
                        AND authorized_target_owner_id IS NULL
                        AND rejection_code IS NULL
                   THEN 1 ELSE 0 END
  FROM external_executions
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null
psql_command --quiet -c "
  UPDATE external_executions
  SET start_state = 'pending', start_token = NULL, start_lease_until = NULL,
      authorized_target_owner_id = NULL, rejection_code = NULL
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null

echo "[074] rollback refuses a durable downstream-identity rejection"
psql_command --quiet -c "
  UPDATE external_executions
  SET start_state = 'rejected', start_token = NULL, start_lease_until = NULL,
      authorized_target_owner_id = NULL, rejection_code = 'DOWNSTREAM_IDENTITY_CONFLICT'
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null
expect_revert_failure "migration 074 rollback cannot represent in-flight or durable external execution start decisions"
assert_failed_down_was_atomic 5
psql_command --quiet -c "
  SELECT 1 / CASE WHEN start_state = 'rejected'
                        AND start_token IS NULL
                        AND start_lease_until IS NULL
                        AND authorized_target_owner_id IS NULL
                        AND rejection_code = 'DOWNSTREAM_IDENTITY_CONFLICT'
                   THEN 1 ELSE 0 END
  FROM external_executions
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null
psql_command --quiet -c "
  UPDATE external_executions
  SET start_state = 'pending', rejection_code = NULL
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null

echo "[074] rollback refuses an authorized but unattached start"
psql_command --quiet -c "
  UPDATE external_executions
  SET start_state = 'authorized', start_token = NULL, start_lease_until = NULL,
      authorized_target_owner_id = '74000000-0000-4000-8000-000000000002',
      rejection_code = NULL
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null
expect_revert_failure "migration 074 rollback cannot represent in-flight or durable external execution start decisions"
assert_failed_down_was_atomic 5
psql_command --quiet -c "
  SELECT 1 / CASE WHEN start_state = 'authorized'
                        AND execution_id IS NULL
                        AND authorized_target_owner_id = '74000000-0000-4000-8000-000000000002'
                        AND start_token IS NULL
                        AND start_lease_until IS NULL
                        AND rejection_code IS NULL
                   THEN 1 ELSE 0 END
  FROM external_executions
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null
psql_command --quiet -c "
  UPDATE external_executions
  SET start_state = 'pending', authorized_target_owner_id = NULL
  WHERE external_request_id = '74000000-0000-4000-8000-000000000103'" >/dev/null

echo "[074] rollback refuses records written with the new fingerprint algorithm"
psql_stdin --quiet <<'SQL'
INSERT INTO external_executions (
  caller_service_id, external_request_id, actor_user_id, target_type,
  target_id, input_fingerprint, trace_id, start_state,
  execution_kind, execution_id
) VALUES (
	'openlinker-cloud', '74000000-0000-4000-8000-000000000106',
  '74000000-0000-4000-8000-000000000001', 'agent',
  '74000000-0000-4000-8000-000000000010',
  decode(repeat('55', 32), 'hex'), 'migration-074-v2-row',
  'authorized', 'run', '74000000-0000-4000-8000-000000000113'
);
SQL
psql_command --quiet -c "SELECT 1 / (request_fingerprint_version - 1) FROM external_executions WHERE external_request_id = '74000000-0000-4000-8000-000000000106'" >/dev/null
expect_revert_failure "migration 074 rollback cannot safely downgrade request fingerprint version 2"
assert_failed_down_was_atomic 6
psql_command --quiet -c "DELETE FROM external_executions WHERE external_request_id = '74000000-0000-4000-8000-000000000106'" >/dev/null
revert_074
psql_stdin --quiet <<'SQL'
DO $$
BEGIN
	IF (SELECT count(*) FROM hosted_service_executions) <> 5
     OR EXISTS (
       SELECT 1 FROM hosted_service_executions
       WHERE seller_user_id <> '74000000-0000-4000-8000-000000000002'
     ) THEN
    RAISE EXCEPTION '074 up/down did not restore exact legacy owners';
  END IF;
  IF (SELECT user_id FROM workflows
      WHERE id = '74000000-0000-4000-8000-000000000020') <>
      '74000000-0000-4000-8000-000000000003' THEN
    RAISE EXCEPTION '074 rollback test did not retain the reassigned current Workflow owner';
  END IF;
  IF EXISTS (
      SELECT 1 FROM agents
      WHERE id = '74000000-0000-4000-8000-000000000099'
  ) THEN
    RAISE EXCEPTION '074 rollback unexpectedly required the deleted historical target';
  END IF;
END
$$;
SQL

echo "migration 074 test passed"
