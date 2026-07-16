#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="$ROOT_DIR/migrations"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16}"
CONTAINER_NAME="openlinker-migration-075-${PPID}-$$"
DATABASE_NAME="openlinker"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "migration 075 test failed: $*" >&2
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

echo "[075] apply predecessor migrations through 072"
for migration_path in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.up.sql; do
  migration_name="$(basename "$migration_path")"
  version="${migration_name%%_*}"
  if ((10#$version <= 72)); then
    run_migration "$migration_path" >/dev/null
  fi
done
psql_command --quiet -c "UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1" >/dev/null

echo "[075] apply 073, 074, 075 and verify exact N/N-1"
run_migration "$MIGRATIONS_DIR/073_runtime_transport_observability.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/074_external_execution_boundary.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility_verify.sql" >/dev/null

echo "[075] roll back without losing wire-contract history"
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility.down.sql" >/dev/null
psql_stdin --quiet <<'SQL'
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'runtime_wire_contracts'
      AND column_name = 'support_tier'
  ) THEN
    RAISE EXCEPTION '075 rollback left support_tier installed';
  END IF;
  IF (
    SELECT count(*) FROM runtime_schema_contracts
    WHERE schema_version = 73
      AND migration_name = '073_runtime_transport_observability'
      AND is_current
  ) <> 1 OR EXISTS (
    SELECT 1 FROM runtime_schema_contracts WHERE schema_version = 75
  ) THEN
    RAISE EXCEPTION '075 rollback did not restore schema contract 73 exactly';
  END IF;
  IF (
    SELECT count(*) FROM runtime_wire_contracts
    WHERE runtime_contract_id = 'openlinker.runtime.v2'
  ) < 4 THEN
    RAISE EXCEPTION '075 rollback lost Runtime wire-contract history';
  END IF;
END
$$;
SQL

echo "[075] re-apply and verify deterministic compatibility state"
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility_verify.sql" >/dev/null

echo "[075] roll back the complete 075/074/073 chain"
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility.down.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/074_external_execution_boundary.down.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/073_runtime_transport_observability.down.sql" >/dev/null
psql_stdin --quiet <<'SQL'
DO $$
BEGIN
  IF to_regclass('runtime_wire_contracts') IS NOT NULL THEN
    RAISE EXCEPTION '073 rollback left runtime_wire_contracts installed';
  END IF;
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'runtime_session_attachments'
      AND column_name IN ('transport', 'transport_reason', 'transport_changed_at')
  ) THEN
    RAISE EXCEPTION '073 rollback left transport-observability columns installed';
  END IF;
  IF (
    SELECT count(*) FROM runtime_schema_contracts
    WHERE schema_version = 71
      AND migration_name = '071_runtime_attachment_generation'
      AND is_current
  ) <> 1 OR EXISTS (
    SELECT 1 FROM runtime_schema_contracts WHERE schema_version IN (73, 75)
  ) THEN
    RAISE EXCEPTION 'full rollback did not restore schema contract 71 exactly';
  END IF;
  IF to_regclass('hosted_service_executions') IS NULL
     OR to_regclass('external_executions') IS NOT NULL THEN
    RAISE EXCEPTION 'full rollback did not restore the legacy execution table';
  END IF;
END
$$;
SQL

echo "[075] re-apply the complete chain and verify again"
run_migration "$MIGRATIONS_DIR/073_runtime_transport_observability.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/074_external_execution_boundary.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/075_runtime_wire_compatibility_verify.sql" >/dev/null

echo "migration 075 test passed"
