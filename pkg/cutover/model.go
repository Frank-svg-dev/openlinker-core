package cutover

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	SignalModeLocal = "local"
	SignalModeRedis = "redis"

	BlockerClusterSchemaUnavailable     = "cluster_schema_unavailable"
	BlockerClusterControlUnavailable    = "cluster_control_unavailable"
	BlockerLegacyRunsNonterminal        = "legacy_runs_nonterminal"
	BlockerLegacyDeliveriesPending      = "legacy_deliveries_pending"
	BlockerLegacyTerminalHistoryInvalid = "legacy_terminal_history_invalid"
	BlockerLegacyEventPayloadInvalid    = "legacy_event_payload_invalid"
	BlockerDatabaseClientsActive        = "database_clients_active"
	BlockerClusterMembersRegistered     = "cluster_members_registered"
	BlockerCurrentSchemaMismatch        = "current_schema_mismatch"
	BlockerCurrentContractMismatch      = "current_contract_mismatch"
	BlockerReplicasUnavailable          = "replicas_unavailable"
	BlockerReplicasUnexpected           = "replicas_unexpected"
	BlockerMemberReleaseMismatch        = "member_release_mismatch"
	BlockerMemberSchemaMismatch         = "member_schema_mismatch"
	BlockerMemberContractMismatch       = "member_contract_mismatch"
	BlockerMemberSchemaChecksumMismatch = "member_schema_checksum_mismatch"
	BlockerMemberNotReady               = "member_not_ready"
	BlockerSignalDependencyUnavailable  = "signal_dependency_unavailable"
	BlockerReleaseIdentityMissing       = "release_identity_missing"
	BlockerExpectedReplicasInvalid      = "expected_replicas_invalid"
	BlockerClusterVersionConflict       = "cluster_version_conflict"
	BlockerCutoverIDMismatch            = "cutover_id_mismatch"
	BlockerTransitionNotAllowed         = "transition_not_allowed"
	BlockerDatabaseUnavailable          = "database_unavailable"
	BlockerRuntimeRunsNonterminal       = "runtime_runs_nonterminal"
	BlockerMaintenance                  = "maintenance"
)

// Blocker is deliberately safe to expose to administrators and automation.
// It contains stable machine codes and redacted evidence, never payloads,
// credentials, database error strings, or Run input/output.
type Blocker struct {
	Code            string `json:"code"`
	Scope           string `json:"scope"`
	ID              string `json:"id,omitempty"`
	MessageRedacted string `json:"message_redacted,omitempty"`
}

type Readiness struct {
	Ready    bool      `json:"ready"`
	Blockers []Blocker `json:"blockers"`
}

type Control struct {
	Mode              string     `json:"mode"`
	ExpectedReplicas  int32      `json:"expected_replicas"`
	CutoverID         uuid.UUID  `json:"cutover_id"`
	DrainStartedAt    *time.Time `json:"drain_started_at,omitempty"`
	DrainDeadlineAt   *time.Time `json:"drain_deadline_at,omitempty"`
	HardMaintenanceAt time.Time  `json:"hard_maintenance_at"`
	ReopenedAt        *time.Time `json:"reopened_at,omitempty"`
	Version           int64      `json:"version"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type Current struct {
	SchemaVersion         int32  `json:"schema_version"`
	SchemaChecksum        string `json:"schema_checksum"`
	MigrationName         string `json:"migration_name,omitempty"`
	RuntimeContractID     string `json:"runtime_contract_id"`
	RuntimeContractDigest string `json:"runtime_contract_digest"`
	ReleaseID             string `json:"release_id"`
	GitSHA                string `json:"git_sha"`
}

type Member struct {
	InstanceID            uuid.UUID `json:"instance_id"`
	ReleaseID             string    `json:"release_id"`
	GitSHA                string    `json:"git_sha"`
	SchemaVersion         int32     `json:"schema_version"`
	SchemaChecksum        string    `json:"schema_checksum"`
	RuntimeContractID     string    `json:"runtime_contract_id"`
	RuntimeContractDigest string    `json:"runtime_contract_digest"`
	StartedAt             time.Time `json:"started_at"`
	LastSeenAt            time.Time `json:"last_seen_at"`
	Live                  bool      `json:"live"`
	Draining              bool      `json:"draining"`
	Ready                 bool      `json:"ready"`
}

type SignalBus struct {
	Mode    string `json:"mode"`
	Healthy bool   `json:"healthy"`
}

type LegacyEvidence struct {
	RunsTablePresent              bool  `json:"runs_table_present"`
	NonterminalRuns               int64 `json:"nonterminal_runs"`
	PendingWebhookDeliveries      int64 `json:"pending_webhook_deliveries"`
	PendingRunDeliveries          int64 `json:"pending_run_deliveries"`
	PendingTaskCallbackDeliveries int64 `json:"pending_task_callback_deliveries"`
	InvalidTerminalRuns           int64 `json:"invalid_terminal_runs"`
	InvalidRunEventPayloads       int64 `json:"invalid_run_event_payloads"`
}

func (e LegacyEvidence) PendingDeliveries() int64 {
	return e.PendingWebhookDeliveries + e.PendingRunDeliveries + e.PendingTaskCallbackDeliveries
}

type DatabaseEvidence struct {
	OtherClientBackends int64 `json:"other_client_backends"`
}

type Report struct {
	DatabaseTime           time.Time        `json:"database_time"`
	SchemaInstalled        bool             `json:"runtime_schema_installed"`
	Control                *Control         `json:"control,omitempty"`
	Current                Current          `json:"current"`
	Members                []Member         `json:"members"`
	Legacy                 *LegacyEvidence  `json:"legacy,omitempty"`
	Database               DatabaseEvidence `json:"database"`
	Readiness              Readiness        `json:"readiness"`
	ReopenReadiness        *Readiness       `json:"reopen_readiness,omitempty"`
	SignalBus              SignalBus        `json:"signal_bus"`
	Changed                bool             `json:"changed,omitempty"`
	RuntimeUninstalledNoop bool             `json:"runtime_uninstalled_noop,omitempty"`
}

func (r Report) MarshalJSON() ([]byte, error) {
	type reportAlias Report
	copyReport := reportAlias(r)
	if copyReport.Members == nil {
		copyReport.Members = []Member{}
	}
	if copyReport.Readiness.Blockers == nil {
		copyReport.Readiness.Blockers = []Blocker{}
	}
	if copyReport.ReopenReadiness != nil && copyReport.ReopenReadiness.Blockers == nil {
		copyReport.ReopenReadiness.Blockers = []Blocker{}
	}
	return json.Marshal(copyReport)
}

type Identity struct {
	ReleaseID             string
	GitSHA                string
	SchemaVersion         int32
	SchemaChecksum        string
	MigrationName         string
	RuntimeContractID     string
	RuntimeContractDigest string
}

type PreflightOptions struct {
	RequireExclusive bool
	RequireNoMembers bool
}

type TransitionRequest struct {
	ExpectedVersion             int64
	ExpectedReplicas            int32
	CutoverID                   uuid.UUID
	DrainDeadline               *time.Time
	AllowRuntimeUninstalledNoop bool
}

type OperationError struct {
	Code string
}

func (e *OperationError) Error() string {
	if e == nil {
		return "cutover operation failed"
	}
	return e.Code
}
