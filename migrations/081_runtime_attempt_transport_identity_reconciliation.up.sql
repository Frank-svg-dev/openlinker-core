BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.migration.081', 0));

LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE run_attempts IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE runtime_session_attachments IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    current_digest CONSTANT TEXT := '4be9b2fe09eeedf0e37119075134064be88f93b301c502cdfa21a6cb978c6481';
    canonical_current_count INTEGER;
    intermediate_current_count INTEGER;
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 081 requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 081 requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 081 requires zero running Runs';
    END IF;
    IF (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 081 requires exactly one current Runtime schema contract';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 77
          AND migration_name = '077_external_execution_cancellation'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 081 requires the exact historical schema contract 77';
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'run_attempts'
          AND column_name = 'runtime_attachment_id'
          AND udt_name = 'uuid'
          AND is_nullable = 'YES'
    ) THEN
        RAISE EXCEPTION 'migration 081 transport evidence column is missing';
    END IF;
    IF (
        SELECT COUNT(*) FROM pg_constraint
        WHERE conrelid IN (
            'run_attempts'::regclass,
            'runtime_session_attachments'::regclass
        )
          AND conname IN (
              'run_attempts_runtime_attachment_state',
              'run_attempts_runtime_attachment_identity_fk',
              'runtime_session_attachments_attempt_identity_unique'
          )
          AND convalidated
    ) <> 3 THEN
        RAISE EXCEPTION 'migration 081 transport evidence constraints are missing or unvalidated';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgrelid = 'run_attempts'::regclass
          AND tgname = 'run_attempts_runtime_attachment_evidence'
          AND tgenabled = 'O'
          AND NOT tgisinternal
    ) OR to_regprocedure('enforce_run_attempt_runtime_attachment_evidence()') IS NULL THEN
        RAISE EXCEPTION 'migration 081 transport evidence trigger is missing';
    END IF;
    IF to_regclass('idx_runtime_sessions_credential_lifecycle') IS NULL THEN
        RAISE EXCEPTION 'migration 081 predecessor lifecycle index is missing';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM run_attempts attempt
        LEFT JOIN runtime_session_attachments attachment
          ON attachment.id = attempt.runtime_attachment_id
         AND attachment.runtime_session_id = attempt.runtime_session_id
        WHERE attempt.runtime_attachment_id IS NOT NULL
          AND (
              attempt.executor_type <> 'runtime'
              OR attempt.accepted_at IS NULL
              OR attachment.id IS NULL
              OR attachment.transport NOT IN ('websocket', 'long_poll')
              OR attachment.transport_reason IS NULL
          )
    ) THEN
        RAISE EXCEPTION 'migration 081 transport evidence is inconsistent';
    END IF;

    SELECT COUNT(*) INTO canonical_current_count
    FROM runtime_schema_contracts
    WHERE schema_version = 80
      AND migration_name = '080_runtime_attempt_transport_evidence'
      AND runtime_contract_id = 'openlinker.runtime.v2'
      AND runtime_contract_digest = current_digest
      AND is_current;

    SELECT COUNT(*) INTO intermediate_current_count
    FROM runtime_schema_contracts
    WHERE schema_version = 79
      AND migration_name = '079_runtime_attempt_transport_evidence'
      AND runtime_contract_id = 'openlinker.runtime.v2'
      AND runtime_contract_digest = current_digest
      AND is_current;

    IF canonical_current_count = 1 THEN
        IF EXISTS (
            SELECT 1 FROM runtime_schema_contracts
            WHERE (schema_version = 79 OR migration_name = '079_runtime_attempt_transport_evidence')
              AND NOT (
                  schema_version = 79
                  AND migration_name = '079_runtime_attempt_transport_evidence'
                  AND runtime_contract_id = 'openlinker.runtime.v2'
                  AND runtime_contract_digest = current_digest
                  AND NOT is_current
              )
        ) THEN
            RAISE EXCEPTION 'migration 081 found conflicting intermediate schema contract 79 history';
        END IF;
    ELSIF intermediate_current_count = 1 THEN
        IF EXISTS (
            SELECT 1 FROM runtime_schema_contracts
            WHERE schema_version = 80
               OR migration_name = '080_runtime_attempt_transport_evidence'
        ) THEN
            RAISE EXCEPTION 'migration 081 found conflicting canonical schema contract 80 history';
        END IF;

        UPDATE runtime_schema_contracts
        SET is_current = FALSE
        WHERE schema_version = 79
          AND migration_name = '079_runtime_attempt_transport_evidence'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current;

        INSERT INTO runtime_schema_contracts (
            schema_version, migration_name, runtime_contract_id,
            runtime_contract_digest, is_current
        ) VALUES (
            80,
            '080_runtime_attempt_transport_evidence',
            'openlinker.runtime.v2',
            current_digest,
            TRUE
        );
    ELSE
        RAISE EXCEPTION 'migration 081 requires canonical 080 or the exact deployed intermediate 079 contract';
    END IF;
END
$$;

COMMIT;
