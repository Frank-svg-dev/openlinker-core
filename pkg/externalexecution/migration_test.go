package externalexecution

import (
	"os"
	"strings"
	"testing"
)

func TestLegacyHostedExecutionMigrationKeepsUpgradeSource(t *testing.T) {
	up, err := os.ReadFile("../../migrations/068_hosted_service_execution_bridge.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	for _, fragment := range []string{
		"CREATE TABLE hosted_service_executions",
		"external_order_id UUID PRIMARY KEY",
		"input_fingerprint BYTEA NOT NULL CHECK (octet_length(input_fingerprint) = 32)",
		"CREATE UNIQUE INDEX idx_hosted_service_executions_execution",
	} {
		if !strings.Contains(string(up), fragment) {
			t.Fatalf("legacy migration missing %q", fragment)
		}
	}
}

func TestExternalExecutionMigrationRemovesHostedOrderSemantics(t *testing.T) {
	up, err := os.ReadFile("../../migrations/074_external_execution_boundary.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	down, err := os.ReadFile("../../migrations/074_external_execution_boundary.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	for _, fragment := range []string{
		"pg_advisory_xact_lock",
		"LOCK TABLE hosted_service_executions IN ACCESS EXCLUSIVE MODE",
		"mode = 'hard_maintenance'",
		"zero registered Core cluster members",
		"zero running Runs",
		"LOCK TABLE workflow_runs IN SHARE MODE",
		"ADD COLUMN downstream_replay_identity JSONB",
		"'version', 1",
		"'kind', 'run'",
		"'source', 'api'",
		"'idempotency_key', 'hosted-service-order/' || e.external_order_id::text",
		"'creation_protocol', 'hosted'",
		"'creation_method', 'service-order.execute'",
		"external_executions_downstream_replay_identity_valid",
		"jsonb_typeof(downstream_replay_identity->'idempotency_key') = 'string'",
		"jsonb_typeof(downstream_replay_identity->'creation_protocol') = 'string'",
		") IS TRUE",
		"SET execution_kind = 'workflow_run'",
		"wr.id = e.external_order_id",
		"ALTER TABLE hosted_service_executions RENAME TO external_executions",
		"RENAME COLUMN external_order_id TO external_request_id",
		"RENAME COLUMN buyer_user_id TO actor_user_id",
		"ADD COLUMN caller_service_id TEXT NOT NULL DEFAULT 'openlinker-cloud'",
		"ADD COLUMN request_fingerprint_version SMALLINT NOT NULL DEFAULT 1",
		"ADD COLUMN start_state TEXT NOT NULL DEFAULT 'pending'",
		"ADD COLUMN authorized_target_owner_id UUID",
		"RENAME COLUMN seller_user_id TO legacy_rollback_target_owner_id",
		"ALTER COLUMN legacy_rollback_target_owner_id DROP NOT NULL",
		"external_executions_legacy_rollback_target_owner_id_fkey",
		"external_executions_start_state_valid",
		"start_lease_until",
		"rejection_code IN ('TARGET_UNAVAILABLE', 'TARGET_CONTRACT_CHANGED', 'DOWNSTREAM_IDENTITY_CONFLICT')",
		"ALTER COLUMN request_fingerprint_version SET DEFAULT 2",
		"CHECK (request_fingerprint_version IN (1, 2))",
		"PRIMARY KEY (caller_service_id, external_request_id)",
	} {
		if !strings.Contains(string(up), fragment) {
			t.Fatalf("up migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"CREATE TABLE external_executions",
		"INSERT INTO external_executions",
		"DROP TABLE hosted_service_executions",
		"subject_user_id",
		"target_owner_user_id",
	} {
		if strings.Contains(string(up), forbidden) {
			t.Fatalf("up migration must use an in-place generic cutover; found %q", forbidden)
		}
	}
	for _, fragment := range []string{
		"LOCK TABLE external_executions IN ACCESS EXCLUSIVE MODE",
		"caller_service_id <> 'openlinker-cloud'",
		"rollback cannot represent multiple caller services",
		"rollback cannot safely downgrade request fingerprint version 2",
		"rollback cannot represent in-flight or durable external execution start decisions",
		"RENAME COLUMN legacy_rollback_target_owner_id TO seller_user_id",
		"ALTER COLUMN legacy_rollback_target_owner_id SET NOT NULL",
		"rollback is missing exact legacy target owner evidence",
		"external_executions_legacy_rollback_target_owner_id_fkey",
		"RENAME COLUMN external_request_id TO external_order_id",
		"RENAME COLUMN actor_user_id TO buyer_user_id",
		"ALTER TABLE external_executions RENAME TO hosted_service_executions",
		"DROP COLUMN request_fingerprint_version",
		"DROP COLUMN downstream_replay_identity",
		"DROP COLUMN authorized_target_owner_id",
		"DROP COLUMN start_state",
	} {
		if !strings.Contains(string(down), fragment) {
			t.Fatalf("down migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"CREATE TABLE hosted_service_executions",
		"INSERT INTO hosted_service_executions",
		"DROP TABLE external_executions",
		"SELECT a.creator_id",
		"SELECT w.user_id",
		"rollback cannot rederive every target owner",
	} {
		if strings.Contains(string(down), forbidden) {
			t.Fatalf("down migration must be in-place; found %q", forbidden)
		}
	}
}
