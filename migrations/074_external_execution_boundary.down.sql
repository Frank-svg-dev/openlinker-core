BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.external-execution.migration.074', 0));

LOCK TABLE external_executions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE users IN SHARE MODE;
LOCK TABLE agents IN SHARE MODE;
LOCK TABLE workflows IN SHARE MODE;
LOCK TABLE runs IN SHARE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 074 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM runtime_cluster_control
        WHERE singleton_id = 1
          AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 074 rollback requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 074 rollback requires zero running Runs';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM external_executions
        WHERE caller_service_id <> 'openlinker-cloud'
    ) THEN
        RAISE EXCEPTION 'migration 074 rollback cannot represent multiple caller services';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM external_executions
        WHERE request_fingerprint_version <> 1
    ) THEN
        RAISE EXCEPTION 'migration 074 rollback cannot safely downgrade request fingerprint version 2';
    END IF;
	IF EXISTS (
		SELECT 1
		FROM external_executions
		WHERE start_state IN ('evaluating', 'rejected')
		   OR (start_state = 'authorized' AND execution_id IS NULL)
	) THEN
		RAISE EXCEPTION 'migration 074 rollback cannot represent in-flight or durable external execution start decisions';
	END IF;
	IF EXISTS (
		SELECT 1
		FROM external_executions
		WHERE legacy_rollback_target_owner_id IS NULL
	) THEN
		RAISE EXCEPTION 'migration 074 rollback is missing exact legacy target owner evidence';
	END IF;
END
$$;

DROP INDEX idx_external_executions_actor;
DROP INDEX idx_external_executions_execution;

ALTER TRIGGER external_executions_set_updated_at
    ON external_executions
    RENAME TO hosted_service_executions_set_updated_at;

ALTER TABLE external_executions
	ALTER COLUMN legacy_rollback_target_owner_id SET NOT NULL,
	DROP CONSTRAINT external_executions_pkey,
	DROP CONSTRAINT external_executions_caller_service_id_valid,
	DROP CONSTRAINT external_executions_request_fingerprint_version_valid,
	DROP CONSTRAINT external_executions_start_state_valid,
	DROP CONSTRAINT external_executions_downstream_replay_identity_valid,
	DROP COLUMN downstream_replay_identity,
	DROP COLUMN rejection_code,
	DROP COLUMN authorized_target_owner_id,
	DROP COLUMN start_lease_until,
	DROP COLUMN start_token,
	DROP COLUMN start_state,
	DROP COLUMN request_fingerprint_version,
    DROP COLUMN caller_service_id;

ALTER TABLE external_executions
    RENAME COLUMN legacy_rollback_target_owner_id TO seller_user_id;

ALTER TABLE external_executions
    RENAME COLUMN external_request_id TO external_order_id;
ALTER TABLE external_executions
    RENAME COLUMN actor_user_id TO buyer_user_id;
ALTER TABLE external_executions RENAME TO hosted_service_executions;

ALTER TABLE hosted_service_executions
    ADD CONSTRAINT hosted_service_executions_pkey PRIMARY KEY (external_order_id);
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_actor_user_id_fkey
    TO hosted_service_executions_buyer_user_id_fkey;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_legacy_rollback_target_owner_id_fkey
    TO hosted_service_executions_seller_user_id_fkey;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_target_type_check
    TO hosted_service_executions_target_type_check;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_input_fingerprint_check
    TO hosted_service_executions_input_fingerprint_check;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_trace_id_check
    TO hosted_service_executions_trace_id_check;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_execution_kind_check
    TO hosted_service_executions_execution_kind_check;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_attachment_complete
    TO hosted_service_executions_check;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_contract_hash_valid
    TO hosted_service_executions_contract_hash_valid;
ALTER TABLE hosted_service_executions RENAME CONSTRAINT external_executions_schema_fingerprint_valid
    TO hosted_service_executions_schema_fingerprint_valid;

CREATE INDEX idx_hosted_service_executions_buyer
    ON hosted_service_executions (buyer_user_id, created_at DESC);

CREATE INDEX idx_hosted_service_executions_seller
    ON hosted_service_executions (seller_user_id, created_at DESC);

CREATE UNIQUE INDEX idx_hosted_service_executions_execution
    ON hosted_service_executions (execution_kind, execution_id)
    WHERE execution_id IS NOT NULL;

COMMIT;
