package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	migratecmd "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestOAuthLoginCodeMigration078Contract(t *testing.T) {
	for _, name := range []string{
		"078_oauth_login_code_subject_only.up.sql",
		"078_oauth_login_code_subject_only.down.sql",
		"078_oauth_login_code_subject_only_verify.sql",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err, "read %s", name)
		sql := string(raw)
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			require.Contains(t, sql, "ALTER COLUMN jwt DROP NOT NULL")
			require.NotContains(t, sql, "DROP COLUMN jwt")
		case strings.HasSuffix(name, ".down.sql"):
			require.Contains(t, sql, "LOCK TABLE oauth_login_codes IN ACCESS EXCLUSIVE MODE")
			require.Contains(t, sql, "jwt IS NULL")
			require.Contains(t, sql, "consumed_at IS NULL")
			require.Contains(t, sql, "expires_at > NOW()")
			require.Contains(t, sql, "ALTER COLUMN jwt SET NOT NULL")
		case strings.HasSuffix(name, "_verify.sql"):
			require.Contains(t, sql, "oauth_login_codes")
			require.Contains(t, sql, "oauth_login_codes_jwt_nonempty")
		}
	}
}

func TestOAuthLoginCodeMigration078StepsDownFailClosed(t *testing.T) {
	dsn := os.Getenv("OAUTH_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OAUTH_MIGRATION_TEST_DATABASE_URL is not set to a disposable PostgreSQL database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, pool.Ping(ctx))

	_, err = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err, "reset disposable database")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	})

	_, err = pool.Exec(ctx, `
CREATE TABLE users (
    id UUID PRIMARY KEY,
    email TEXT NOT NULL,
    display_name TEXT NOT NULL,
    disabled_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
CREATE TABLE oauth_login_codes (
    code_hash TEXT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    jwt TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT oauth_login_codes_hash_len CHECK (char_length(code_hash) = 64),
    CONSTRAINT oauth_login_codes_jwt_nonempty CHECK (char_length(jwt) > 0)
);
CREATE INDEX idx_oauth_login_codes_expires_at ON oauth_login_codes (expires_at);
`)
	require.NoError(t, err, "create migration 077 predecessor schema")

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	require.NoError(t, err)
	m, err := migratecmd.New("file://"+migrationsDir, dsn)
	require.NoError(t, err)
	defer func() { _, _ = m.Close() }()
	require.NoError(t, m.Force(77))
	require.NoError(t, m.Steps(1), "apply migration 078")
	requireOAuthJWTNullability(t, pool, "YES")

	verifySQL, err := os.ReadFile(filepath.Join(migrationsDir, "078_oauth_login_code_subject_only_verify.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(verifySQL))
	require.NoError(t, err, "run migration 078 verification")

	userID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)`, userID, "migration-078@example.test", "Migration 078")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO oauth_login_codes (code_hash, user_id, jwt, expires_at)
VALUES ($1, $2, NULL, NOW() + INTERVAL '2 minutes')
`, strings.Repeat("a", 64), userID)
	require.NoError(t, err)

	err = m.Steps(-1)
	require.Error(t, err, "down must fail while an active unconsumed subject-only code exists")
	require.Contains(t, err.Error(), "active unconsumed subject-only OAuth code")
	// The down file uses an explicit transaction. After its fail-closed RAISE,
	// reconnect before inspecting the golang-migrate dirty marker; closing the
	// failed connection also rolls back the aborted PostgreSQL transaction.
	_, _ = m.Close()
	m, err = migratecmd.New("file://"+migrationsDir, dsn)
	require.NoError(t, err)
	version, dirty, versionErr := m.Version()
	require.NoError(t, versionErr)
	require.Equal(t, uint(77), version)
	require.True(t, dirty, "failed golang-migrate Steps(-1) must be recovered explicitly")
	requireOAuthJWTNullability(t, pool, "YES")

	require.NoError(t, m.Force(78), "clear dirty version only after verifying migration 078 schema remains")
	_, err = pool.Exec(ctx, `UPDATE oauth_login_codes SET expires_at = NOW() - INTERVAL '1 second'`)
	require.NoError(t, err)
	require.NoError(t, m.Steps(-1), "down should succeed after active subject-only codes expire")
	version, dirty, err = m.Version()
	require.NoError(t, err)
	require.Equal(t, uint(77), version)
	require.False(t, dirty)
	requireOAuthJWTNullability(t, pool, "NO")

	var nullRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM oauth_login_codes WHERE jwt IS NULL`).Scan(&nullRows))
	require.Zero(t, nullRows)
}

func requireOAuthJWTNullability(t *testing.T, pool *pgxpool.Pool, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got string
	err := pool.QueryRow(ctx, `
SELECT is_nullable
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name = 'oauth_login_codes'
  AND column_name = 'jwt'
`).Scan(&got)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timed out checking oauth_login_codes.jwt nullability")
	}
	require.NoError(t, err, fmt.Sprintf("read oauth_login_codes.jwt nullability; want %s", want))
	require.Equal(t, want, got)
}
