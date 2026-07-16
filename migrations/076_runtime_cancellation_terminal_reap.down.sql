BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.migration.076', 0));

LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE run_attempts IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE run_cancellations IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 076 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 076 rollback requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 076 rollback requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 76
          AND migration_name = '076_runtime_cancellation_terminal_reap'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 076 rollback requires the exact current schema contract 76';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 75
          AND migration_name = '075_runtime_wire_compatibility'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 076 rollback requires the exact historical schema contract 75';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM run_cancellations c
        JOIN run_attempts a
          ON a.run_id = c.run_id
         AND a.id = c.target_attempt_id
        WHERE c.state IN ('unsupported', 'failed')
          AND a.finished_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'migration 076 rollback refuses reaped negative terminal cancellation evidence';
    END IF;
END
$$;

DO $migration$
DECLARE
    definition TEXT;
    old_declaration TEXT := $old$
    cancellation_target_attempt_id UUID;
    cancellation_state TEXT;
    cancellation_requested_at TIMESTAMPTZ;
BEGIN$old$;
    new_declaration TEXT := $new$
    cancellation_target_attempt_id UUID;
    cancellation_state TEXT;
BEGIN$new$;
    old_select TEXT := $old$
            SELECT target_attempt_id, state, requested_at
            INTO cancellation_target_attempt_id, cancellation_state, cancellation_requested_at
            FROM run_cancellations
            WHERE run_id = current_run.id;$old$;
    new_select TEXT := $new$
            SELECT target_attempt_id, state
            INTO cancellation_target_attempt_id, cancellation_state
            FROM run_cancellations
            WHERE run_id = current_run.id;$new$;
    old_lifecycle TEXT := $old$
                       OR (
                           cancellation_state IN ('requested', 'delivered', 'stopping')
                           AND (
                               latest_attempt.executor_type NOT IN ('runtime', 'core_http', 'core_mcp')
                               OR latest_attempt.finished_at IS NOT NULL
                               OR latest_attempt.outcome IS NOT NULL
                           )
                       )
                       OR (
                           cancellation_state IN ('unsupported', 'failed')
                           AND (
                               latest_attempt.executor_type IS DISTINCT FROM 'runtime'
                               OR (
                                   latest_attempt.finished_at IS NULL
                                   AND latest_attempt.outcome IS NOT NULL
                               )
                               OR (
                                   latest_attempt.finished_at IS NOT NULL
                                   AND (
                                       latest_attempt.outcome IS DISTINCT FROM 'canceled'
                                       OR latest_attempt.error_code IS DISTINCT FROM 'CANCEL_UNCONFIRMED'
                                       OR cancellation_requested_at IS NULL
                                       OR latest_attempt.finished_at
                                           < cancellation_requested_at + INTERVAL '30 seconds'
                                   )
                               )
                           )
                       )$old$;
    new_lifecycle TEXT := $new$
                       OR (
                           cancellation_state IN ('requested', 'delivered', 'stopping', 'unsupported', 'failed')
                           AND (
                               latest_attempt.executor_type NOT IN ('runtime', 'core_http', 'core_mcp')
                               OR latest_attempt.finished_at IS NOT NULL
                               OR latest_attempt.outcome IS NOT NULL
                           )
                       )$new$;
BEGIN
    definition := pg_get_functiondef('enforce_run_terminal_artifacts_consistency()'::regprocedure);
    IF POSITION(old_declaration IN definition) = 0
       OR POSITION(old_select IN definition) = 0
       OR POSITION(old_lifecycle IN definition) = 0 THEN
        RAISE EXCEPTION 'migration 076 rollback terminal artifact invariant source mismatch';
    END IF;
    definition := replace(definition, old_declaration, new_declaration);
    definition := replace(definition, old_select, new_select);
    definition := replace(definition, old_lifecycle, new_lifecycle);
    EXECUTE definition;
END
$migration$;

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 76
  AND migration_name = '076_runtime_cancellation_terminal_reap'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND is_current;

DELETE FROM runtime_schema_contracts
WHERE schema_version = 76
  AND migration_name = '076_runtime_cancellation_terminal_reap'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND NOT is_current;

UPDATE runtime_schema_contracts
SET is_current = TRUE
WHERE schema_version = 75
  AND migration_name = '075_runtime_wire_compatibility'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND NOT is_current;

COMMIT;
