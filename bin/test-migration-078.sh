#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="$ROOT_DIR/migrations"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16}"
CONTAINER_NAME="openlinker-migration-078-${PPID}-$$"
DATABASE_NAME="openlinker"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "migration 078 test failed: $*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "docker daemon is not available"

docker run --detach --name "$CONTAINER_NAME" \
  --env POSTGRES_HOST_AUTH_METHOD=trust \
  --env POSTGRES_DB="$DATABASE_NAME" \
  --publish 127.0.0.1::5432 \
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

host_endpoint="$(docker port "$CONTAINER_NAME" 5432/tcp | head -n 1)"
host_port="${host_endpoint##*:}"
[[ "$host_port" =~ ^[0-9]+$ ]] || fail "could not determine mapped PostgreSQL port"
database_url="postgres://postgres@127.0.0.1:${host_port}/${DATABASE_NAME}?sslmode=disable"

psql_stdin() {
  docker exec -i --env PGOPTIONS="-c client_min_messages=warning" "$CONTAINER_NAME" \
    psql -X -v ON_ERROR_STOP=1 -U postgres -d "$DATABASE_NAME" "$@"
}

run_migration() {
  psql_stdin --quiet <"$1"
}

echo "[078] exercise golang-migrate Steps(-1), dirty recovery, and legal rollback"
(
  cd "$ROOT_DIR"
  OAUTH_MIGRATION_TEST_DATABASE_URL="$database_url" \
    go test ./pkg/auth -run '^TestOAuthLoginCodeMigration078StepsDownFailClosed$' -count=1
)

echo "[078] install the focused Auth predecessor schema and migration 078"
psql_stdin --quiet <<'SQL'
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
SQL
run_migration "$MIGRATIONS_DIR/001_init.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/050_oauth_login_codes.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/078_oauth_login_code_subject_only.up.sql" >/dev/null
run_migration "$MIGRATIONS_DIR/078_oauth_login_code_subject_only_verify.sql" >/dev/null

echo "[078] verify Auth regressions, legacy/subject-only writers, compatibility reader, rollback, and concurrency"
(
  cd "$ROOT_DIR"
  TEST_DATABASE_URL="$database_url" \
    go test -race ./pkg/auth -count=5
)

echo "migration 078 test: ok"
