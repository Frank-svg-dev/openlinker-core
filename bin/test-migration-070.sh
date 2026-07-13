#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="$ROOT_DIR/migrations"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16}"
CONTAINER_NAME="openlinker-migration-070-${PPID}-$$"
DATABASE_NAME="openlinker"
CONTRACT_DIGEST="fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "migration 070 test failed: $*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "docker daemon is not available"

for fragment in \
  "UPDATE agents" \
  "UPDATE runs" \
  "UPDATE run_attempts" \
  "SET schema_version = 70" \
  "DROP TRIGGER runs_v2_contract_identity" \
  "DROP TRIGGER run_attempts_identity_immutable"; do
  grep -Fq "$fragment" "$MIGRATIONS_DIR/070_sdk_first_runtime_boundary.up.sql" \
    || fail "set-based cutover fragment is missing: $fragment"
done

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

reset_database() {
  docker exec "$CONTAINER_NAME" \
    psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres --quiet \
      -c "DROP DATABASE IF EXISTS $DATABASE_NAME WITH (FORCE)" \
      -c "CREATE DATABASE $DATABASE_NAME"
}

run_migration() {
  psql_stdin --quiet <"$1"
}

apply_through_069() {
  local migration_path migration_name version
  for migration_path in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.up.sql; do
    migration_name="$(basename "$migration_path")"
    version="${migration_name%%_*}"
    if ((10#$version <= 69)); then
      run_migration "$migration_path" >/dev/null
    fi
  done
}

apply_070() {
  run_migration "$MIGRATIONS_DIR/070_sdk_first_runtime_boundary.up.sql" >/dev/null
}

revert_070() {
  run_migration "$MIGRATIONS_DIR/070_sdk_first_runtime_boundary.down.sql" >/dev/null
}

verify_070() {
  run_migration "$MIGRATIONS_DIR/070_sdk_first_runtime_boundary_verify.sql" >/dev/null
}

expect_apply_failure() {
  local expected="$1"
  local output status
  set +e
  output="$(apply_070 2>&1)"
  status=$?
  set -e
  if ((status == 0)); then
    fail "070 unexpectedly succeeded; expected: $expected"
  fi
  [[ "$output" == *"$expected"* ]] \
    || fail "070 failed for the wrong reason; expected '$expected', got: $output"
}

insert_agent() {
  psql_stdin --quiet <<'SQL'
INSERT INTO users (id, email, password_hash, display_name, is_creator)
VALUES (
  '70000000-0000-4000-8000-000000000001',
  'migration-070@example.test', 'test-hash', 'Migration 070', TRUE
);

INSERT INTO agents (
  id, creator_id, slug, name, description, endpoint_url,
  price_per_call_cents, connection_mode
) VALUES (
  '70000000-0000-4000-8000-000000000002',
  '70000000-0000-4000-8000-000000000001',
  'migration-070-agent', 'Migration 070 Agent', 'Runtime boundary fixture',
  'openlinker-agent-node://migration-070-agent', 0, 'agent_node'
);
SQL
}

assert_boundary() {
  local want_schema="$1"
  local want_migration="$2"
  local want_mode="$3"
  local want_endpoint="$4"
  psql_stdin --quiet <<SQL
DO \$\$
BEGIN
  IF (
    SELECT COUNT(*) FROM runtime_schema_contracts
    WHERE schema_version = $want_schema
      AND migration_name = '$want_migration'
      AND runtime_contract_digest = '$CONTRACT_DIGEST'
      AND is_current
  ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
    RAISE EXCEPTION 'unexpected current Runtime schema identity';
  END IF;
  IF (
    SELECT connection_mode FROM agents
    WHERE id = '70000000-0000-4000-8000-000000000002'
  ) <> '$want_mode' OR (
    SELECT endpoint_url FROM agents
    WHERE id = '70000000-0000-4000-8000-000000000002'
  ) <> '$want_endpoint' THEN
    RAISE EXCEPTION 'Agent Runtime boundary was not converted';
  END IF;
END
\$\$;
SQL
}

echo "[070] apply predecessor migrations"
apply_through_069

echo "[070] fail closed outside hard maintenance"
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'normal' WHERE singleton_id = 1" >/dev/null
expect_apply_failure "migration 070 requires hard maintenance"
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1" >/dev/null

echo "[070] fail closed while a Core member is registered"
psql_command --quiet -c "
  INSERT INTO runtime_cluster_members (
    instance_id, release_version, release_commit, schema_version,
    schema_checksum, runtime_contract_id, runtime_contract_digest
  ) VALUES (
    '70000000-0000-4000-8000-000000000098', 'test', 'test', 69,
    repeat('a', 64), 'openlinker.runtime.v2', '$CONTRACT_DIGEST'
  )" >/dev/null
expect_apply_failure "migration 070 requires zero registered Core cluster members"
psql_command --quiet -c "DELETE FROM runtime_cluster_members" >/dev/null

echo "[070] fail closed when the Agent table lock cannot be acquired"
docker exec \
  --env PGOPTIONS="-c client_min_messages=warning" \
  --env PGAPPNAME="migration-070-lock-holder" \
  "$CONTAINER_NAME" \
  psql -X -v ON_ERROR_STOP=1 -U postgres -d "$DATABASE_NAME" \
  -c "BEGIN; LOCK TABLE agents IN ACCESS SHARE MODE; SELECT pg_sleep(30); COMMIT;" \
  >/dev/null 2>&1 &
lock_holder_pid=$!
for _ in $(seq 1 100); do
  if [[ "$(psql_command --tuples-only --no-align -c "
    SELECT COUNT(*) FROM pg_stat_activity
    WHERE datname = current_database()
      AND application_name = 'migration-070-lock-holder'
      AND state = 'active'
  ")" == "1" ]]; then
    break
  fi
  sleep 0.1
done
expect_apply_failure "canceling statement due to lock timeout"
psql_command --quiet -c "
  SELECT pg_terminate_backend(pid) FROM pg_stat_activity
  WHERE datname = current_database()
    AND application_name = 'migration-070-lock-holder'
" >/dev/null
wait "$lock_holder_pid" 2>/dev/null || true

insert_agent

echo "[070] fail closed while a Run is still running"
psql_command --quiet -c "
  INSERT INTO runs (
    id, user_id, agent_id, input, status, cost_cents,
    platform_fee_cents, creator_revenue_cents, source,
    idempotency_key_hash, idempotency_fingerprint,
    connection_mode_snapshot, dispatch_deadline_at, run_deadline_at
  ) VALUES (
    '70000000-0000-4000-8000-000000000040',
    '70000000-0000-4000-8000-000000000001',
    '70000000-0000-4000-8000-000000000002',
    '{\"case\":\"migration-070-running\"}', 'running', 0, 0, 0, 'api',
    decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'),
    'agent_node', clock_timestamp() + interval '2 minutes',
    clock_timestamp() + interval '10 minutes'
  )" >/dev/null
expect_apply_failure "migration 070 requires zero running Runs"

reset_database
apply_through_069
insert_agent

echo "[070] fail closed on a conflicting schema 70 identity"
psql_command --quiet -c "
  INSERT INTO runtime_schema_contracts (
    schema_version, migration_name, runtime_contract_id,
    runtime_contract_digest, is_current
  ) VALUES (
    70, '070_conflicting_fixture', 'openlinker.runtime.v2',
    repeat('f', 64), FALSE
  )" >/dev/null
expect_apply_failure "migration 070 found a conflicting schema contract 70"

reset_database
apply_through_069
insert_agent

echo "[070] fail closed on an unknown connection value"
psql_stdin --quiet <<'SQL'
ALTER TABLE agents DROP CONSTRAINT agents_connection_mode_valid;
ALTER TABLE agents DROP CONSTRAINT agents_runtime_queue_endpoint;
ALTER TABLE agents DROP CONSTRAINT agents_endpoint_https;
UPDATE agents SET connection_mode = 'unknown_runtime_fixture';
SQL
expect_apply_failure "migration 070 found an unknown connection or executor value"

reset_database
apply_through_069
insert_agent

echo "[070] apply SDK-first Runtime boundary"
apply_070
verify_070
assert_boundary 70 "070_sdk_first_runtime_boundary" "runtime" "openlinker-runtime://migration-070-agent"

echo "[070] rollback restores schema 69 and legacy data labels"
revert_070
assert_boundary 69 "069_runtime_entry_discovery" "agent_node" "openlinker-agent-node://migration-070-agent"

echo "[070] re-up is deterministic"
apply_070
verify_070
assert_boundary 70 "070_sdk_first_runtime_boundary" "runtime" "openlinker-runtime://migration-070-agent"

echo "migration 070 test passed"
