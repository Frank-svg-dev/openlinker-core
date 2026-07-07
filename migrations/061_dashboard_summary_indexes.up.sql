CREATE INDEX IF NOT EXISTS idx_runs_agent_success_time
    ON runs (agent_id, started_at DESC)
    WHERE status = 'success';

CREATE INDEX IF NOT EXISTS idx_runs_user_success_time
    ON runs (user_id, started_at DESC)
    WHERE status = 'success';
