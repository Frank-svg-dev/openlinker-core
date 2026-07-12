DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM runtime_schema_contracts
        WHERE schema_version = 67
          AND migration_name = '067_runtime_v2_core_execution'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
          AND is_current
    ) OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 67 is missing or mismatched';
    END IF;

    IF EXISTS (
        SELECT 1 FROM runtime_sessions
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest <> '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
    ) OR EXISTS (
        SELECT 1 FROM runtime_nodes
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest <> '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
    ) THEN
        RAISE EXCEPTION 'active Runtime principals retain an old contract digest';
    END IF;

    IF EXISTS (
        SELECT 1 FROM agents
        WHERE connection_mode IN ('runtime_pull', 'runtime_ws')
    ) OR EXISTS (
        SELECT 1 FROM runs
        WHERE connection_mode_snapshot IN ('runtime_pull', 'runtime_ws')
    ) THEN
        RAISE EXCEPTION 'seller connection modes were not consolidated to agent_node';
    END IF;

    IF EXISTS (
        SELECT 1 FROM user_token_core_grants
        WHERE permission IN ('tasks:write', 'tasks:publish', 'tasks:work', 'tasks:review')
    ) OR EXISTS (
        SELECT 1 FROM user_tokens
        WHERE COALESCE(scopes, ARRAY[]::TEXT[])
              && ARRAY['tasks:write', 'tasks:publish', 'tasks:work', 'tasks:review']::TEXT[]
    ) THEN
        RAISE EXCEPTION 'public Task marketplace exposure or grants remain';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'task_queries'
          AND column_name IN (
              'claimed_agent_id', 'claimed_by_user_id', 'claimed_at', 'claim_run_id',
              'delivery_status', 'delivery_visibility', 'delivery_artifact',
              'accepted_at', 'revision_requested_at', 'revision_note',
              'visibility', 'public_summary', 'published_at'
          )
    ) OR to_regclass('public.idx_task_queries_public_board') IS NOT NULL THEN
        RAISE EXCEPTION 'retired Task marketplace columns or indexes remain';
    END IF;

    IF COALESCE(pg_get_constraintdef(
           (SELECT oid FROM pg_constraint
            WHERE conrelid = 'task_queries'::regclass
              AND conname = 'task_queries_completion_consistency')
       ), '') NOT LIKE '%completion_run_id%chosen_agent_id%' THEN
        RAISE EXCEPTION 'private Task completion consistency is missing';
    END IF;

    IF COALESCE(pg_get_constraintdef(
           (SELECT oid FROM pg_constraint
            WHERE conrelid = 'agents'::regclass
              AND conname = 'agents_connection_mode_valid')
       ), '') NOT LIKE '%agent_node%'
       OR COALESCE(pg_get_constraintdef(
           (SELECT oid FROM pg_constraint
            WHERE conrelid = 'runs'::regclass
              AND conname = 'runs_connection_mode_snapshot_valid')
       ), '') NOT LIKE '%agent_node%' THEN
        RAISE EXCEPTION 'agent_node connection mode constraints are missing';
    END IF;

    IF pg_get_functiondef('enforce_run_active_attempt_consistency()'::regprocedure)
           NOT LIKE '%core_http%core_mcp%'
       OR pg_get_functiondef('enforce_run_terminal_artifacts_consistency()'::regprocedure)
           NOT LIKE '%core_http%core_mcp%' THEN
        RAISE EXCEPTION 'Core two-phase cancellation invariants are missing';
    END IF;
END
$$;
