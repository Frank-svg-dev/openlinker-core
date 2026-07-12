BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.v2.migration.067', 0));

LOCK TABLE runtime_session_attachments IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_sessions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_nodes IN ACCESS EXCLUSIVE MODE;
LOCK TABLE agents IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE task_queries IN ACCESS EXCLUSIVE MODE;
LOCK TABLE user_tokens IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE user_token_core_grants IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE run_attempts IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE run_cancellations IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 067 requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 067 requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 067 requires zero nonterminal Runs';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 66
          AND migration_name = '066_runtime_v2_deadline_reconciler'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61'
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 067 requires the exact current schema contract 66';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM runtime_schema_contracts
        WHERE (
                schema_version = 67
                OR migration_name = '067_runtime_v2_core_execution'
                OR runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
              )
          AND NOT (
                schema_version = 67
                AND migration_name = '067_runtime_v2_core_execution'
                AND runtime_contract_id = 'openlinker.runtime.v2'
                AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
                AND NOT is_current
              )
    ) THEN
        RAISE EXCEPTION 'migration 067 found a conflicting historical schema contract 67';
    END IF;
END
$$;

-- Old-digest Sessions are immutable identity history. Detach and close them;
-- a new Agent Node process must negotiate the new canonical contract.
UPDATE runtime_session_attachments attachment
SET detached_at = clock_timestamp(),
    disconnect_reason = 'runtime contract digest cutover'
FROM runtime_sessions session
WHERE session.runtime_session_id = attachment.runtime_session_id
  AND session.runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61'
  AND session.status IN ('active', 'draining')
  AND attachment.detached_at IS NULL;

UPDATE runtime_sessions
SET status = 'closed',
    attached_core_instance_id = NULL,
    disconnected_at = COALESCE(disconnected_at, clock_timestamp()),
    heartbeat_at = GREATEST(heartbeat_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61'
  AND status IN ('active', 'draining');

SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_current;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_current;
ALTER TABLE agents
    DROP CONSTRAINT agents_connection_mode_valid,
    DROP CONSTRAINT agents_runtime_queue_endpoint,
    DROP CONSTRAINT agents_endpoint_https;
ALTER TABLE runs
    DROP CONSTRAINT runs_connection_snapshot_consistent,
    DROP CONSTRAINT runs_connection_mode_snapshot_valid;

-- Marketplace connection mode describes the execution owner, not the wire
-- transport. Agent Node chooses WebSocket or Pull v2 per Session.
UPDATE agents
SET connection_mode = 'agent_node',
    endpoint_url = 'openlinker-agent-node://' || slug,
    mcp_tool_name = NULL,
    endpoint_auth_header = NULL,
    updated_at = clock_timestamp()
WHERE connection_mode IN ('runtime_pull', 'runtime_ws');

UPDATE runs
SET connection_mode_snapshot = 'agent_node'
WHERE runtime_contract_id = 'openlinker.runtime.v2'
  AND connection_mode_snapshot IN ('runtime_pull', 'runtime_ws');

ALTER TABLE agents
    ADD CONSTRAINT agents_connection_mode_valid
        CHECK (connection_mode IN ('direct_http', 'mcp_server', 'agent_node')),
    ADD CONSTRAINT agents_runtime_queue_endpoint
        CHECK (
            connection_mode <> 'agent_node'
            OR endpoint_url LIKE 'openlinker-agent-node://%'
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
        endpoint_url LIKE 'openlinker-agent-node://%'
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
                AND connection_mode_snapshot IN ('direct_http', 'mcp_server', 'agent_node')
                AND (
                    connection_mode_snapshot NOT IN ('direct_http', 'mcp_server')
                    OR endpoint_idempotency_snapshot IS NOT NULL
                )
            )
        ),
    ADD CONSTRAINT runs_connection_mode_snapshot_valid
        CHECK (
            connection_mode_snapshot IS NULL
            OR connection_mode_snapshot IN ('direct_http', 'mcp_server', 'agent_node')
        );

-- Tasks remain private demand context for recommendation and execution. The
-- public marketplace is seller-owned Agent supply, not buyer task postings.
-- This is a pre-1.0 breaking cutover: the former public board, worker claim,
-- delivery and review columns are removed from the live schema instead of
-- being retained as a dormant compatibility contract.
ALTER TABLE task_queries
    DROP CONSTRAINT task_queries_completion_consistency;

UPDATE task_queries
SET chosen_agent_id = COALESCE(chosen_agent_id, claimed_agent_id),
    chosen_at = COALESCE(chosen_at, claimed_at),
    completion_run_id = COALESCE(completion_run_id, claim_run_id);

UPDATE task_queries task
SET chosen_agent_id = run.agent_id,
    chosen_at = COALESCE(task.chosen_at, task.completed_at, run.finished_at, run.started_at),
    completed_at = COALESCE(task.completed_at, run.finished_at, run.started_at)
FROM runs run
WHERE task.completion_run_id = run.id;

-- Old board completions without durable Run evidence cannot satisfy the new
-- private result contract. Keep the Task, but reset that unverifiable terminal
-- marker so a future successful Run can complete it authoritatively.
UPDATE task_queries
SET completed_at = NULL,
    completion_summary = NULL
WHERE completion_run_id IS NULL;

DROP INDEX IF EXISTS idx_task_queries_public_board;
DROP INDEX IF EXISTS idx_task_queries_claimed_by_user;
DROP INDEX IF EXISTS idx_task_queries_claimed_agent;
DROP INDEX IF EXISTS idx_task_queries_delivery_status;

ALTER TABLE task_queries
    DROP COLUMN claimed_agent_id,
    DROP COLUMN claimed_by_user_id,
    DROP COLUMN claimed_at,
    DROP COLUMN claim_run_id,
    DROP COLUMN delivery_status,
    DROP COLUMN delivery_visibility,
    DROP COLUMN delivery_artifact,
    DROP COLUMN accepted_at,
    DROP COLUMN revision_requested_at,
    DROP COLUMN revision_note,
    DROP COLUMN visibility,
    DROP COLUMN public_summary,
    DROP COLUMN published_at,
    ADD CONSTRAINT task_queries_completion_consistency CHECK (
        (
            completed_at IS NULL
            AND completion_run_id IS NULL
            AND completion_summary IS NULL
        )
        OR (
            completed_at IS NOT NULL
            AND completion_run_id IS NOT NULL
            AND chosen_agent_id IS NOT NULL
        )
    );

DELETE FROM user_token_core_grants
WHERE permission IN ('tasks:write', 'tasks:publish', 'tasks:work', 'tasks:review');

UPDATE user_tokens token
SET scopes = ARRAY(
        SELECT scope
        FROM unnest(COALESCE(token.scopes, ARRAY[]::TEXT[])) AS expanded(scope)
        WHERE scope NOT IN ('tasks:write', 'tasks:publish', 'tasks:work', 'tasks:review')
    ),
    updated_at = clock_timestamp()
WHERE COALESCE(token.scopes, ARRAY[]::TEXT[])
      && ARRAY['tasks:write', 'tasks:publish', 'tasks:work', 'tasks:review']::TEXT[];

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 66
  AND migration_name = '066_runtime_v2_deadline_reconciler'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61'
  AND is_current;

INSERT INTO runtime_schema_contracts (
    schema_version, migration_name, runtime_contract_id,
    runtime_contract_digest, is_current
) VALUES (
    67,
    '067_runtime_v2_core_execution',
    'openlinker.runtime.v2',
    '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
    TRUE
)
ON CONFLICT (schema_version) DO UPDATE
SET is_current = TRUE;

UPDATE runtime_nodes
SET runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
    updated_at = clock_timestamp()
WHERE status <> 'revoked'
  AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61';

SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE runtime_nodes
    ADD CONSTRAINT runtime_nodes_contract_current
        CHECK (
            protocol_version = 2
            AND runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (status IN ('active', 'draining')
                 AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f')
                OR
                (status = 'revoked'
                 AND runtime_contract_digest IN (
                     '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                     '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
                 ))
            )
        );

ALTER TABLE runtime_sessions
    ADD CONSTRAINT runtime_sessions_contract_current
        CHECK (
            protocol_version = 2
            AND runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (status IN ('active', 'draining')
                 AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f')
                OR
                (status IN ('offline', 'revoked', 'closed')
                 AND runtime_contract_digest IN (
                     '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                     '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
                 ))
            )
        );

-- Cancellation is two-phase for Core-owned Attempts too. Public Run
-- cancellation clears the active summary immediately; stopped/unconfirmed
-- Attempt evidence is written only after goroutine exit or deadline reaping.
DO $migration$
DECLARE
    definition TEXT;
    old_fragment TEXT := $old$latest_attempt.executor_type IS DISTINCT FROM 'agent_node'$old$;
    new_fragment TEXT := $new$latest_attempt.executor_type NOT IN ('agent_node', 'core_http', 'core_mcp')$new$;
BEGIN
    definition := pg_get_functiondef('enforce_run_active_attempt_consistency()'::regprocedure);
    IF POSITION(old_fragment IN definition) = 0 THEN
        RAISE EXCEPTION 'migration 067 active Attempt invariant source mismatch';
    END IF;
    EXECUTE replace(definition, old_fragment, new_fragment);

    definition := pg_get_functiondef('enforce_run_terminal_artifacts_consistency()'::regprocedure);
    IF POSITION(old_fragment IN definition) = 0 THEN
        RAISE EXCEPTION 'migration 067 terminal artifact invariant source mismatch';
    END IF;
    EXECUTE replace(definition, old_fragment, new_fragment);
END
$migration$;

COMMIT;
