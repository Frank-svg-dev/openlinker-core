DO $$
BEGIN
    IF (
        SELECT is_nullable
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'oauth_login_codes'
          AND column_name = 'jwt'
    ) <> 'YES' THEN
        RAISE EXCEPTION 'migration 078 requires oauth_login_codes.jwt to be nullable';
    END IF;

    IF (
        SELECT COUNT(*)
        FROM pg_constraint
        WHERE conrelid = 'oauth_login_codes'::regclass
          AND conname = 'oauth_login_codes_jwt_nonempty'
          AND pg_get_constraintdef(oid) LIKE '%char_length(jwt) > 0%'
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 078 must preserve the nonempty legacy JWT constraint';
    END IF;

    IF to_regclass('idx_oauth_login_codes_expires_at') IS NULL THEN
        RAISE EXCEPTION 'migration 078 must preserve the OAuth code expiry index';
    END IF;
END
$$;
