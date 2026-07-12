package cutover

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLegacyBlockersAreStableAndPayloadFree(t *testing.T) {
	blockers := legacyBlockers(LegacyEvidence{
		NonterminalRuns: 2, PendingRunDeliveries: 1,
		InvalidTerminalRuns: 3, InvalidRunEventPayloads: 4,
	})
	want := []string{
		BlockerLegacyRunsNonterminal,
		BlockerLegacyDeliveriesPending,
		BlockerLegacyTerminalHistoryInvalid,
		BlockerLegacyEventPayloadInvalid,
	}
	if len(blockers) != len(want) {
		t.Fatalf("blockers=%#v", blockers)
	}
	for i := range want {
		if blockers[i].Code != want[i] || blockers[i].Scope == "" || blockers[i].ID != "" {
			t.Fatalf("blocker[%d]=%#v", i, blockers[i])
		}
	}
}

func TestRuntimeReadinessRequiresExactReleaseContractAndReplicaSet(t *testing.T) {
	identity := testIdentity()
	service := NewService(nil, ServiceConfig{Identity: identity, SignalMode: "redis", SignalHealth: healthySignal{}})
	now := time.Now().UTC()
	report := Report{
		SchemaInstalled: true,
		Control:         &Control{Mode: "hard_maintenance", ExpectedReplicas: 1, CutoverID: uuid.New(), Version: 2},
		Current: Current{
			SchemaVersion: identity.SchemaVersion, MigrationName: identity.MigrationName,
			RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest,
		},
		Members: []Member{{
			InstanceID: uuid.New(), ReleaseID: identity.ReleaseID, GitSHA: identity.GitSHA,
			SchemaVersion: identity.SchemaVersion, SchemaChecksum: identity.SchemaChecksum,
			RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest,
			LastSeenAt: now, Live: true, Ready: false,
		}},
		SignalBus: SignalBus{Mode: "redis", Healthy: true},
	}
	got := service.evaluateRuntimeReadiness(report, true)
	if !got.Ready || len(got.Blockers) != 0 {
		t.Fatalf("reopen readiness=%#v", got)
	}
	report.Members[0].GitSHA = "different"
	report.Members[0].RuntimeContractDigest = "different"
	got = service.evaluateRuntimeReadiness(report, true)
	if got.Ready || !containsCode(got.Blockers, BlockerMemberReleaseMismatch) || !containsCode(got.Blockers, BlockerMemberContractMismatch) {
		t.Fatalf("mismatch readiness=%#v", got)
	}
}

func TestOperationalReadinessKeepsHardMaintenanceVisible(t *testing.T) {
	identity := testIdentity()
	service := NewService(nil, ServiceConfig{Identity: identity, SignalMode: "local"})
	report := Report{
		SchemaInstalled: true,
		Control:         &Control{Mode: "hard_maintenance", ExpectedReplicas: 1, CutoverID: uuid.New(), Version: 2},
		Current:         Current{SchemaVersion: identity.SchemaVersion, MigrationName: identity.MigrationName, RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest},
		Members: []Member{{
			InstanceID: uuid.New(), ReleaseID: identity.ReleaseID, GitSHA: identity.GitSHA,
			SchemaVersion: identity.SchemaVersion, SchemaChecksum: identity.SchemaChecksum,
			RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest,
			Live: true,
		}},
		SignalBus: SignalBus{Mode: "local", Healthy: true},
	}
	operational := service.evaluateRuntimeReadiness(report, false)
	reopen := service.evaluateRuntimeReadiness(report, true)
	if operational.Ready || !containsCode(operational.Blockers, BlockerMaintenance) {
		t.Fatalf("operational readiness=%#v", operational)
	}
	if !reopen.Ready {
		t.Fatalf("reopen readiness=%#v", reopen)
	}
}

func TestRuntimeReadinessRequiresRedisForHAAndReadyBitOnlyAfterReopen(t *testing.T) {
	identity := testIdentity()
	service := NewService(nil, ServiceConfig{Identity: identity, SignalMode: "local"})
	report := Report{
		SchemaInstalled: true,
		Control:         &Control{Mode: "normal", ExpectedReplicas: 2, CutoverID: uuid.New(), Version: 3},
		Current:         Current{SchemaVersion: identity.SchemaVersion, MigrationName: identity.MigrationName, RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest},
		SignalBus:       SignalBus{Mode: "local", Healthy: true},
	}
	for i := 0; i < 2; i++ {
		report.Members = append(report.Members, Member{
			InstanceID: uuid.New(), ReleaseID: identity.ReleaseID, GitSHA: identity.GitSHA,
			SchemaVersion: identity.SchemaVersion, SchemaChecksum: identity.SchemaChecksum,
			RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest,
			Live: true, Ready: i == 0,
		})
	}
	got := service.evaluateRuntimeReadiness(report, false)
	if got.Ready || !containsCode(got.Blockers, BlockerSignalDependencyUnavailable) || !containsCode(got.Blockers, BlockerMemberNotReady) {
		t.Fatalf("readiness=%#v", got)
	}
}

func TestRuntimeReadinessRejectsAnUnplannedExtraLiveReplica(t *testing.T) {
	identity := testIdentity()
	service := NewService(nil, ServiceConfig{Identity: identity, SignalMode: "local"})
	report := Report{
		SchemaInstalled: true,
		Control:         &Control{Mode: "hard_maintenance", ExpectedReplicas: 1, CutoverID: uuid.New(), Version: 4},
		Current:         Current{SchemaVersion: identity.SchemaVersion, MigrationName: identity.MigrationName, RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest},
		SignalBus:       SignalBus{Mode: "local", Healthy: true},
	}
	for i := 0; i < 2; i++ {
		report.Members = append(report.Members, Member{
			InstanceID: uuid.New(), ReleaseID: identity.ReleaseID, GitSHA: identity.GitSHA,
			SchemaVersion: identity.SchemaVersion, SchemaChecksum: identity.SchemaChecksum,
			RuntimeContractID: identity.RuntimeContractID, RuntimeContractDigest: identity.RuntimeContractDigest,
			Live: true,
		})
	}
	got := service.evaluateRuntimeReadiness(report, true)
	if got.Ready || !containsCode(got.Blockers, BlockerReplicasUnexpected) {
		t.Fatalf("extra replica readiness=%#v", got)
	}
}

func TestTransitionMatrixIsBreakingAndExplicit(t *testing.T) {
	tests := []struct {
		current string
		target  string
		want    bool
	}{
		{"normal", "draining", true},
		{"draining", "hard_maintenance", true},
		{"normal", "hard_maintenance", true},
		{"hard_maintenance", "hard_maintenance", true},
		{"hard_maintenance", "normal", true},
		{"draining", "normal", false},
		{"normal", "normal", false},
		{"legacy", "draining", false},
	}
	for _, tt := range tests {
		if got := transitionAllowed(tt.current, tt.target); got != tt.want {
			t.Fatalf("transition %s -> %s = %v, want %v", tt.current, tt.target, got, tt.want)
		}
	}
}

func TestIdentityRequiresCanonicalSHA256Evidence(t *testing.T) {
	identity := testIdentity()
	if !validIdentity(identity) {
		t.Fatal("test identity should be valid")
	}
	identity.SchemaChecksum = "A" + identity.SchemaChecksum[1:]
	if validIdentity(identity) {
		t.Fatal("uppercase/non-canonical checksum was accepted")
	}
	identity = testIdentity()
	identity.RuntimeContractDigest = identity.RuntimeContractDigest[:63] + "z"
	if validIdentity(identity) {
		t.Fatal("non-hex contract digest was accepted")
	}
}

func testIdentity() Identity {
	return Identity{
		ReleaseID: "20260712-abcdef0", GitSHA: "abcdef0123456789",
		SchemaVersion:         66,
		SchemaChecksum:        "a8b1c6b088771ad7f3604edac6820c4ab5aa5b2daaa6d63c1620890d3930c76d",
		MigrationName:         "066_runtime_v2_deadline_reconciler",
		RuntimeContractID:     "openlinker.runtime.v2",
		RuntimeContractDigest: "60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61",
	}
}

type healthySignal struct{}

func (healthySignal) Health(context.Context) error { return nil }

func containsCode(blockers []Blocker, code string) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}
