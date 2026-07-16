DO $$
DECLARE
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
    terminal_definition TEXT;
    cancellation_definition TEXT;
BEGIN
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 76
          AND migration_name = '076_runtime_cancellation_terminal_reap'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 76 is missing or mismatched';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 75
          AND migration_name = '075_runtime_wire_compatibility'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 75 history is missing or mismatched';
    END IF;

    terminal_definition := pg_get_functiondef(
        'enforce_run_terminal_artifacts_consistency()'::regprocedure
    );
    IF terminal_definition NOT LIKE '%cancellation_requested_at TIMESTAMPTZ%'
       OR terminal_definition NOT LIKE '%unsupported%failed%CANCEL_UNCONFIRMED%'
       OR terminal_definition NOT LIKE '%finished_at%< cancellation_requested_at + INTERVAL ''30 seconds''%'
       OR POSITION(
           'cancellation_state IN (''requested'', ''delivered'', ''stopping'', ''unsupported'', ''failed'')'
           IN terminal_definition
       ) <> 0 THEN
        RAISE EXCEPTION 'negative terminal cancellation deadline invariant is missing or over-broad';
    END IF;

    cancellation_definition := pg_get_functiondef(
        'enforce_run_cancellation_transition()'::regprocedure
    );
    IF cancellation_definition NOT LIKE '%terminal cancellation state cannot change%'
       OR cancellation_definition NOT LIKE '%terminal cancellation evidence is immutable%' THEN
        RAISE EXCEPTION 'terminal cancellation state or original error evidence is not immutable';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM runs r
        JOIN run_cancellations c
          ON c.run_id = r.id
         AND c.id = r.cancel_request_id
        JOIN run_attempts a
          ON a.run_id = c.run_id
         AND a.id = c.target_attempt_id
        WHERE r.runtime_contract_id = 'openlinker.runtime.v2'
          AND r.status = 'canceled'
          AND c.state IN ('unsupported', 'failed')
          AND (
              a.executor_type IS DISTINCT FROM 'runtime'
              OR (
                  a.finished_at IS NULL
                  AND (
                      a.outcome IS NOT NULL
                      OR a.slot_released_at IS NOT NULL
                      OR a.active_runtime_session_id IS NULL
                  )
              )
              OR (
                  a.finished_at IS NOT NULL
                  AND (
                      a.outcome IS DISTINCT FROM 'canceled'
                      OR a.error_code IS DISTINCT FROM 'CANCEL_UNCONFIRMED'
                      OR a.finished_at < c.requested_at + INTERVAL '30 seconds'
                      OR a.slot_released_at IS NULL
                      OR a.active_runtime_session_id IS NOT NULL
                  )
              )
          )
    ) THEN
        RAISE EXCEPTION 'stored negative terminal cancellation reap evidence is inconsistent';
    END IF;
END
$$;
