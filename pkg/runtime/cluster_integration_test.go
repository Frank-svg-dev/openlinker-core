package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

func TestRuntimeClusterPostgresMembershipAndMaintenanceGate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	identity := runtimeClusterTestIdentity()
	repository := &postgresRuntimeClusterRepository{pool: pool}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = repository.CloseMember(cleanupCtx, identity.InstanceID)
	}()
	if err = repository.UpsertMember(ctx, identity, false, true); err != nil {
		t.Fatalf("upsert member: %v", err)
	}
	snapshot, err := repository.Snapshot(ctx, RuntimeClusterMemberLiveWindow)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.DatabaseTime.IsZero() || snapshot.CurrentSchema.SchemaVersion != RuntimeSchemaVersion ||
		snapshot.CurrentSchema.RuntimeContractID != RuntimeContractID ||
		snapshot.CurrentSchema.RuntimeContractDigest != RuntimeContractDigest {
		t.Fatalf("schema/database clock evidence = %#v", snapshot)
	}
	found := false
	for _, member := range snapshot.LiveMembers {
		if member.InstanceID != identity.InstanceID {
			continue
		}
		found = true
		if !runtimeClusterIdentityEqual(member.RuntimeClusterIdentity, identity) ||
			member.HeartbeatAt.IsZero() || !member.Ready || member.Draining {
			t.Fatalf("member evidence = %#v", member)
		}
	}
	if !found {
		t.Fatalf("member %s not live in snapshot", identity.InstanceID)
	}

	for _, test := range []struct {
		mode      RuntimeClusterMode
		operation RuntimeClusterOperation
		allowed   bool
	}{
		{RuntimeClusterModeNormal, RuntimeClusterNewRun, true},
		{RuntimeClusterModeDraining, RuntimeClusterNewRun, false},
		{RuntimeClusterModeDraining, RuntimeClusterClaim, true},
		{RuntimeClusterModeHardMaintenance, RuntimeClusterNewSession, false},
	} {
		t.Run(string(test.mode)+"/"+string(test.operation), func(t *testing.T) {
			tx, beginErr := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
			if beginErr != nil {
				t.Fatalf("begin: %v", beginErr)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, updateErr := tx.Exec(ctx, `UPDATE runtime_cluster_control SET mode = $1 WHERE singleton_id = 1`, test.mode); updateErr != nil {
				t.Fatalf("set mode: %v", updateErr)
			}
			gateErr := RequireRuntimeClusterOperation(ctx, tx, test.operation)
			if test.allowed && gateErr != nil {
				t.Fatalf("gate error = %v", gateErr)
			}
			if !test.allowed {
				var httpErr *httpx.HTTPError
				if !errors.As(gateErr, &httpErr) || httpErr.Status != 503 {
					t.Fatalf("gate error = %#v", gateErr)
				}
			}
		})
	}

	if err = repository.CloseMember(ctx, identity.InstanceID); err != nil {
		t.Fatalf("close member: %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM runtime_cluster_members WHERE instance_id = $1`, identity.InstanceID).Scan(&count); err != nil {
		t.Fatalf("count closed member: %v", err)
	}
	if count != 0 {
		t.Fatalf("closed member count = %d", count)
	}
}

func TestRuntimeClusterPostgresRepositoryRejectsUnknownSchemaContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()
	identity := runtimeClusterTestIdentity()
	identity.InstanceID = uuid.New()
	identity.SchemaVersion = RuntimeSchemaVersion + 1000
	repository := &postgresRuntimeClusterRepository{pool: pool}
	if err = repository.UpsertMember(ctx, identity, false, false); err == nil {
		_ = repository.CloseMember(ctx, identity.InstanceID)
		t.Fatal("unknown schema contract should violate membership foreign key")
	}
}

func TestRuntimeClusterGateSerializesWithMaintenanceTransition(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()
	var originalMode RuntimeClusterMode
	if err = pool.QueryRow(ctx, `SELECT mode FROM runtime_cluster_control WHERE singleton_id = 1`).Scan(&originalMode); err != nil {
		t.Fatalf("read original mode: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE runtime_cluster_control SET mode = 'normal' WHERE singleton_id = 1`); err != nil {
		t.Fatalf("set normal mode: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `UPDATE runtime_cluster_control SET mode = $1 WHERE singleton_id = 1`, originalMode)
	}()

	runTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatalf("begin run transaction: %v", err)
	}
	if err = RequireRuntimeClusterOperation(ctx, runTx, RuntimeClusterNewRun); err != nil {
		_ = runTx.Rollback(ctx)
		t.Fatalf("acquire run gate: %v", err)
	}

	transitionDone := make(chan error, 1)
	go func() {
		_, updateErr := pool.Exec(ctx, `UPDATE runtime_cluster_control SET mode = 'hard_maintenance' WHERE singleton_id = 1`)
		transitionDone <- updateErr
	}()
	select {
	case updateErr := <-transitionDone:
		_ = runTx.Rollback(ctx)
		t.Fatalf("maintenance transition bypassed Run gate: %v", updateErr)
	case <-time.After(150 * time.Millisecond):
	}
	if err = runTx.Rollback(ctx); err != nil {
		t.Fatalf("release run gate: %v", err)
	}
	select {
	case err = <-transitionDone:
		if err != nil {
			t.Fatalf("maintenance transition after gate release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance transition remained blocked after gate release")
	}
}
