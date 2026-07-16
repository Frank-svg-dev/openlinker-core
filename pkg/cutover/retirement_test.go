package cutover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetireStaleMembersCommitsOnlyStaleRowsWithStructuredEvidence(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{
		retirementTestMember(now.Add(-16 * time.Second)),
		retirementTestMember(now.Add(-time.Minute)),
	})
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !tx.committed || tx.rolledBack || !report.Changed || !report.Readiness.Ready {
		t.Fatalf("transaction committed=%v rolled_back=%v report=%#v", tx.committed, tx.rolledBack, report)
	}
	if report.Retirement == nil {
		t.Fatal("retirement evidence is missing")
	}
	evidence := *report.Retirement
	if evidence.LiveWindowMilliseconds != 15_000 || evidence.MembersBefore != 2 ||
		evidence.LiveMembers != 0 || evidence.StaleMembers != 2 ||
		evidence.RetiredStaleMembers != 2 || evidence.MembersAfter != 0 {
		t.Fatalf("retirement evidence=%#v", evidence)
	}
	if len(report.Members) != 0 {
		t.Fatalf("successful report retained deleted members: %#v", report.Members)
	}
	if tx.deleteCutoff == nil || !tx.deleteCutoff.Equal(now.Add(-15*time.Second)) {
		t.Fatalf("delete cutoff=%v", tx.deleteCutoff)
	}
	wantOrder := []string{"schema", "lock", "clock", "stats", "clients", "members", "stats", "clients", "delete", "after", "stats", "clients", "commit"}
	if strings.Join(tx.events, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("events=%v want=%v", tx.events, wantOrder)
	}
}

func TestRetireStaleMembersRefusesLiveMemberWithoutDeleting(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{
		retirementTestMember(now.Add(-time.Minute)),
		retirementTestMember(now.Add(-15 * time.Second)),
		retirementTestMember(now.Add(time.Minute)),
	})
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	assertRetirementBlocked(t, tx, report, err, BlockerClusterMembersRegistered)
	if report.Retirement == nil || report.Retirement.LiveMembers != 2 || report.Retirement.StaleMembers != 1 {
		t.Fatalf("retirement evidence=%#v", report.Retirement)
	}
	if report.Retirement.MembersAfter != report.Retirement.MembersBefore {
		t.Fatalf("blocked operation changed member evidence: %#v", report.Retirement)
	}
	if tx.deleteCutoff != nil {
		t.Fatalf("live-member refusal executed delete with cutoff %s", *tx.deleteCutoff)
	}
}

func TestRetireStaleMembersRefusesOtherDatabaseClientWithoutDeleting(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{retirementTestMember(now.Add(-time.Minute))})
	tx.otherClients = 1
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	assertRetirementBlocked(t, tx, report, err, BlockerDatabaseClientsActive)
	if tx.deleteCutoff != nil {
		t.Fatalf("client refusal executed delete with cutoff %s", *tx.deleteCutoff)
	}
}

func TestRetireStaleMembersRechecksClientsBeforeDelete(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{retirementTestMember(now.Add(-time.Minute))})
	tx.clientCounts = []int64{0, 1}
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	assertRetirementBlocked(t, tx, report, err, BlockerDatabaseClientsActive)
	if tx.deleteCutoff != nil {
		t.Fatalf("pre-delete client refusal executed delete with cutoff %s", *tx.deleteCutoff)
	}
}

func TestRetireStaleMembersRechecksClientsBeforeCommitAndRollsBackDelete(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{retirementTestMember(now.Add(-time.Minute))})
	tx.clientCounts = []int64{0, 0, 1}
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	assertRetirementBlocked(t, tx, report, err, BlockerDatabaseClientsActive)
	if tx.deleteCutoff == nil || report.Retirement == nil || report.Retirement.RetiredStaleMembers != 0 {
		t.Fatalf("delete=%v evidence=%#v", tx.deleteCutoff, report.Retirement)
	}
}

func TestRetireStaleMembersRollsBackWhenTableIsNotEmptyAfterDelete(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{retirementTestMember(now.Add(-time.Minute))})
	tx.afterCountOverride = int64Pointer(1)
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	assertRetirementBlocked(t, tx, report, err, BlockerClusterMembersRegistered)
	if tx.deleteCutoff == nil || report.Retirement == nil || report.Retirement.MembersAfter != 1 {
		t.Fatalf("delete=%v evidence=%#v", tx.deleteCutoff, report.Retirement)
	}
}

func TestRetireStaleMembersIsIdempotentForEmptyTable(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, nil)
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	if err != nil || !tx.committed || tx.rolledBack || report.Changed || !report.Readiness.Ready {
		t.Fatalf("err=%v committed=%v rolled_back=%v report=%#v", err, tx.committed, tx.rolledBack, report)
	}
	if report.Retirement == nil || report.Retirement.MembersBefore != 0 ||
		report.Retirement.RetiredStaleMembers != 0 || report.Retirement.MembersAfter != 0 {
		t.Fatalf("retirement evidence=%#v", report.Retirement)
	}
	if tx.deleteCutoff != nil {
		t.Fatalf("empty-table operation executed delete with cutoff %s", *tx.deleteCutoff)
	}
}

func TestRetireStaleMembersRollsBackOnPostDeleteQueryFailure(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	tx := newRetirementFakeTx(now, []Member{retirementTestMember(now.Add(-time.Minute))})
	tx.afterErr = errors.New("forced count failure")
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	_, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
	if ErrorCode(err) != BlockerDatabaseUnavailable || IsBlocked(err) || tx.committed || !tx.rolledBack || tx.deleteCutoff == nil {
		t.Fatalf("err=%v committed=%v rolled_back=%v delete=%v", err, tx.committed, tx.rolledBack, tx.deleteCutoff)
	}
}

func TestRetireStaleMembersRuntimeUninstalledPolicy(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 5, 6, 0, time.UTC)
	service := NewService(nil, ServiceConfig{Identity: testIdentity(), LiveWindow: 15 * time.Second})

	t.Run("explicit noop", func(t *testing.T) {
		tx := newRetirementFakeTx(now, nil)
		tx.schemaTables = [3]bool{}
		report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{AllowRuntimeUninstalledNoop: true})
		if err != nil || !tx.committed || tx.rolledBack || !report.RuntimeUninstalledNoop ||
			report.SchemaInstalled || !report.Readiness.Ready || report.Retirement == nil ||
			report.Retirement.RetiredStaleMembers != 0 {
			t.Fatalf("err=%v committed=%v rolled_back=%v report=%#v", err, tx.committed, tx.rolledBack, report)
		}
	})

	t.Run("flag required", func(t *testing.T) {
		tx := newRetirementFakeTx(now, nil)
		tx.schemaTables = [3]bool{}
		report, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{})
		assertRetirementBlocked(t, tx, report, err, BlockerClusterSchemaUnavailable)
	})

	t.Run("partial schema is operational failure", func(t *testing.T) {
		tx := newRetirementFakeTx(now, nil)
		tx.schemaTables = [3]bool{true, false, false}
		_, err := service.retireStaleMembers(context.Background(), tx.begin, RetirementOptions{AllowRuntimeUninstalledNoop: true})
		if ErrorCode(err) != BlockerDatabaseUnavailable || IsBlocked(err) || tx.committed || !tx.rolledBack {
			t.Fatalf("err=%v committed=%v rolled_back=%v", err, tx.committed, tx.rolledBack)
		}
	})
}

func assertRetirementBlocked(t *testing.T, tx *retirementFakeTx, report Report, err error, code string) {
	t.Helper()
	if ErrorCode(err) != code || !IsBlocked(err) {
		t.Fatalf("err=%v code=%q", err, ErrorCode(err))
	}
	if tx.committed || !tx.rolledBack || report.Readiness.Ready || !containsCode(report.Readiness.Blockers, code) {
		t.Fatalf("committed=%v rolled_back=%v readiness=%#v", tx.committed, tx.rolledBack, report.Readiness)
	}
}

func retirementTestMember(lastSeen time.Time) Member {
	identity := testIdentity()
	return Member{
		InstanceID: uuid.New(), ReleaseID: identity.ReleaseID, GitSHA: identity.GitSHA,
		SchemaVersion: identity.SchemaVersion, SchemaChecksum: identity.SchemaChecksum,
		RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest,
		StartedAt: lastSeen.Add(-time.Minute), LastSeenAt: lastSeen, Ready: true,
	}
}

type retirementFakeTx struct {
	pgx.Tx
	now                time.Time
	members            []Member
	schemaTables       [3]bool
	otherClients       int64
	clientCounts       []int64
	clientRead         int
	afterCountOverride *int64
	afterErr           error
	deleteCutoff       *time.Time
	committed          bool
	rolledBack         bool
	events             []string
}

func newRetirementFakeTx(now time.Time, members []Member) *retirementFakeTx {
	return &retirementFakeTx{
		now: now, members: append([]Member(nil), members...),
		schemaTables: [3]bool{true, true, true},
	}
}

func (f *retirementFakeTx) begin(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	if opts.IsoLevel != pgx.ReadCommitted {
		return nil, fmt.Errorf("isolation=%q", opts.IsoLevel)
	}
	return f, nil
}

func (f *retirementFakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "pg_stat_clear_snapshot"):
		f.events = append(f.events, "stats")
		return pgconn.NewCommandTag("SELECT 1"), nil
	case strings.Contains(sql, "LOCK TABLE runtime_cluster_members"):
		f.events = append(f.events, "lock")
		if !strings.Contains(sql, "SHARE ROW EXCLUSIVE MODE") {
			return pgconn.CommandTag{}, errors.New("membership lock permits concurrent mutation")
		}
		return pgconn.NewCommandTag("LOCK TABLE"), nil
	case strings.Contains(sql, "DELETE FROM runtime_cluster_members"):
		f.events = append(f.events, "delete")
		cutoff, ok := args[0].(time.Time)
		if !ok || !strings.Contains(sql, "heartbeat_at < $1") {
			return pgconn.CommandTag{}, errors.New("delete was not bounded by stale cutoff")
		}
		f.deleteCutoff = &cutoff
		remaining := f.members[:0]
		var deleted int64
		for _, member := range f.members {
			if member.LastSeenAt.Before(cutoff) {
				deleted++
				continue
			}
			remaining = append(remaining, member)
		}
		f.members = remaining
		return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", deleted)), nil
	default:
		return pgconn.CommandTag{}, fmt.Errorf("unexpected exec: %s", sql)
	}
}

func (f *retirementFakeTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "to_regclass('public.runtime_cluster_control')"):
		f.events = append(f.events, "schema")
		return retirementFakeRow{values: []any{
			f.schemaTables[0], f.schemaTables[1], f.schemaTables[2],
		}}
	case strings.Contains(sql, "SELECT clock_timestamp()"):
		f.events = append(f.events, "clock")
		return retirementFakeRow{values: []any{f.now}}
	case strings.Contains(sql, "FROM pg_stat_activity"):
		f.events = append(f.events, "clients")
		count := f.otherClients
		if f.clientRead < len(f.clientCounts) {
			count = f.clientCounts[f.clientRead]
		}
		f.clientRead++
		return retirementFakeRow{values: []any{count}}
	case strings.Contains(sql, "COUNT(*)::bigint FROM runtime_cluster_members"):
		f.events = append(f.events, "after")
		if f.afterErr != nil {
			return retirementFakeRow{err: f.afterErr}
		}
		count := int64(len(f.members))
		if f.afterCountOverride != nil {
			count = *f.afterCountOverride
		}
		return retirementFakeRow{values: []any{count}}
	default:
		return retirementFakeRow{err: fmt.Errorf("unexpected query row: %s", sql)}
	}
}

func (f *retirementFakeTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM runtime_cluster_members") {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}
	f.events = append(f.events, "members")
	return &retirementFakeRows{members: append([]Member(nil), f.members...), index: -1}, nil
}

func (f *retirementFakeTx) Commit(context.Context) error {
	f.events = append(f.events, "commit")
	f.committed = true
	return nil
}

func (f *retirementFakeTx) Rollback(context.Context) error {
	if f.committed {
		return pgx.ErrTxClosed
	}
	f.rolledBack = true
	return nil
}

type retirementFakeRow struct {
	values []any
	err    error
}

func (r retirementFakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return retirementScan(dest, r.values)
}

type retirementFakeRows struct {
	members []Member
	index   int
	closed  bool
	err     error
}

func (r *retirementFakeRows) Close()                                       { r.closed = true }
func (r *retirementFakeRows) Err() error                                   { return r.err }
func (r *retirementFakeRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT") }
func (r *retirementFakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *retirementFakeRows) Values() ([]any, error)                       { return nil, errors.New("not implemented") }
func (r *retirementFakeRows) RawValues() [][]byte                          { return nil }
func (r *retirementFakeRows) Conn() *pgx.Conn                              { return nil }

func (r *retirementFakeRows) Next() bool {
	if r.closed {
		return false
	}
	r.index++
	if r.index >= len(r.members) {
		r.Close()
		return false
	}
	return true
}

func (r *retirementFakeRows) Scan(dest ...any) error {
	if r.index < 0 || r.index >= len(r.members) {
		return errors.New("scan outside row")
	}
	member := r.members[r.index]
	return retirementScan(dest, []any{
		member.InstanceID, member.ReleaseID, member.GitSHA,
		member.SchemaVersion, member.SchemaChecksum,
		member.RuntimeContractID, member.RuntimeContractDigest,
		member.StartedAt, member.LastSeenAt, member.Draining, member.Ready,
	})
}

func retirementScan(dest, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("scan destinations=%d values=%d", len(dest), len(values))
	}
	for i, value := range values {
		switch target := dest[i].(type) {
		case *bool:
			*target = value.(bool)
		case *int32:
			*target = value.(int32)
		case *int64:
			*target = value.(int64)
		case *string:
			*target = value.(string)
		case *time.Time:
			*target = value.(time.Time)
		case *uuid.UUID:
			*target = value.(uuid.UUID)
		default:
			return fmt.Errorf("unsupported scan destination %T", dest[i])
		}
	}
	return nil
}

func int64Pointer(value int64) *int64 { return &value }
