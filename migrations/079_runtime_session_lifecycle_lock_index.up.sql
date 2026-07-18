CREATE INDEX idx_runtime_sessions_credential_lifecycle
    ON runtime_sessions (credential_id, runtime_session_id)
    WHERE status IN ('active', 'draining', 'offline');
