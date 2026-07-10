BEGIN;

-- Keep user_tokens and every credential row on rollback.  Older Cloud builds
-- can continue reading the compatibility scopes column.
DROP TABLE IF EXISTS user_token_core_grants;
DROP TABLE IF EXISTS core_instance_identity;

DROP TRIGGER IF EXISTS user_tokens_set_updated_at ON user_tokens;
ALTER TABLE IF EXISTS user_tokens
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS updated_at;

COMMIT;
