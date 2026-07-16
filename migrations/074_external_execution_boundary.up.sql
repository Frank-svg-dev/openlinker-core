BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.external-execution.migration.074', 0));

LOCK TABLE hosted_service_executions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE users IN SHARE MODE;
LOCK TABLE agents IN SHARE MODE;
LOCK TABLE workflows IN SHARE MODE;
LOCK TABLE runs IN SHARE MODE;
LOCK TABLE workflow_runs IN SHARE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 074 requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM runtime_cluster_control
        WHERE singleton_id = 1
          AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 074 requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 074 requires zero running Runs';
    END IF;
END
$$;

-- Preserve the exact downstream Runtime creation identity for every legacy
-- Agent reservation that has not yet attached. SQL cannot reproduce the
-- Runtime RFC 8785 fingerprint safely, so migration 074 deliberately does not
-- guess which Run owns a colliding key. The service performs a read-only exact
-- lookup through Runtime before any mutable target validation.
ALTER TABLE hosted_service_executions
    ADD COLUMN downstream_replay_identity JSONB;

UPDATE hosted_service_executions e
SET downstream_replay_identity = jsonb_build_object(
    'version', 1,
    'kind', 'run',
    'source', 'api',
    'idempotency_key', 'hosted-service-order/' || e.external_order_id::text,
    'creation_protocol', 'hosted',
    'creation_method', 'service-order.execute',
    'metadata', jsonb_build_object(
        'external_order_id', e.external_order_id::text,
        'seller_user_id', e.seller_user_id::text,
        'trace_id', e.trace_id
    )
)
WHERE e.execution_id IS NULL
  AND e.target_type = 'agent';

UPDATE hosted_service_executions e
SET execution_kind = 'workflow_run',
    execution_id = wr.id
FROM workflow_runs wr
WHERE e.execution_id IS NULL
  AND e.target_type = 'workflow'
  AND wr.id = e.external_order_id
  AND wr.workflow_id = e.target_id
  AND wr.user_id = e.buyer_user_id;

-- This is an in-place metadata cutover. ACCESS EXCLUSIVE prevents concurrent
-- inserts, so every pre-cutover row is preserved without a copy/drop window.
DROP INDEX idx_hosted_service_executions_buyer;
DROP INDEX idx_hosted_service_executions_seller;
DROP INDEX idx_hosted_service_executions_execution;

ALTER TRIGGER hosted_service_executions_set_updated_at
    ON hosted_service_executions
    RENAME TO external_executions_set_updated_at;

ALTER TABLE hosted_service_executions RENAME TO external_executions;
ALTER TABLE external_executions
    RENAME COLUMN external_order_id TO external_request_id;
ALTER TABLE external_executions
    RENAME COLUMN buyer_user_id TO actor_user_id;
-- Preserve the exact pre-cutover owner solely as rollback evidence. Runtime
-- ownership may change after cutover, so a down migration must never infer
-- historical legacy data from the current Agent or Workflow owner.
ALTER TABLE external_executions
    RENAME COLUMN seller_user_id TO legacy_rollback_target_owner_id;
ALTER TABLE external_executions
    ALTER COLUMN legacy_rollback_target_owner_id DROP NOT NULL;

ALTER TABLE external_executions
    ADD COLUMN caller_service_id TEXT NOT NULL DEFAULT 'openlinker-cloud',
    ADD COLUMN request_fingerprint_version SMALLINT NOT NULL DEFAULT 1,
	ADD COLUMN start_state TEXT NOT NULL DEFAULT 'pending',
	ADD COLUMN start_token UUID,
	ADD COLUMN start_lease_until TIMESTAMPTZ,
	ADD COLUMN authorized_target_owner_id UUID,
	ADD COLUMN rejection_code TEXT;

UPDATE external_executions
SET start_state = 'authorized'
WHERE execution_id IS NOT NULL;

ALTER TABLE external_executions
    ALTER COLUMN caller_service_id DROP DEFAULT,
    ALTER COLUMN request_fingerprint_version SET DEFAULT 2,
    ADD CONSTRAINT external_executions_caller_service_id_valid
        CHECK (
            caller_service_id = btrim(caller_service_id)
            AND length(caller_service_id) BETWEEN 1 AND 200
        ),
	ADD CONSTRAINT external_executions_request_fingerprint_version_valid
		CHECK (request_fingerprint_version IN (1, 2)),
	ADD CONSTRAINT external_executions_start_state_valid
		CHECK ((
			(
				start_state = 'pending'
				AND start_token IS NULL
				AND start_lease_until IS NULL
				AND authorized_target_owner_id IS NULL
				AND rejection_code IS NULL
				AND execution_id IS NULL
			)
			OR (
				start_state = 'evaluating'
				AND start_token IS NOT NULL
				AND start_lease_until IS NOT NULL
				AND authorized_target_owner_id IS NULL
				AND rejection_code IS NULL
				AND execution_id IS NULL
			)
			OR (
				start_state = 'authorized'
				AND start_token IS NULL
				AND start_lease_until IS NULL
				AND (execution_id IS NOT NULL OR authorized_target_owner_id IS NOT NULL)
				AND rejection_code IS NULL
			)
			OR (
				start_state = 'rejected'
				AND start_token IS NULL
				AND start_lease_until IS NULL
				AND authorized_target_owner_id IS NULL
				AND rejection_code IN ('TARGET_UNAVAILABLE', 'TARGET_CONTRACT_CHANGED', 'DOWNSTREAM_IDENTITY_CONFLICT')
				AND execution_id IS NULL
			)
		) IS TRUE),
	ADD CONSTRAINT external_executions_downstream_replay_identity_valid
		CHECK ((
			(
				downstream_replay_identity IS NULL
				OR (
					request_fingerprint_version = 1
					AND target_type = 'agent'
					AND jsonb_typeof(downstream_replay_identity) = 'object'
					AND downstream_replay_identity ?& ARRAY[
						'version', 'kind', 'source', 'idempotency_key',
						'creation_protocol', 'creation_method', 'metadata'
					]
					AND downstream_replay_identity - ARRAY[
						'version', 'kind', 'source', 'idempotency_key',
						'creation_protocol', 'creation_method', 'metadata'
					] = '{}'::jsonb
					AND jsonb_typeof(downstream_replay_identity->'version') = 'number'
					AND downstream_replay_identity->'version' = '1'::jsonb
					AND jsonb_typeof(downstream_replay_identity->'kind') = 'string'
					AND downstream_replay_identity->>'kind' = 'run'
					AND jsonb_typeof(downstream_replay_identity->'source') = 'string'
					AND downstream_replay_identity->>'source' = 'api'
					AND jsonb_typeof(downstream_replay_identity->'idempotency_key') = 'string'
					AND length(downstream_replay_identity->>'idempotency_key') BETWEEN 1 AND 255
					AND downstream_replay_identity->>'idempotency_key' ~ '^[ -~]+$'
					AND jsonb_typeof(downstream_replay_identity->'creation_protocol') = 'string'
					AND length(downstream_replay_identity->>'creation_protocol') BETWEEN 1 AND 80
					AND downstream_replay_identity->>'creation_protocol' = btrim(downstream_replay_identity->>'creation_protocol')
					AND downstream_replay_identity->>'creation_protocol' = lower(downstream_replay_identity->>'creation_protocol')
					AND jsonb_typeof(downstream_replay_identity->'creation_method') = 'string'
					AND length(downstream_replay_identity->>'creation_method') BETWEEN 1 AND 120
					AND downstream_replay_identity->>'creation_method' = btrim(downstream_replay_identity->>'creation_method')
					AND downstream_replay_identity->>'creation_method' = lower(downstream_replay_identity->>'creation_method')
					AND jsonb_typeof(downstream_replay_identity->'metadata') = 'object'
				)
			)
			AND (
				request_fingerprint_version <> 1
				OR target_type <> 'agent'
				OR execution_id IS NOT NULL
				OR downstream_replay_identity IS NOT NULL
			)
		) IS TRUE);

ALTER TABLE external_executions
    DROP CONSTRAINT hosted_service_executions_pkey,
    ADD CONSTRAINT external_executions_pkey
        PRIMARY KEY (caller_service_id, external_request_id);
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_buyer_user_id_fkey
    TO external_executions_actor_user_id_fkey;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_seller_user_id_fkey
    TO external_executions_legacy_rollback_target_owner_id_fkey;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_target_type_check
    TO external_executions_target_type_check;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_input_fingerprint_check
    TO external_executions_input_fingerprint_check;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_trace_id_check
    TO external_executions_trace_id_check;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_execution_kind_check
    TO external_executions_execution_kind_check;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_check
    TO external_executions_attachment_complete;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_contract_hash_valid
    TO external_executions_contract_hash_valid;
ALTER TABLE external_executions RENAME CONSTRAINT hosted_service_executions_schema_fingerprint_valid
    TO external_executions_schema_fingerprint_valid;

CREATE INDEX idx_external_executions_actor
    ON external_executions (actor_user_id, created_at DESC);

CREATE UNIQUE INDEX idx_external_executions_execution
    ON external_executions (execution_kind, execution_id)
    WHERE execution_id IS NOT NULL;

COMMIT;
