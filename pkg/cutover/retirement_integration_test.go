package cutover

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRetireStaleMembersIntegration(t *testing.T) {
	databaseURL := os.Getenv("CUTOVER_RETIREMENT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CUTOVER_RETIREMENT_TEST_DATABASE_URL is not set to a disposable database")
	}
	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// The production command also owns a one-connection pool. This makes every
	// other backend visible to its strict pg_stat_activity exclusivity check.
	poolConfig.MaxConns = 1
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "cutover-retirement-test"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	target := testIdentity()
	target.SchemaVersion = 76
	target.MigrationName = "076_runtime_cancellation_terminal_reap"
	service := NewService(pool, ServiceConfig{Identity: target, LiveWindow: 15 * time.Second})
	reset := func(t *testing.T) {
		resetRetirementIntegrationSchema(t, pool, target)
	}
	t.Cleanup(func() { dropRetirementIntegrationSchema(t, pool) })

	t.Run("predecessor stale rows retire", func(t *testing.T) {
		reset(t)
		insertRetirementIntegrationMember(t, pool, target, time.Now().UTC().Add(-time.Minute))
		report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{})
		if retireErr != nil || !report.Changed || !report.Readiness.Ready ||
			report.Retirement == nil || report.Retirement.RetiredStaleMembers != 1 ||
			retirementIntegrationMemberCount(t, pool) != 0 {
			t.Fatalf("err=%v report=%#v", retireErr, report)
		}
	})

	t.Run("member committed while lock waits is observed as live", func(t *testing.T) {
		reset(t)
		holderConfig, parseErr := pgx.ParseConfig(databaseURL)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		holderConfig.RuntimeParams["application_name"] = "cutover-retirement-holder"
		holder, connectErr := pgx.ConnectConfig(ctx, holderConfig)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		defer func() { _ = holder.Close(ctx) }()
		holderTx, beginErr := holder.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = holderTx.Rollback(ctx) }()
		if _, err = holderTx.Exec(ctx, `LOCK TABLE runtime_cluster_members IN ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		type retireResult struct {
			report Report
			err    error
		}
		result := make(chan retireResult, 1)
		go func() {
			report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{})
			result <- retireResult{report: report, err: retireErr}
		}()

		observer, connectErr := pgx.Connect(ctx, databaseURL)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		defer func() { _ = observer.Close(ctx) }()
		waitForRetirementLockWaiter(t, observer, "cutover-retirement-test")
		_ = observer.Close(ctx)
		insertRetirementIntegrationMemberWithTx(t, holderTx, target, time.Now().UTC())
		if err = holderTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		_ = holder.Close(ctx)

		select {
		case got := <-result:
			if !IsBlocked(got.err) || got.report.Retirement == nil ||
				got.report.Retirement.LiveMembers != 1 ||
				!containsCode(got.report.Readiness.Blockers, BlockerClusterMembersRegistered) ||
				retirementIntegrationMemberCount(t, pool) != 1 {
				t.Fatalf("err=%v report=%#v", got.err, got.report)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("retirement did not finish after the holder committed")
		}
	})

	t.Run("database client evidence refreshes inside one transaction", func(t *testing.T) {
		reset(t)
		tx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		before, countErr := otherDatabaseClientBackends(ctx, tx)
		if countErr != nil {
			t.Fatal(countErr)
		}
		other, connectErr := pgx.Connect(ctx, databaseURL)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		defer func() { _ = other.Close(ctx) }()
		after, countErr := otherDatabaseClientBackends(ctx, tx)
		if countErr != nil {
			t.Fatal(countErr)
		}
		if before != 0 || after < 1 {
			t.Fatalf("database clients before=%d after=%d", before, after)
		}
	})

	t.Run("membership lock timeout is operational failure", func(t *testing.T) {
		reset(t)
		insertRetirementIntegrationMember(t, pool, target, time.Now().UTC().Add(-time.Minute))
		holder, connectErr := pgx.Connect(ctx, databaseURL)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		defer func() { _ = holder.Close(ctx) }()
		holderTx, beginErr := holder.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer func() { _ = holderTx.Rollback(ctx) }()
		if _, err = holderTx.Exec(ctx, `LOCK TABLE runtime_cluster_members IN ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, retireErr := service.RetireStaleMembers(timeoutCtx, RetirementOptions{})
		if ErrorCode(retireErr) != BlockerDatabaseUnavailable || IsBlocked(retireErr) ||
			retirementIntegrationMemberCount(t, pool) != 1 {
			t.Fatalf("err=%v", retireErr)
		}
	})

	t.Run("live member refuses without mutation", func(t *testing.T) {
		reset(t)
		insertRetirementIntegrationMember(t, pool, target, time.Now().UTC())
		report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{})
		if ErrorCode(retireErr) != BlockerClusterMembersRegistered || !IsBlocked(retireErr) ||
			report.Readiness.Ready || retirementIntegrationMemberCount(t, pool) != 1 {
			t.Fatalf("err=%v report=%#v", retireErr, report)
		}
	})

	t.Run("other database client refuses without mutation", func(t *testing.T) {
		reset(t)
		insertRetirementIntegrationMember(t, pool, target, time.Now().UTC().Add(-time.Minute))
		other, connectErr := pgx.Connect(ctx, databaseURL)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		defer func() { _ = other.Close(ctx) }()
		report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{})
		if ErrorCode(retireErr) != BlockerDatabaseClientsActive || !IsBlocked(retireErr) ||
			report.Database.OtherClientBackends < 1 || retirementIntegrationMemberCount(t, pool) != 1 {
			t.Fatalf("err=%v report=%#v", retireErr, report)
		}
	})

	t.Run("post-delete residue rolls back", func(t *testing.T) {
		reset(t)
		staleAt := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
		insertRetirementIntegrationMember(t, pool, target, staleAt)
		if _, err = pool.Exec(ctx, `
CREATE FUNCTION cutover_reinsert_member() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO runtime_cluster_members (
        instance_id, release_version, release_commit, schema_version,
        schema_checksum, runtime_contract_id, runtime_contract_digest,
        started_at, heartbeat_at, draining, ready
    ) VALUES (
        OLD.instance_id, OLD.release_version, OLD.release_commit, OLD.schema_version,
        OLD.schema_checksum, OLD.runtime_contract_id, OLD.runtime_contract_digest,
        OLD.started_at, clock_timestamp(), OLD.draining, OLD.ready
    );
    RETURN OLD;
END
$$`); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `
CREATE TRIGGER cutover_reinsert_member_after_delete
AFTER DELETE ON runtime_cluster_members
FOR EACH ROW EXECUTE FUNCTION cutover_reinsert_member()`); err != nil {
			t.Fatal(err)
		}
		report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{})
		var heartbeat time.Time
		if err = pool.QueryRow(ctx, `SELECT heartbeat_at FROM runtime_cluster_members`).Scan(&heartbeat); err != nil {
			t.Fatal(err)
		}
		if ErrorCode(retireErr) != BlockerClusterMembersRegistered || !IsBlocked(retireErr) ||
			report.Retirement == nil || report.Retirement.MembersAfter != 1 ||
			retirementIntegrationMemberCount(t, pool) != 1 || !heartbeat.Equal(staleAt) {
			t.Fatalf("err=%v heartbeat=%s report=%#v", retireErr, heartbeat, report)
		}
	})

	t.Run("empty table is idempotent", func(t *testing.T) {
		reset(t)
		report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{})
		if retireErr != nil || report.Changed || !report.Readiness.Ready || report.Retirement == nil ||
			report.Retirement.RetiredStaleMembers != 0 || report.Retirement.MembersAfter != 0 {
			t.Fatalf("err=%v report=%#v", retireErr, report)
		}
	})

	t.Run("pre-runtime schema requires explicit noop", func(t *testing.T) {
		dropRetirementIntegrationSchema(t, pool)
		report, retireErr := service.RetireStaleMembers(ctx, RetirementOptions{AllowRuntimeUninstalledNoop: true})
		if retireErr != nil || !report.RuntimeUninstalledNoop || report.SchemaInstalled ||
			!report.Readiness.Ready || report.Retirement == nil || report.Retirement.RetiredStaleMembers != 0 {
			t.Fatalf("explicit noop err=%v report=%#v", retireErr, report)
		}
		report, retireErr = service.RetireStaleMembers(ctx, RetirementOptions{})
		if ErrorCode(retireErr) != BlockerClusterSchemaUnavailable || !IsBlocked(retireErr) ||
			report.Readiness.Ready {
			t.Fatalf("missing flag err=%v report=%#v", retireErr, report)
		}
		if _, err = pool.Exec(ctx, `CREATE TABLE runtime_cluster_control (singleton_id smallint PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		_, retireErr = service.RetireStaleMembers(ctx, RetirementOptions{AllowRuntimeUninstalledNoop: true})
		if ErrorCode(retireErr) != BlockerDatabaseUnavailable || IsBlocked(retireErr) {
			t.Fatalf("partial schema err=%v", retireErr)
		}
	})

}

func resetRetirementIntegrationSchema(t *testing.T, pool *pgxpool.Pool, target Identity) {
	t.Helper()
	dropRetirementIntegrationSchema(t, pool)
	ctx := context.Background()
	statements := []string{
		`CREATE TABLE runtime_cluster_control (singleton_id smallint PRIMARY KEY)`,
		`CREATE TABLE runtime_schema_contracts (
            schema_version integer NOT NULL,
            migration_name text NOT NULL,
            runtime_contract_id text NOT NULL,
            runtime_contract_digest text NOT NULL,
            is_current boolean NOT NULL
        )`,
		`CREATE TABLE runtime_cluster_members (
            instance_id uuid PRIMARY KEY,
            release_version text NOT NULL,
            release_commit text NOT NULL,
            schema_version integer NOT NULL,
            schema_checksum text NOT NULL,
            runtime_contract_id text NOT NULL,
            runtime_contract_digest text NOT NULL,
            started_at timestamptz NOT NULL,
            heartbeat_at timestamptz NOT NULL,
            draining boolean NOT NULL,
            ready boolean NOT NULL
        )`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO runtime_schema_contracts (
    schema_version, migration_name, runtime_contract_id,
    runtime_contract_digest, is_current
) VALUES (75, '075_runtime_wire_compatibility', $1, $2, TRUE)
`, target.RuntimeContractID, target.RuntimeContractDigest); err != nil {
		t.Fatal(err)
	}
}

func dropRetirementIntegrationSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS runtime_cluster_members CASCADE`,
		`DROP TABLE IF EXISTS runtime_schema_contracts CASCADE`,
		`DROP TABLE IF EXISTS runtime_cluster_control CASCADE`,
		`DROP FUNCTION IF EXISTS cutover_reinsert_member() CASCADE`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func insertRetirementIntegrationMember(t *testing.T, pool *pgxpool.Pool, identity Identity, heartbeatAt time.Time) {
	t.Helper()
	insertRetirementIntegrationMemberWithQuerier(t, pool, identity, heartbeatAt)
}

func insertRetirementIntegrationMemberWithTx(t *testing.T, tx pgx.Tx, identity Identity, heartbeatAt time.Time) {
	t.Helper()
	insertRetirementIntegrationMemberWithQuerier(t, tx, identity, heartbeatAt)
}

type retirementIntegrationExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertRetirementIntegrationMemberWithQuerier(t *testing.T, execer retirementIntegrationExecer, identity Identity, heartbeatAt time.Time) {
	t.Helper()
	if _, err := execer.Exec(context.Background(), `
INSERT INTO runtime_cluster_members (
    instance_id, release_version, release_commit, schema_version,
    schema_checksum, runtime_contract_id, runtime_contract_digest,
    started_at, heartbeat_at, draining, ready
) VALUES ($1, 'previous-release', 'previous-commit', 75, $2, $3, $4, $5, $6, FALSE, TRUE)
`, uuid.New(), identity.SchemaChecksum, identity.RuntimeContractID,
		identity.RuntimeContractDigest, heartbeatAt.Add(-time.Minute), heartbeatAt); err != nil {
		t.Fatal(err)
	}
}

func waitForRetirementLockWaiter(t *testing.T, observer *pgx.Conn, applicationName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := observer.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_locks AS locks
    JOIN pg_stat_activity AS activity ON activity.pid = locks.pid
    WHERE activity.application_name = $1
      AND locks.relation = 'runtime_cluster_members'::regclass
      AND locks.mode = 'ShareRowExclusiveLock'
      AND NOT locks.granted
)
`, applicationName).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("retirement did not wait on the membership lock")
		case <-ticker.C:
		}
	}
}

func retirementIntegrationMemberCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*)::bigint FROM runtime_cluster_members`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
