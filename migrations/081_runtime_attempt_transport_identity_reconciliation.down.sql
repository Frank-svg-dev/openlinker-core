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
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 081 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 081 rollback requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 081 rollback requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 80
          AND migration_name = '080_runtime_attempt_transport_evidence'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 081 rollback requires the exact canonical schema contract 80';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'run_attempts'
          AND column_name = 'runtime_attachment_id'
          AND udt_name = 'uuid'
          AND is_nullable = 'YES'
    ) THEN
        RAISE EXCEPTION 'migration 081 rollback transport evidence column is missing';
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
        RAISE EXCEPTION 'migration 081 rollback transport evidence constraints are missing or unvalidated';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgrelid = 'run_attempts'::regclass
          AND tgname = 'run_attempts_runtime_attachment_evidence'
          AND tgenabled = 'O'
          AND NOT tgisinternal
    ) OR to_regprocedure('enforce_run_attempt_runtime_attachment_evidence()') IS NULL THEN
        RAISE EXCEPTION 'migration 081 rollback transport evidence trigger is missing';
    END IF;
END
$$;

-- 081 is an identity reconciliation marker. Removing its migrate version must
-- not recreate the non-canonical intermediate 079 identity or remove physical
-- transport evidence owned by canonical migration 080.

COMMIT;
