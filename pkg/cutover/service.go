package cutover

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultLiveWindow = 15 * time.Second

type SignalHealth interface {
	Health(context.Context) error
}

type ServiceConfig struct {
	Identity     Identity
	SignalMode   string
	SignalHealth SignalHealth
	LiveWindow   time.Duration
}

type Service struct {
	pool         *pgxpool.Pool
	identity     Identity
	signalMode   string
	signalHealth SignalHealth
	liveWindow   time.Duration
}

func NewService(pool *pgxpool.Pool, cfg ServiceConfig) *Service {
	mode := strings.TrimSpace(cfg.SignalMode)
	if mode == "" {
		mode = SignalModeLocal
	}
	liveWindow := cfg.LiveWindow
	if liveWindow <= 0 {
		liveWindow = defaultLiveWindow
	}
	return &Service{
		pool: pool, identity: normalizeIdentity(cfg.Identity), signalMode: mode,
		signalHealth: cfg.SignalHealth, liveWindow: liveWindow,
	}
}

func normalizeIdentity(identity Identity) Identity {
	identity.ReleaseID = strings.TrimSpace(identity.ReleaseID)
	identity.GitSHA = strings.TrimSpace(identity.GitSHA)
	identity.SchemaChecksum = strings.TrimSpace(identity.SchemaChecksum)
	identity.MigrationName = strings.TrimSpace(identity.MigrationName)
	identity.RuntimeContractID = strings.TrimSpace(identity.RuntimeContractID)
	identity.RuntimeContractDigest = strings.TrimSpace(identity.RuntimeContractDigest)
	return identity
}

func (s *Service) Status(ctx context.Context) (Report, error) {
	report, err := s.collect(ctx, false)
	if err != nil {
		return Report{}, err
	}
	report.Readiness = s.evaluateRuntimeReadiness(report, false)
	reopen := s.evaluateRuntimeReadiness(report, true)
	report.ReopenReadiness = &reopen
	return report, nil
}

func (s *Service) Preflight(ctx context.Context, opts PreflightOptions) (Report, error) {
	report, err := s.collect(ctx, true)
	if err != nil {
		return Report{}, err
	}
	blockers := legacyBlockers(*report.Legacy)
	if opts.RequireExclusive && report.Database.OtherClientBackends > 0 {
		blockers = append(blockers, blocker(BlockerDatabaseClientsActive, "database", "other application database clients are still connected"))
	}
	if opts.RequireNoMembers && len(report.Members) > 0 {
		blockers = append(blockers, blocker(BlockerClusterMembersRegistered, "cluster", "runtime cluster membership rows must be empty before migration"))
	}
	report.Readiness = readiness(blockers)
	return report, nil
}

func (s *Service) collect(ctx context.Context, includeMigrationEvidence bool) (Report, error) {
	if s == nil || s.pool == nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	report := Report{
		Current: Current{
			SchemaVersion: s.identity.SchemaVersion, SchemaChecksum: s.identity.SchemaChecksum,
			MigrationName: s.identity.MigrationName, RuntimeContractID: s.identity.RuntimeContractID,
			RuntimeContractDigest: s.identity.RuntimeContractDigest,
			ReleaseID:             s.identity.ReleaseID, GitSHA: s.identity.GitSHA,
		},
		Members:   []Member{},
		SignalBus: SignalBus{Mode: s.signalMode},
	}
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&report.DatabaseTime); err != nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	if err := s.collectDatabaseEvidence(ctx, &report); err != nil {
		return Report{}, err
	}
	if includeMigrationEvidence {
		if err := s.collectLegacyEvidence(ctx, &report); err != nil {
			return Report{}, err
		}
	}
	if err := s.collectCluster(ctx, &report); err != nil {
		return Report{}, err
	}
	report.SignalBus.Healthy = s.signalHealthy(ctx)
	return report, nil
}

func (s *Service) collectDatabaseEvidence(ctx context.Context, report *Report) error {
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*)::bigint
FROM pg_stat_activity
WHERE datid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND pid <> pg_backend_pid()
  AND backend_type = 'client backend'
`).Scan(&report.Database.OtherClientBackends); err != nil {
		return &OperationError{Code: BlockerDatabaseUnavailable}
	}
	return nil
}

func (s *Service) collectLegacyEvidence(ctx context.Context, report *Report) error {
	evidence := &LegacyEvidence{}
	report.Legacy = evidence
	tables := []string{"runs", "run_events", "webhook_deliveries", "run_deliveries", "task_callback_deliveries"}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		var tablePresent bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&tablePresent); err != nil {
			return &OperationError{Code: BlockerDatabaseUnavailable}
		}
		present[table] = tablePresent
	}
	evidence.RunsTablePresent = present["runs"]
	if present["runs"] {
		if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM runs WHERE status = 'running'`).Scan(&evidence.NonterminalRuns); err != nil {
			return &OperationError{Code: BlockerDatabaseUnavailable}
		}
		// Columns used here all predate migration 063. Keeping this check in the
		// pre-migration CLI makes migration failures deterministic and redacted.
		if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*)::bigint
FROM runs
WHERE status <> 'running'
  AND (finished_at IS NULL OR COALESCE(duration_ms, 0) < 0
       OR platform_fee_cents < 0 OR creator_revenue_cents < 0)
`).Scan(&evidence.InvalidTerminalRuns); err != nil {
			return &OperationError{Code: BlockerDatabaseUnavailable}
		}
	}
	if present["run_events"] {
		if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM run_events WHERE jsonb_typeof(payload) <> 'object'`).Scan(&evidence.InvalidRunEventPayloads); err != nil {
			return &OperationError{Code: BlockerDatabaseUnavailable}
		}
	}
	deliveryCounts := []struct {
		present bool
		query   string
		target  *int64
	}{
		{present["webhook_deliveries"], `SELECT COUNT(*)::bigint FROM webhook_deliveries WHERE status = 'pending'`, &evidence.PendingWebhookDeliveries},
		{present["run_deliveries"], `SELECT COUNT(*)::bigint FROM run_deliveries WHERE status = 'pending'`, &evidence.PendingRunDeliveries},
		{present["task_callback_deliveries"], `SELECT COUNT(*)::bigint FROM task_callback_deliveries WHERE status = 'pending'`, &evidence.PendingTaskCallbackDeliveries},
	}
	for _, item := range deliveryCounts {
		if item.present {
			if err := s.pool.QueryRow(ctx, item.query).Scan(item.target); err != nil {
				return &OperationError{Code: BlockerDatabaseUnavailable}
			}
		}
	}
	return nil
}

func (s *Service) collectCluster(ctx context.Context, report *Report) error {
	var controlPresent, contractsPresent, membersPresent bool
	if err := s.pool.QueryRow(ctx, `
SELECT to_regclass('public.runtime_cluster_control') IS NOT NULL,
       to_regclass('public.runtime_schema_contracts') IS NOT NULL,
       to_regclass('public.runtime_cluster_members') IS NOT NULL
`).Scan(&controlPresent, &contractsPresent, &membersPresent); err != nil {
		return &OperationError{Code: BlockerDatabaseUnavailable}
	}
	report.SchemaInstalled = controlPresent && contractsPresent && membersPresent
	if !report.SchemaInstalled {
		return nil
	}
	control := &Control{}
	if err := s.pool.QueryRow(ctx, `
SELECT mode, expected_replicas, cutover_id, drain_started_at,
       drain_deadline_at, hard_maintenance_at, reopened_at, version, updated_at
FROM runtime_cluster_control WHERE singleton_id = 1
`).Scan(
		&control.Mode, &control.ExpectedReplicas, &control.CutoverID,
		&control.DrainStartedAt, &control.DrainDeadlineAt,
		&control.HardMaintenanceAt, &control.ReopenedAt,
		&control.Version, &control.UpdatedAt,
	); err != nil {
		return &OperationError{Code: BlockerClusterControlUnavailable}
	}
	report.Control = control
	var current Current
	if err := s.pool.QueryRow(ctx, `
SELECT schema_version, migration_name, runtime_contract_id, runtime_contract_digest
FROM runtime_schema_contracts WHERE is_current
`).Scan(
		&current.SchemaVersion, &current.MigrationName,
		&current.RuntimeContractID, &current.RuntimeContractDigest,
	); err != nil {
		return &OperationError{Code: BlockerClusterControlUnavailable}
	}
	current.SchemaChecksum = s.identity.SchemaChecksum
	current.ReleaseID = s.identity.ReleaseID
	current.GitSHA = s.identity.GitSHA
	report.Current = current
	rows, err := s.pool.Query(ctx, `
SELECT instance_id, release_version, release_commit, schema_version,
       schema_checksum, runtime_contract_id, runtime_contract_digest,
       started_at, heartbeat_at, draining, ready,
       heartbeat_at >= clock_timestamp() - ($1::bigint * interval '1 millisecond')
FROM runtime_cluster_members
ORDER BY started_at, instance_id
`, s.liveWindow.Milliseconds())
	if err != nil {
		return &OperationError{Code: BlockerClusterControlUnavailable}
	}
	defer rows.Close()
	for rows.Next() {
		var member Member
		if err = rows.Scan(
			&member.InstanceID, &member.ReleaseID, &member.GitSHA,
			&member.SchemaVersion, &member.SchemaChecksum,
			&member.RuntimeContractID, &member.RuntimeContractDigest,
			&member.StartedAt, &member.LastSeenAt, &member.Draining,
			&member.Ready, &member.Live,
		); err != nil {
			return &OperationError{Code: BlockerClusterControlUnavailable}
		}
		report.Members = append(report.Members, member)
	}
	if err = rows.Err(); err != nil {
		return &OperationError{Code: BlockerClusterControlUnavailable}
	}
	return nil
}

func (s *Service) signalHealthy(ctx context.Context) bool {
	if s.signalMode != SignalModeRedis {
		return true
	}
	if s.signalHealth == nil {
		return false
	}
	healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.signalHealth.Health(healthCtx) == nil
}

func legacyBlockers(evidence LegacyEvidence) []Blocker {
	var blockers []Blocker
	if evidence.NonterminalRuns > 0 {
		blockers = append(blockers, blocker(BlockerLegacyRunsNonterminal, "runs", "nonterminal runs must settle before migration"))
	}
	if evidence.PendingDeliveries() > 0 {
		blockers = append(blockers, blocker(BlockerLegacyDeliveriesPending, "deliveries", "pending legacy deliveries must settle before migration"))
	}
	if evidence.InvalidTerminalRuns > 0 {
		blockers = append(blockers, blocker(BlockerLegacyTerminalHistoryInvalid, "runs", "terminal run history is not migration-safe"))
	}
	if evidence.InvalidRunEventPayloads > 0 {
		blockers = append(blockers, blocker(BlockerLegacyEventPayloadInvalid, "run_events", "historical run events are not migration-safe"))
	}
	return blockers
}

func (s *Service) evaluateRuntimeReadiness(report Report, reopening bool) Readiness {
	var blockers []Blocker
	if !report.SchemaInstalled || report.Control == nil {
		return readiness([]Blocker{blocker(BlockerClusterSchemaUnavailable, "cluster", "Runtime schema is not installed")})
	}
	if !validIdentity(s.identity) {
		blockers = append(blockers, blocker(BlockerReleaseIdentityMissing, "release", "release identity is incomplete"))
	}
	if reopening && report.Control.Mode != "hard_maintenance" {
		blockers = append(blockers, blocker(BlockerTransitionNotAllowed, "cluster", "reopen requires hard maintenance"))
	}
	if !reopening && report.Control.Mode != "normal" {
		blockers = append(blockers, blocker(BlockerMaintenance, "cluster", "runtime cluster is in maintenance"))
	}
	if report.Current.SchemaVersion != s.identity.SchemaVersion || !textEqual(report.Current.MigrationName, s.identity.MigrationName) {
		blockers = append(blockers, blocker(BlockerCurrentSchemaMismatch, "schema", "database schema does not match this release"))
	}
	if !textEqual(report.Current.RuntimeContractID, s.identity.RuntimeContractID) || !textEqual(report.Current.RuntimeContractDigest, s.identity.RuntimeContractDigest) {
		blockers = append(blockers, blocker(BlockerCurrentContractMismatch, "contract", "database runtime contract does not match this release"))
	}
	if report.Control.ExpectedReplicas < 1 || report.Control.ExpectedReplicas > 1024 {
		blockers = append(blockers, blocker(BlockerExpectedReplicasInvalid, "cluster", "expected replica count is invalid"))
	}
	live := make([]Member, 0, len(report.Members))
	for _, member := range report.Members {
		if member.Live {
			live = append(live, member)
		}
	}
	if int32(len(live)) < report.Control.ExpectedReplicas {
		blockers = append(blockers, blocker(BlockerReplicasUnavailable, "cluster", "not all expected Core replicas are live"))
	} else if int32(len(live)) > report.Control.ExpectedReplicas {
		blockers = append(blockers, blocker(BlockerReplicasUnexpected, "cluster", "more Core replicas are live than declared"))
	}
	for _, member := range live {
		id := member.InstanceID.String()
		if !textEqual(member.ReleaseID, s.identity.ReleaseID) || !textEqual(member.GitSHA, s.identity.GitSHA) {
			blockers = append(blockers, memberBlocker(BlockerMemberReleaseMismatch, id, "Core member release does not match"))
		}
		if member.SchemaVersion != s.identity.SchemaVersion {
			blockers = append(blockers, memberBlocker(BlockerMemberSchemaMismatch, id, "Core member schema version does not match"))
		}
		if !textEqual(member.SchemaChecksum, s.identity.SchemaChecksum) {
			blockers = append(blockers, memberBlocker(BlockerMemberSchemaChecksumMismatch, id, "Core member schema checksum does not match"))
		}
		if !textEqual(member.RuntimeContractID, s.identity.RuntimeContractID) || !textEqual(member.RuntimeContractDigest, s.identity.RuntimeContractDigest) {
			blockers = append(blockers, memberBlocker(BlockerMemberContractMismatch, id, "Core member runtime contract does not match"))
		}
		// During hard maintenance the normal readiness bit is intentionally
		// false. Reopen validates identity, liveness, and dependencies first;
		// /readyz validates the ready bit after the mode changes to normal.
		if !reopening && report.Control.Mode == "normal" && !member.Ready {
			blockers = append(blockers, memberBlocker(BlockerMemberNotReady, id, "Core member is not ready"))
		}
	}
	if report.Control.ExpectedReplicas > 1 && report.SignalBus.Mode != SignalModeRedis {
		blockers = append(blockers, blocker(BlockerSignalDependencyUnavailable, "signal_bus", "HA replicas require the Redis signal bus"))
	} else if report.SignalBus.Mode == SignalModeRedis && !report.SignalBus.Healthy {
		blockers = append(blockers, blocker(BlockerSignalDependencyUnavailable, "signal_bus", "Redis signal bus is unavailable"))
	}
	return readiness(uniqueBlockers(blockers))
}

func validIdentity(identity Identity) bool {
	return identity.ReleaseID != "" && identity.ReleaseID != "local" &&
		identity.GitSHA != "" && identity.GitSHA != "unknown" &&
		identity.SchemaVersion > 0 && validSHA256Hex(identity.SchemaChecksum) &&
		identity.MigrationName != "" && identity.RuntimeContractID != "" &&
		validSHA256Hex(identity.RuntimeContractDigest)
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func readiness(blockers []Blocker) Readiness {
	if blockers == nil {
		blockers = []Blocker{}
	}
	return Readiness{Ready: len(blockers) == 0, Blockers: blockers}
}

func blocker(code, scope, message string) Blocker {
	return Blocker{Code: code, Scope: scope, MessageRedacted: message}
}

func memberBlocker(code, id, message string) Blocker {
	return Blocker{Code: code, Scope: "member", ID: id, MessageRedacted: message}
}

func uniqueBlockers(values []Blocker) []Blocker {
	seen := make(map[string]struct{}, len(values))
	out := make([]Blocker, 0, len(values))
	for _, value := range values {
		key := value.Code + "\x00" + value.Scope + "\x00" + value.ID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func textEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (s *Service) Drain(ctx context.Context, req TransitionRequest) (Report, error) {
	return s.transition(ctx, "draining", req)
}

func (s *Service) HardMaintenance(ctx context.Context, req TransitionRequest) (Report, error) {
	return s.transition(ctx, "hard_maintenance", req)
}

func (s *Service) Reopen(ctx context.Context, req TransitionRequest) (Report, error) {
	return s.transition(ctx, "normal", req)
}

func (s *Service) transition(ctx context.Context, target string, req TransitionRequest) (Report, error) {
	if s == nil || s.pool == nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var installed bool
	if err = tx.QueryRow(ctx, `
SELECT to_regclass('public.runtime_cluster_control') IS NOT NULL
   AND to_regclass('public.runtime_schema_contracts') IS NOT NULL
   AND to_regclass('public.runtime_cluster_members') IS NOT NULL
`).Scan(&installed); err != nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	if !installed {
		if req.AllowRuntimeUninstalledNoop && target != "normal" {
			if err = tx.Commit(ctx); err != nil {
				return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
			}
			report, statusErr := s.Preflight(ctx, PreflightOptions{})
			report.RuntimeUninstalledNoop = true
			return report, statusErr
		}
		return Report{}, &OperationError{Code: BlockerClusterSchemaUnavailable}
	}
	if req.ExpectedVersion < 1 {
		return Report{}, &OperationError{Code: BlockerClusterVersionConflict}
	}
	if target == "draining" && (req.ExpectedReplicas < 1 || req.ExpectedReplicas > 1024) {
		return Report{}, &OperationError{Code: BlockerExpectedReplicasInvalid}
	}
	var control Control
	if err = tx.QueryRow(ctx, `
SELECT mode, expected_replicas, cutover_id, drain_started_at,
       drain_deadline_at, hard_maintenance_at, reopened_at, version, updated_at
FROM runtime_cluster_control WHERE singleton_id = 1 FOR UPDATE
`).Scan(
		&control.Mode, &control.ExpectedReplicas, &control.CutoverID,
		&control.DrainStartedAt, &control.DrainDeadlineAt,
		&control.HardMaintenanceAt, &control.ReopenedAt,
		&control.Version, &control.UpdatedAt,
	); err != nil {
		return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
	}
	if control.Version != req.ExpectedVersion {
		return Report{}, &OperationError{Code: BlockerClusterVersionConflict}
	}
	if target == "normal" && (req.CutoverID == uuid.Nil || req.CutoverID != control.CutoverID) {
		return Report{}, &OperationError{Code: BlockerCutoverIDMismatch}
	}
	if !transitionAllowed(control.Mode, target) {
		return Report{}, &OperationError{Code: BlockerTransitionNotAllowed}
	}
	if target == "hard_maintenance" {
		var running, pendingDeliveries int64
		if err = tx.QueryRow(ctx, `
SELECT (SELECT COUNT(*)::bigint FROM runs WHERE status = 'running'),
       (SELECT COUNT(*)::bigint FROM webhook_deliveries WHERE status = 'pending')
     + (SELECT COUNT(*)::bigint FROM run_deliveries WHERE status = 'pending')
     + (SELECT COUNT(*)::bigint FROM task_callback_deliveries WHERE status = 'pending')
`).Scan(&running, &pendingDeliveries); err != nil {
			return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
		}
		if running > 0 {
			return Report{}, &OperationError{Code: BlockerRuntimeRunsNonterminal}
		}
		if pendingDeliveries > 0 {
			return Report{}, &OperationError{Code: BlockerLegacyDeliveriesPending}
		}
	}
	if target == "normal" {
		// Freeze membership and schema evidence between validation and the mode
		// update. Heartbeats wait for this short transaction; they can never be
		// omitted from the exact replica set and then race the reopen commit.
		if _, lockErr := tx.Exec(ctx, `LOCK TABLE runtime_cluster_members IN SHARE MODE`); lockErr != nil {
			return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
		}
		if _, lockErr := tx.Exec(ctx, `LOCK TABLE runtime_schema_contracts IN SHARE MODE`); lockErr != nil {
			return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
		}
		report, collectErr := s.collectWithQuerier(ctx, tx)
		if collectErr != nil {
			return Report{}, collectErr
		}
		report.Readiness = s.evaluateRuntimeReadiness(report, true)
		if !report.Readiness.Ready {
			return report, &OperationError{Code: report.Readiness.Blockers[0].Code}
		}
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	newControl := controlAfterTransition(control, target, req, now)
	commandID := uuid.New()
	tag, err := tx.Exec(ctx, `
UPDATE runtime_cluster_control
SET mode = $2, expected_replicas = $3, cutover_id = $4,
    drain_started_at = $5, drain_deadline_at = $6,
    hard_maintenance_at = $7, reopened_at = $8,
    version = version + 1, updated_by_type = 'runtime_cutover_cli',
    updated_by_id = $9, updated_at = clock_timestamp()
WHERE singleton_id = 1 AND version = $1
`, control.Version, newControl.Mode, newControl.ExpectedReplicas,
		newControl.CutoverID, newControl.DrainStartedAt, newControl.DrainDeadlineAt,
		newControl.HardMaintenanceAt, newControl.ReopenedAt, commandID)
	if err != nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	if tag.RowsAffected() != 1 {
		return Report{}, &OperationError{Code: BlockerClusterVersionConflict}
	}
	if err = tx.Commit(ctx); err != nil {
		return Report{}, &OperationError{Code: BlockerClusterVersionConflict}
	}
	report, err := s.Status(ctx)
	if err != nil {
		return Report{}, err
	}
	report.Changed = true
	return report, nil
}

func controlAfterTransition(control Control, target string, req TransitionRequest, now time.Time) Control {
	newControl := control
	switch target {
	case "draining":
		newControl.Mode = target
		newControl.ExpectedReplicas = req.ExpectedReplicas
		newControl.CutoverID = uuid.New()
		newControl.DrainStartedAt = &now
		newControl.DrainDeadlineAt = req.DrainDeadline
		newControl.ReopenedAt = nil
	case "hard_maintenance":
		newControl.Mode = target
		switch control.Mode {
		case "normal":
			newControl.CutoverID = uuid.New()
			newControl.DrainStartedAt = nil
			newControl.DrainDeadlineAt = nil
			newControl.HardMaintenanceAt = now
		case "draining":
			newControl.HardMaintenanceAt = now
		}
		newControl.ReopenedAt = nil
	case "normal":
		newControl.Mode = target
		newControl.ReopenedAt = &now
	}
	return newControl
}

func transitionAllowed(current, target string) bool {
	switch target {
	case "draining":
		return current == "normal"
	case "hard_maintenance":
		return current == "normal" || current == "draining" || current == "hard_maintenance"
	case "normal":
		return current == "hard_maintenance"
	default:
		return false
	}
}

// collectWithQuerier reads reopen evidence while the control row is locked.
// It intentionally omits legacy/history checks; hard-maintenance transition
// already established zero nonterminal Runs and migrations own schema checks.
func (s *Service) collectWithQuerier(ctx context.Context, tx pgx.Tx) (Report, error) {
	report := Report{
		SchemaInstalled: true,
		Current:         Current{SchemaChecksum: s.identity.SchemaChecksum, ReleaseID: s.identity.ReleaseID, GitSHA: s.identity.GitSHA},
		Members:         []Member{}, SignalBus: SignalBus{Mode: s.signalMode},
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&report.DatabaseTime); err != nil {
		return Report{}, &OperationError{Code: BlockerDatabaseUnavailable}
	}
	control := &Control{}
	if err := tx.QueryRow(ctx, `
SELECT mode, expected_replicas, cutover_id, drain_started_at,
       drain_deadline_at, hard_maintenance_at, reopened_at, version, updated_at
FROM runtime_cluster_control WHERE singleton_id = 1
`).Scan(
		&control.Mode, &control.ExpectedReplicas, &control.CutoverID,
		&control.DrainStartedAt, &control.DrainDeadlineAt,
		&control.HardMaintenanceAt, &control.ReopenedAt,
		&control.Version, &control.UpdatedAt,
	); err != nil {
		return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
	}
	report.Control = control
	if err := tx.QueryRow(ctx, `
SELECT schema_version, migration_name, runtime_contract_id, runtime_contract_digest
FROM runtime_schema_contracts WHERE is_current
`).Scan(
		&report.Current.SchemaVersion, &report.Current.MigrationName,
		&report.Current.RuntimeContractID, &report.Current.RuntimeContractDigest,
	); err != nil {
		return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
	}
	rows, err := tx.Query(ctx, `
SELECT instance_id, release_version, release_commit, schema_version,
       schema_checksum, runtime_contract_id, runtime_contract_digest,
       started_at, heartbeat_at, draining, ready,
       heartbeat_at >= clock_timestamp() - ($1::bigint * interval '1 millisecond')
FROM runtime_cluster_members ORDER BY started_at, instance_id
`, s.liveWindow.Milliseconds())
	if err != nil {
		return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
	}
	defer rows.Close()
	for rows.Next() {
		var member Member
		if err = rows.Scan(
			&member.InstanceID, &member.ReleaseID, &member.GitSHA,
			&member.SchemaVersion, &member.SchemaChecksum,
			&member.RuntimeContractID, &member.RuntimeContractDigest,
			&member.StartedAt, &member.LastSeenAt, &member.Draining,
			&member.Ready, &member.Live,
		); err != nil {
			return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
		}
		report.Members = append(report.Members, member)
	}
	if err = rows.Err(); err != nil {
		return Report{}, &OperationError{Code: BlockerClusterControlUnavailable}
	}
	report.SignalBus.Healthy = s.signalHealthy(ctx)
	return report, nil
}

func IsBlocked(err error) bool {
	var operationErr *OperationError
	if !errors.As(err, &operationErr) {
		return false
	}
	return operationErr.Code != BlockerDatabaseUnavailable
}

func ErrorCode(err error) string {
	var operationErr *OperationError
	if errors.As(err, &operationErr) {
		return operationErr.Code
	}
	return BlockerDatabaseUnavailable
}
