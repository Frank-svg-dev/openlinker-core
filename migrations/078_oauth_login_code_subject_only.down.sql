BEGIN;

-- Serialize the preflight, cleanup, and NOT NULL restoration with all OAuth
-- code writers so a new active subject-only row cannot race the guard.
LOCK TABLE oauth_login_codes IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM oauth_login_codes
        WHERE jwt IS NULL
          AND consumed_at IS NULL
          AND expires_at > NOW()
    ) THEN
        RAISE EXCEPTION 'migration 078 rollback refuses an active unconsumed subject-only OAuth code';
    END IF;
END
$$;

-- Once no active subject-only code exists, every remaining NULL row is either
-- consumed or expired and can no longer be exchanged.
DELETE FROM oauth_login_codes
WHERE jwt IS NULL;

ALTER TABLE oauth_login_codes
    ALTER COLUMN jwt SET NOT NULL;

COMMIT;
