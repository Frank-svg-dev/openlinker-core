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
        RAISE EXCEPTION 'migration 067 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) OR EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 067 rollback requires hard maintenance and zero nonterminal Runs';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 67
          AND migration_name = '067_runtime_v2_core_execution'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 067 rollback requires the exact current schema contract 67';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 66
          AND migration_name = '066_runtime_v2_deadline_reconciler'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61'
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 067 rollback requires the exact historical schema contract 66';
    END IF;
END
$$;

UPDATE runtime_session_attachments attachment
SET detached_at = clock_timestamp(),
    disconnect_reason = 'runtime contract rollback'
FROM runtime_sessions session
WHERE session.runtime_session_id = attachment.runtime_session_id
  AND session.runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
  AND session.status IN ('active', 'draining')
  AND attachment.detached_at IS NULL;

UPDATE runtime_sessions
SET status = 'closed',
    attached_core_instance_id = NULL,
    disconnected_at = COALESCE(disconnected_at, clock_timestamp()),
    heartbeat_at = GREATEST(heartbeat_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
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

-- The pre-067 schema cannot represent transport-neutral Agent Node mode.
-- Rollback chooses the former WebSocket listing value deterministically.
UPDATE agents
SET connection_mode = 'runtime_ws',
    endpoint_url = 'openlinker-runtime-ws://' || slug,
    updated_at = clock_timestamp()
WHERE connection_mode = 'agent_node';

UPDATE runs
SET connection_mode_snapshot = 'runtime_ws'
WHERE runtime_contract_id = 'openlinker.runtime.v2'
  AND connection_mode_snapshot = 'agent_node';

ALTER TABLE agents
    ADD CONSTRAINT agents_connection_mode_valid
        CHECK (connection_mode IN ('direct_http', 'mcp_server', 'runtime_pull', 'runtime_ws')),
    ADD CONSTRAINT agents_runtime_queue_endpoint
        CHECK (
            (connection_mode <> 'runtime_pull' OR endpoint_url LIKE 'openlinker-runtime-pull://%')
            AND
            (connection_mode <> 'runtime_ws' OR endpoint_url LIKE 'openlinker-runtime-ws://%')
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
        endpoint_url LIKE 'openlinker-runtime-pull://%' OR
        endpoint_url LIKE 'openlinker-runtime-ws://%'
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
                AND connection_mode_snapshot IN (
                    'direct_http', 'mcp_server', 'runtime_pull', 'runtime_ws'
                )
                AND (
                    connection_mode_snapshot NOT IN ('direct_http', 'mcp_server')
                    OR endpoint_idempotency_snapshot IS NOT NULL
                )
            )
        ),
    ADD CONSTRAINT runs_connection_mode_snapshot_valid
        CHECK (
            connection_mode_snapshot IS NULL
            OR connection_mode_snapshot IN (
                'direct_http', 'mcp_server', 'runtime_pull', 'runtime_ws'
            )
        );

-- Rollback restores the former schema shape, but removed board/claim/delivery
-- data and grants are intentionally not recoverable.
ALTER TABLE task_queries
    DROP CONSTRAINT task_queries_completion_consistency,
    ADD COLUMN claimed_agent_id UUID REFERENCES agents(id) ON DELETE SET NULL,
    ADD COLUMN claimed_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN claimed_at TIMESTAMPTZ,
    ADD COLUMN claim_run_id UUID REFERENCES runs(id) ON DELETE SET NULL,
    ADD COLUMN delivery_status TEXT NOT NULL DEFAULT 'pending',
    ADD COLUMN delivery_visibility TEXT NOT NULL DEFAULT 'private',
    ADD COLUMN delivery_artifact JSONB,
    ADD COLUMN accepted_at TIMESTAMPTZ,
    ADD COLUMN revision_requested_at TIMESTAMPTZ,
    ADD COLUMN revision_note TEXT,
    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private',
    ADD COLUMN public_summary TEXT,
    ADD COLUMN published_at TIMESTAMPTZ;

UPDATE task_queries
SET claimed_agent_id = chosen_agent_id,
    claimed_by_user_id = user_id,
    claimed_at = completed_at,
    claim_run_id = completion_run_id
WHERE completed_at IS NOT NULL;

ALTER TABLE task_queries
    ADD CONSTRAINT task_queries_claim_consistency CHECK (
        (claimed_agent_id IS NULL AND claimed_by_user_id IS NULL AND claimed_at IS NULL)
        OR
        (claimed_agent_id IS NOT NULL AND claimed_by_user_id IS NOT NULL AND claimed_at IS NOT NULL)
    ),
    ADD CONSTRAINT task_queries_delivery_status_valid CHECK (
        delivery_status IN ('pending', 'submitted', 'revision_requested', 'accepted', 'failed')
    ),
    ADD CONSTRAINT task_queries_delivery_visibility_valid CHECK (
        delivery_visibility IN ('private', 'shared', 'public_example')
    ),
    ADD CONSTRAINT task_queries_revision_note_len CHECK (
        revision_note IS NULL OR char_length(revision_note) <= 2000
    ),
    ADD CONSTRAINT task_queries_delivery_acceptance_consistency CHECK (
        accepted_at IS NULL OR delivery_status = 'accepted'
    ),
    ADD CONSTRAINT task_queries_visibility_valid
        CHECK (visibility IN ('private', 'public')),
    ADD CONSTRAINT task_queries_public_summary_len CHECK (
        public_summary IS NULL OR char_length(public_summary) BETWEEN 4 AND 240
    ),
    ADD CONSTRAINT task_queries_publication_consistency CHECK (
        (
            visibility = 'private'
            AND published_at IS NULL
        )
        OR (
            visibility = 'public'
            AND published_at IS NOT NULL
            AND public_summary IS NOT NULL
        )
    ),
    ADD CONSTRAINT task_queries_completion_consistency
        CHECK (completed_at IS NULL OR claimed_agent_id IS NOT NULL);

UPDATE task_queries
SET delivery_status = 'submitted'
WHERE completed_at IS NOT NULL;

CREATE INDEX idx_task_queries_claimed_by_user
    ON task_queries (claimed_by_user_id, claimed_at DESC)
    WHERE claimed_by_user_id IS NOT NULL;

CREATE INDEX idx_task_queries_claimed_agent
    ON task_queries (claimed_agent_id, claimed_at DESC)
    WHERE claimed_agent_id IS NOT NULL;

CREATE INDEX idx_task_queries_delivery_status
    ON task_queries (delivery_status, created_at DESC);

CREATE INDEX idx_task_queries_public_board
    ON task_queries (published_at DESC, created_at DESC)
    WHERE visibility = 'public';

-- Keep the contract row as immutable FK evidence for closed Sessions and
-- revoked Nodes created under schema 67. A later re-up reactivates this row.
UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 67
  AND migration_name = '067_runtime_v2_core_execution'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
  AND is_current;

UPDATE runtime_schema_contracts SET is_current = TRUE
WHERE schema_version = 66
  AND migration_name = '066_runtime_v2_deadline_reconciler'
  AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61'
  AND NOT is_current;

UPDATE runtime_nodes
SET runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
    updated_at = clock_timestamp()
WHERE status <> 'revoked'
  AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f';

SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE runtime_nodes
    ADD CONSTRAINT runtime_nodes_contract_current
        CHECK (
            protocol_version = 2
            AND runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (status IN ('active', 'draining')
                 AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61')
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
                 AND runtime_contract_digest = '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61')
                OR
                (status IN ('offline', 'revoked', 'closed')
                 AND runtime_contract_digest IN (
                     '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                     '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
                 ))
            )
        );

DO $migration$
DECLARE
    definition TEXT;
    old_fragment TEXT := $old$latest_attempt.executor_type NOT IN ('agent_node', 'core_http', 'core_mcp')$old$;
    new_fragment TEXT := $new$latest_attempt.executor_type IS DISTINCT FROM 'agent_node'$new$;
BEGIN
    definition := pg_get_functiondef('enforce_run_active_attempt_consistency()'::regprocedure);
    IF POSITION(old_fragment IN definition) = 0 THEN
        RAISE EXCEPTION 'migration 067 rollback active Attempt invariant source mismatch';
    END IF;
    EXECUTE replace(definition, old_fragment, new_fragment);

    definition := pg_get_functiondef('enforce_run_terminal_artifacts_consistency()'::regprocedure);
    IF POSITION(old_fragment IN definition) = 0 THEN
        RAISE EXCEPTION 'migration 067 rollback terminal artifact invariant source mismatch';
    END IF;
    EXECUTE replace(definition, old_fragment, new_fragment);
END
$migration$;

COMMIT;
