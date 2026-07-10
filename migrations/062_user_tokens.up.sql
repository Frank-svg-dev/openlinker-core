BEGIN;

-- Core owns User Token credentials from this migration onward.  The table may
-- already exist in hosted installations because older Cloud releases created
-- it as api_keys/user_tokens in the same database.
CREATE TABLE IF NOT EXISTS user_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Adopt every supported legacy shape without replacing credential rows.
ALTER TABLE user_tokens
    ADD COLUMN IF NOT EXISTS scopes TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'user_tokens'
          AND column_name = 'key_hash'
    ) AND NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'user_tokens'
          AND column_name = 'token_hash'
    ) THEN
        ALTER TABLE user_tokens RENAME COLUMN key_hash TO token_hash;
    END IF;
END
$$;

ALTER TABLE user_tokens
    DROP CONSTRAINT IF EXISTS api_keys_prefix_format,
    DROP CONSTRAINT IF EXISTS api_keys_name_len,
    DROP CONSTRAINT IF EXISTS user_tokens_prefix_format,
    DROP CONSTRAINT IF EXISTS user_tokens_name_len;

ALTER TABLE user_tokens
    ADD CONSTRAINT user_tokens_prefix_format
        CHECK (prefix ~ '^ol_user_[a-f0-9]+$'),
    ADD CONSTRAINT user_tokens_name_len
        CHECK (char_length(name) BETWEEN 1 AND 80);

DROP TRIGGER IF EXISTS user_tokens_set_updated_at ON user_tokens;
CREATE TRIGGER user_tokens_set_updated_at BEFORE UPDATE ON user_tokens
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE INDEX IF NOT EXISTS idx_user_tokens_user
    ON user_tokens (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_tokens_prefix_active
    ON user_tokens (prefix)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS user_token_core_grants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id UUID NOT NULL REFERENCES user_tokens(id) ON DELETE CASCADE,
    permission TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id UUID,
    constraints JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT user_token_core_grants_permission_nonempty
        CHECK (char_length(permission) BETWEEN 1 AND 80),
    CONSTRAINT user_token_core_grants_resource_type_nonempty
        CHECK (char_length(resource_type) BETWEEN 1 AND 40),
    CONSTRAINT user_token_core_grants_constraints_object
        CHECK (jsonb_typeof(constraints) = 'object')
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_token_core_grants_identity
    ON user_token_core_grants (
        token_id,
        permission,
        resource_type,
        COALESCE(resource_id, '00000000-0000-0000-0000-000000000000'::uuid)
    );
CREATE INDEX IF NOT EXISTS idx_user_token_core_grants_token
    ON user_token_core_grants (token_id, permission);

-- Legacy scopes become wildcard grants.  tasks:write intentionally maps only
-- to tasks:create and never expands into publish/run/work/review permissions.
INSERT INTO user_token_core_grants (token_id, permission, resource_type)
SELECT DISTINCT
    t.id,
    CASE scope
        WHEN 'tasks:write' THEN 'tasks:create'
        ELSE scope
    END,
    CASE
        WHEN scope LIKE 'agents:%' THEN 'agent'
        WHEN scope LIKE 'runs:%' THEN 'run'
        WHEN scope LIKE 'tasks:%' THEN 'task'
        WHEN scope LIKE 'workflows:%' THEN 'workflow'
        WHEN scope LIKE 'agent-tokens:%' THEN 'agent'
        ELSE 'core'
    END
FROM user_tokens AS t
CROSS JOIN LATERAL unnest(COALESCE(t.scopes, '{}'::text[])) AS scope
WHERE scope = ANY (ARRAY[
    'agents:read', 'agents:run', 'agents:create',
    'runs:read', 'runs:cancel',
    'tasks:read', 'tasks:write', 'tasks:create', 'tasks:run',
    'tasks:publish', 'tasks:work', 'tasks:review',
    'workflows:read', 'workflows:manage', 'workflows:run',
    'agent-tokens:read', 'agent-tokens:issue', 'agent-tokens:revoke'
])
ON CONFLICT DO NOTHING;

-- A persisted singleton gives Cloud an issuer identity that survives process
-- restarts and URL changes.
CREATE TABLE IF NOT EXISTS core_instance_identity (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    issuer_instance_id TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO core_instance_identity (singleton, issuer_instance_id)
VALUES (
    TRUE,
    'core_' || replace(gen_random_uuid()::text, '-', '')
)
ON CONFLICT (singleton) DO NOTHING;

COMMIT;
