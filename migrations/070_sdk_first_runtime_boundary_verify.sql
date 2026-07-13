DO $$
DECLARE
    contract_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
    definition TEXT;
    function_identity regprocedure;
BEGIN
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 70
          AND migration_name = '070_sdk_first_runtime_boundary'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = contract_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 70 is missing or mismatched';
    END IF;

    IF EXISTS (
        SELECT 1 FROM agents WHERE connection_mode = 'agent_node'
    ) OR EXISTS (
        SELECT 1 FROM agents WHERE endpoint_url LIKE 'openlinker-agent-node://%'
    ) OR EXISTS (
        SELECT 1 FROM runs WHERE connection_mode_snapshot = 'agent_node'
    ) OR EXISTS (
        SELECT 1 FROM runs WHERE executor_type = 'agent_node'
    ) OR EXISTS (
        SELECT 1 FROM run_attempts WHERE executor_type = 'agent_node'
    ) THEN
        RAISE EXCEPTION 'active data retained an agent_node product identity';
    END IF;

    IF EXISTS (
        SELECT 1 FROM agents
        WHERE connection_mode = 'runtime'
          AND endpoint_url NOT LIKE 'openlinker-runtime://%'
    ) OR EXISTS (
        SELECT 1 FROM agents
        WHERE connection_mode <> 'runtime'
          AND endpoint_url LIKE 'openlinker-runtime://%'
    ) THEN
        RAISE EXCEPTION 'Runtime endpoint sentinel does not match connection mode';
    END IF;

    FOR definition IN
        SELECT pg_get_constraintdef(oid)
        FROM pg_constraint
        WHERE conname IN (
            'agents_connection_mode_valid',
            'agents_runtime_queue_endpoint',
            'agents_endpoint_https',
            'runs_connection_snapshot_consistent',
            'runs_connection_mode_snapshot_valid',
            'runs_executor_identity',
            'run_attempts_executor_valid',
            'run_attempts_executor_identity',
            'run_attempts_slot_shape'
        )
    LOOP
        IF definition LIKE '%agent_node%' THEN
            RAISE EXCEPTION 'active constraint retained agent_node: %', definition;
        END IF;
    END LOOP;

    FOREACH function_identity IN ARRAY ARRAY[
        'enforce_run_attempt_slot_evidence()'::regprocedure,
        'enforce_run_attempt_slot_release_on_finish()'::regprocedure,
        'enforce_run_active_attempt_consistency()'::regprocedure,
        'enforce_run_terminal_artifacts_consistency()'::regprocedure
    ] LOOP
        IF pg_get_functiondef(function_identity) LIKE '%''agent_node''%' THEN
            RAISE EXCEPTION 'active Runtime invariant retained agent_node: %', function_identity;
        END IF;
    END LOOP;

    IF (
        SELECT COUNT(*)
        FROM pg_trigger
        WHERE tgrelid = 'runs'::regclass
          AND tgname = 'runs_v2_contract_identity'
          AND NOT tgisinternal
          AND tgenabled <> 'D'
    ) <> 1 OR (
        SELECT COUNT(*)
        FROM pg_trigger
        WHERE tgrelid = 'run_attempts'::regclass
          AND tgname = 'run_attempts_identity_immutable'
          AND NOT tgisinternal
          AND tgenabled <> 'D'
    ) <> 1 THEN
        RAISE EXCEPTION 'Runtime immutable identity trigger was not restored';
    END IF;
END
$$;
