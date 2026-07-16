#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="$ROOT_DIR/migrations"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16}"
CONTAINER_NAME="openlinker-migration-076-${PPID}-$$"
DATABASE_NAME="openlinker"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "migration 076 test failed: $*" >&2
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

run_migration() {
  psql_stdin --quiet <"$1"
}

echo "[076] apply predecessor migrations through 072"
for migration_path in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.up.sql; do
  migration_name="$(basename "$migration_path")"
  version="${migration_name%%_*}"
  if ((10#$version <= 72)); then
    run_migration "$migration_path" >/dev/null
  fi
done
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1" >/dev/null

echo "[076] apply 073 through 076 and verify terminal reaper invariant"
run_migration "$MIGRATIONS_DIR/073_runtime_transport_observability.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/074_external_execution_boundary.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/076_runtime_cancellation_terminal_reap.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/076_runtime_cancellation_terminal_reap_verify.sql" >/dev/null

echo "[076] roll back and verify the exact schema-75 invariant"
run_migration "$MIGRATIONS_DIR/076_runtime_cancellation_terminal_reap.down.sql" >/dev/null
psql_stdin --quiet <<'SQL'
DO $$
DECLARE
  definition TEXT;
BEGIN
  IF (
    SELECT count(*) FROM runtime_schema_contracts
    WHERE schema_version = 75
      AND migration_name = '075_runtime_wire_compatibility'
      AND is_current
  ) <> 1 OR EXISTS (
    SELECT 1 FROM runtime_schema_contracts WHERE schema_version = 76
  ) THEN
    RAISE EXCEPTION '076 rollback did not restore schema contract 75 exactly';
  END IF;
  definition := pg_get_functiondef(
    'enforce_run_terminal_artifacts_consistency()'::regprocedure
  );
  IF POSITION(
       'cancellation_state IN (''requested'', ''delivered'', ''stopping'', ''unsupported'', ''failed'')'
       IN definition
     ) = 0
     OR definition LIKE '%cancellation_requested_at TIMESTAMPTZ%' THEN
    RAISE EXCEPTION '076 rollback did not restore the strict negative ACK invariant';
  END IF;
END
$$;
SQL

echo "[076] re-apply and verify deterministic invariant state"
run_migration "$MIGRATIONS_DIR/076_runtime_cancellation_terminal_reap.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/076_runtime_cancellation_terminal_reap_verify.sql" >/dev/null

echo "migration 076 test passed"
