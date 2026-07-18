#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="$ROOT_DIR/migrations"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16}"
CONTAINER_NAME="openlinker-migration-081-${PPID}-$$"
DATABASE_NAME="openlinker"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "migration 081 test failed: $*" >&2
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

run_migration() {
  psql_stdin --quiet <"$1"
}

apply_through_079() {
  local migration_path migration_name version
  for migration_path in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.up.sql; do
    migration_name="$(basename "$migration_path")"
    version="${migration_name%%_*}"
    if ((10#$version <= 79)); then
      run_migration "$migration_path" >/dev/null
    fi
  done
  psql_stdin --quiet <<'SQL'
UPDATE runtime_cluster_control
SET mode = 'hard_maintenance'
WHERE singleton_id = 1;
SQL
}

reset_database() {
  docker exec "$CONTAINER_NAME" dropdb --force -U postgres "$DATABASE_NAME"
  docker exec "$CONTAINER_NAME" createdb -U postgres "$DATABASE_NAME"
}

expect_up_failure() {
  local expected="$1"
  local output status
  set +e
  output="$(run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.up.sql" 2>&1)"
  status=$?
  set -e
  if ((status == 0)); then
    fail "081 reconciliation unexpectedly succeeded; expected: $expected"
  fi
  [[ "$output" == *"$expected"* ]] \
    || fail "081 reconciliation failed for the wrong reason; expected '$expected', got: $output"
}

assert_canonical_080() {
  local current
  current="$(psql_stdin --tuples-only --no-align --field-separator='|' --command \
    "SELECT schema_version, migration_name, is_current
     FROM runtime_schema_contracts
     WHERE is_current")"
  [[ "$current" == "80|080_runtime_attempt_transport_evidence|t" ]] \
    || fail "current Runtime schema contract is not canonical 080: $current"
}

echo "[081] canonical 080 path is a validated no-op"
apply_through_079
run_migration "$MIGRATIONS_DIR/080_runtime_attempt_transport_evidence.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/080_runtime_attempt_transport_evidence_verify.sql" >/dev/null
psql_stdin --quiet <<'SQL'
UPDATE runtime_cluster_control
SET mode = 'normal'
WHERE singleton_id = 1;
SQL
expect_up_failure "requires hard maintenance"
psql_stdin --quiet <<'SQL'
UPDATE runtime_cluster_control
SET mode = 'hard_maintenance'
WHERE singleton_id = 1;
SQL
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation_verify.sql" >/dev/null
assert_canonical_080
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.down.sql" >/dev/null
assert_canonical_080
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.up.sql" >/dev/null

echo "[081] reconcile the deployed intermediate 079 transport identity"
reset_database
apply_through_079
run_migration "$MIGRATIONS_DIR/080_runtime_attempt_transport_evidence.up.sql" >/dev/null
psql_stdin --quiet <<'SQL'
UPDATE runtime_schema_contracts
SET schema_version = 79,
    migration_name = '079_runtime_attempt_transport_evidence'
WHERE schema_version = 80
  AND migration_name = '080_runtime_attempt_transport_evidence'
  AND is_current;
SQL
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation_verify.sql" >/dev/null
assert_canonical_080
if [[ "$(psql_stdin --tuples-only --no-align --command \
  "SELECT COUNT(*) FROM runtime_schema_contracts
   WHERE schema_version = 79
     AND migration_name = '079_runtime_attempt_transport_evidence'
     AND NOT is_current")" != "1" ]]; then
  fail "reconciled intermediate 079 history is missing"
fi
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.down.sql" >/dev/null
assert_canonical_080
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/081_runtime_attempt_transport_identity_reconciliation_verify.sql" >/dev/null

echo "[081] fail closed when the physical transport evidence schema is incomplete"
psql_stdin --quiet <<'SQL'
DROP TRIGGER run_attempts_runtime_attachment_evidence ON run_attempts;
SQL
expect_up_failure "transport evidence trigger is missing"

echo "migration 081 test passed"
