BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.migration.070', 0));

LOCK TABLE agents IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
LOCK TABLE run_attempts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE run_cancellations IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    contract_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 070 requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM runtime_cluster_control
        WHERE singleton_id = 1
          AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 070 requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 070 requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 69
          AND migration_name = '069_runtime_entry_discovery'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = contract_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 070 requires the exact current schema contract 69';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM runtime_schema_contracts
        WHERE schema_version = 70
           OR migration_name = '070_sdk_first_runtime_boundary'
    ) THEN
        RAISE EXCEPTION 'migration 070 found a conflicting schema contract 70';
    END IF;
    IF EXISTS (
        SELECT 1 FROM agents
        WHERE connection_mode NOT IN ('direct_http', 'mcp_server', 'agent_node')
    ) OR EXISTS (
        SELECT 1 FROM runs
        WHERE connection_mode_snapshot IS NOT NULL
          AND connection_mode_snapshot NOT IN ('direct_http', 'mcp_server', 'agent_node')
    ) OR EXISTS (
        SELECT 1 FROM runs
        WHERE executor_type IS NOT NULL
          AND executor_type NOT IN ('agent_node', 'core_http', 'core_mcp')
    ) OR EXISTS (
        SELECT 1 FROM run_attempts
        WHERE executor_type NOT IN ('agent_node', 'core_http', 'core_mcp')
    ) THEN
        RAISE EXCEPTION 'migration 070 found an unknown connection or executor value';
    END IF;
    IF EXISTS (
        SELECT 1 FROM agents
        WHERE connection_mode = 'agent_node'
          AND endpoint_url NOT LIKE 'openlinker-agent-node://%'
    ) OR EXISTS (
        SELECT 1 FROM agents
        WHERE connection_mode <> 'agent_node'
          AND endpoint_url LIKE 'openlinker-agent-node://%'
    ) OR EXISTS (
        SELECT 1 FROM agents
        WHERE endpoint_url LIKE 'openlinker-runtime://%'
    ) THEN
        RAISE EXCEPTION 'migration 070 found a conflicting Runtime endpoint sentinel';
    END IF;
END
$$;

ALTER TABLE agents
    DROP CONSTRAINT agents_connection_mode_valid,
    DROP CONSTRAINT agents_runtime_queue_endpoint,
    DROP CONSTRAINT agents_endpoint_https;
ALTER TABLE runs
    DROP CONSTRAINT runs_connection_snapshot_consistent,
    DROP CONSTRAINT runs_connection_mode_snapshot_valid,
    DROP CONSTRAINT runs_executor_identity;
ALTER TABLE run_attempts
    DROP CONSTRAINT run_attempts_executor_valid,
    DROP CONSTRAINT run_attempts_executor_identity,
    DROP CONSTRAINT run_attempts_slot_shape;

DROP TRIGGER runs_v2_contract_identity ON runs;
DROP TRIGGER run_attempts_identity_immutable ON run_attempts;

UPDATE agents
SET connection_mode = 'runtime',
    endpoint_url = 'openlinker-runtime://' || substr(endpoint_url, char_length('openlinker-agent-node://') + 1),
    updated_at = clock_timestamp()
WHERE connection_mode = 'agent_node';

UPDATE runs
SET connection_mode_snapshot = 'runtime'
WHERE connection_mode_snapshot = 'agent_node';

UPDATE runs
SET executor_type = 'runtime'
WHERE executor_type = 'agent_node';

UPDATE run_attempts
SET executor_type = 'runtime'
WHERE executor_type = 'agent_node';

DO $$
DECLARE
    function_identity regprocedure;
    definition TEXT;
    function_identities CONSTANT regprocedure[] := ARRAY[
        'enforce_run_attempt_slot_evidence()'::regprocedure,
        'enforce_run_attempt_slot_release_on_finish()'::regprocedure,
        'enforce_run_active_attempt_consistency()'::regprocedure,
        'enforce_run_terminal_artifacts_consistency()'::regprocedure
    ];
BEGIN
    FOREACH function_identity IN ARRAY function_identities LOOP
        definition := pg_get_functiondef(function_identity);
        IF POSITION('''agent_node''' IN definition) = 0 THEN
            RAISE EXCEPTION 'migration 070 Runtime invariant source mismatch for %', function_identity;
        END IF;
        definition := replace(definition, '''agent_node''', '''runtime''');
        EXECUTE definition;
        IF POSITION('''agent_node''' IN pg_get_functiondef(function_identity)) <> 0 THEN
            RAISE EXCEPTION 'migration 070 left an old executor value in %', function_identity;
        END IF;
    END LOOP;
END
$$;

-- The set-based label rewrite queues deferred invariant checks on Runs and
-- Attempts. Flush them after the trigger functions understand the new label
-- and before altering either table; PostgreSQL rejects table DDL while those
-- trigger events are still pending.
SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE agents
    ADD CONSTRAINT agents_connection_mode_valid
        CHECK (connection_mode IN ('direct_http', 'mcp_server', 'runtime')),
    ADD CONSTRAINT agents_runtime_queue_endpoint
        CHECK (
            connection_mode <> 'runtime'
            OR endpoint_url LIKE 'openlinker-runtime://%'
        ),
    ADD CONSTRAINT agents_endpoint_https CHECK (
        endpoint_url LIKE 'https://%' OR
        endpoint_url = 'http://localhost' OR
        endpoint_url LIKE 'http://localhost:%' OR
        endpoint_url LIKE 'http://localhost/%' OR
        endpoint_url = 'http://127.0.0.1' OR
        endpoint_url LIKE 'http://127.0.0.1:%' OR
        endpoint_url LIKE 'http://127.0.0.1/%' OR
        endpoint_url = 'http://[::1]' OR
        endpoint_url LIKE 'http://[::1]:%' OR
        endpoint_url LIKE 'http://[::1]/%' OR
        endpoint_url LIKE 'openlinker-runtime://%'
    );

ALTER TABLE runs
    ADD CONSTRAINT runs_connection_snapshot_consistent
        CHECK (
            (
                runtime_contract_id = 'legacy.pre-v2'
                AND connection_mode_snapshot IS NULL
                AND endpoint_idempotency_snapshot IS NULL
            )
            OR
            (
                runtime_contract_id = 'openlinker.runtime.v2'
                AND connection_mode_snapshot IN ('direct_http', 'mcp_server', 'runtime')
                AND (
                    connection_mode_snapshot NOT IN ('direct_http', 'mcp_server')
                    OR endpoint_idempotency_snapshot IS NOT NULL
                )
            )
        ),
    ADD CONSTRAINT runs_connection_mode_snapshot_valid
        CHECK (
            connection_mode_snapshot IS NULL
            OR connection_mode_snapshot IN ('direct_http', 'mcp_server', 'runtime')
        ),
    ADD CONSTRAINT runs_executor_identity
        CHECK (
            dispatch_state NOT IN ('offered', 'executing')
            OR (
                executor_type = 'runtime'
                AND runtime_node_id IS NOT NULL
                AND runtime_worker_id IS NOT NULL
                AND runtime_session_id IS NOT NULL
                AND lease_token_id IS NOT NULL
            )
            OR (
                executor_type IN ('core_http', 'core_mcp')
                AND runtime_node_id IS NULL
                AND runtime_worker_id IS NULL
                AND runtime_session_id IS NULL
                AND lease_token_id IS NULL
            )
        );

ALTER TABLE run_attempts
    ADD CONSTRAINT run_attempts_executor_valid
        CHECK (executor_type IN ('runtime', 'core_http', 'core_mcp')),
    ADD CONSTRAINT run_attempts_executor_identity
        CHECK (
            (
                executor_type = 'runtime'
                AND runtime_token_id IS NOT NULL
                AND runtime_worker_id IS NOT NULL
                AND runtime_session_id IS NOT NULL
                AND node_id IS NOT NULL
            )
            OR
            (
                executor_type IN ('core_http', 'core_mcp')
                AND runtime_token_id IS NULL
                AND runtime_worker_id IS NULL
                AND runtime_session_id IS NULL
                AND node_id IS NULL
            )
        ),
    ADD CONSTRAINT run_attempts_slot_shape
        CHECK (
            (
                executor_type = 'runtime'
                AND slot_acquired_at IS NOT NULL
                AND (
                    (
                        slot_released_at IS NULL
                        AND active_runtime_session_id IS NOT NULL
                    )
                    OR (
                        slot_released_at IS NOT NULL
                        AND active_runtime_session_id IS NULL
                    )
                )
            )
            OR (
                executor_type IN ('core_http', 'core_mcp')
                AND slot_acquired_at IS NULL
                AND slot_released_at IS NULL
                AND active_runtime_session_id IS NULL
            )
        );

CREATE TRIGGER runs_v2_contract_identity
    BEFORE INSERT OR UPDATE OR DELETE ON runs
    FOR EACH ROW EXECUTE FUNCTION enforce_run_v2_contract_identity();

CREATE TRIGGER run_attempts_identity_immutable
    BEFORE UPDATE OR DELETE ON run_attempts
    FOR EACH ROW EXECUTE FUNCTION enforce_run_attempt_identity_immutable();

UPDATE runtime_schema_contracts
SET schema_version = 70,
    migration_name = '070_sdk_first_runtime_boundary',
    applied_at = clock_timestamp()
WHERE schema_version = 69
  AND migration_name = '069_runtime_entry_discovery'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53'
  AND is_current;

COMMIT;
