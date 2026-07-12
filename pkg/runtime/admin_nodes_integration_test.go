package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestRuntimeNodeAdminInventoryDrainAndRevokeAgainstPostgres(t *testing.T) {
	pool := setupTestDB(t)
	requireReliableRuntimeV2Schema(t, pool)
	resetRuntimeNodeAdminTables(t, pool)
	fixture := insertRuntimeNodeAdminFixture(t, pool)
	svc := newTestService(t, pool)

	inventory, err := svc.ListRuntimeNodes(context.Background(), 25, 0)
	require.NoError(t, err)
	require.Equal(t, int32(1), inventory.Total)
	require.Equal(t, int32(25), inventory.Limit)
	require.Equal(t, runtime.RuntimeContractID, inventory.CurrentContractID)
	require.Equal(t, runtime.RuntimeContractDigest, inventory.CurrentContractDigest)
	require.False(t, inventory.DatabaseTime.IsZero())
	require.Len(t, inventory.Items, 1)
	node := inventory.Items[0]
	require.Equal(t, fixture.nodeID.String(), node.NodeID)
	require.True(t, node.ContractMatch)
	require.Equal(t, int32(1), node.ActiveSessionCount)
	require.Equal(t, int32(1), node.ActiveAgentCount)
	require.Equal(t, int32(4), node.Capacity)
	require.Equal(t, int32(1), node.Inflight)

	drained, err := svc.DrainRuntimeNode(context.Background(), fixture.nodeID)
	require.NoError(t, err)
	require.Equal(t, "draining", drained.Status)
	require.Equal(t, int32(1), drained.ActiveSessionCount)
	requireRuntimeNodeAdminState(t, pool, fixture, "draining", "draining", false)
	requireRuntimeNodeSignal(t, pool, fixture, "node.drain", 1)

	revoked, err := svc.RevokeRuntimeNode(context.Background(), fixture.nodeID, "rotating the host certificate")
	require.NoError(t, err)
	require.Equal(t, "revoked", revoked.Status)
	require.Equal(t, int32(0), revoked.ActiveSessionCount)
	require.Equal(t, "rotating the host certificate", *revoked.RevokeReason)
	requireRuntimeNodeAdminState(t, pool, fixture, "revoked", "revoked", true)
	requireRuntimeNodeSignal(t, pool, fixture, "node.revoke", 1)

	// Revocation is idempotent and never rewrites immutable evidence or emits
	// duplicate durable signals.
	replayed, err := svc.RevokeRuntimeNode(context.Background(), fixture.nodeID, "different retry reason")
	require.NoError(t, err)
	require.Equal(t, "rotating the host certificate", *replayed.RevokeReason)
	requireRuntimeNodeSignal(t, pool, fixture, "node.revoke", 1)

	var tokenStatus string
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT status FROM agent_tokens WHERE id = $1`, fixture.credentialID).Scan(&tokenStatus))
	require.Equal(t, "active_runtime", tokenStatus, "Node revocation must not revoke a portable Agent credential")
}

type runtimeNodeAdminFixture struct {
	nodeID         uuid.UUID
	agentID        uuid.UUID
	credentialID   uuid.UUID
	sessionID      uuid.UUID
	coreInstanceID uuid.UUID
}

func resetRuntimeNodeAdminTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
TRUNCATE runtime_signal_outbox, runtime_session_attachments,
         runtime_sessions, runtime_nodes RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
}

func insertRuntimeNodeAdminFixture(t *testing.T, pool *pgxpool.Pool) runtimeNodeAdminFixture {
	t.Helper()
	creatorID := insertCreator(t, pool)
	agentID := insertAgent(t, pool, creatorID, "https://example.com/runtime", 0, "approved")
	_, err := pool.Exec(context.Background(), `
UPDATE agents
SET connection_mode = 'runtime_ws', endpoint_url = 'openlinker-runtime-ws://admin-test'
WHERE id = $1`, agentID)
	require.NoError(t, err)

	fixture := runtimeNodeAdminFixture{
		nodeID:         uuid.New(),
		agentID:        agentID,
		credentialID:   uuid.New(),
		sessionID:      uuid.New(),
		coreInstanceID: uuid.New(),
	}
	serial := strings.ReplaceAll(fixture.nodeID.String(), "-", "")
	prefix := "ol_agent_" + fixture.credentialID.String()[:8]
	err = pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `
INSERT INTO agent_tokens (
    id, agent_id, creator_user_id, name, prefix, token_hash, scopes,
    status, redeemed_at
) VALUES ($1, $2, $3, 'node-admin', $4, 'test-hash',
          ARRAY['agent:pull']::text[], 'active_runtime', clock_timestamp())`,
			fixture.credentialID, fixture.agentID, creatorID, prefix); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(), `
INSERT INTO runtime_nodes (
    node_id, display_name, device_certificate_serial,
    device_public_key_thumbprint, node_version, protocol_version,
    runtime_contract_id, runtime_contract_digest, features,
    capacity, inflight, status, last_seen_at
) VALUES ($1, 'Node admin fixture', $2, $3, 'node-admin-v2', 2,
          $4, $5, $6, 4, 1, 'active', clock_timestamp())`,
			fixture.nodeID, serial, strings.Repeat("b", 64),
			runtime.RuntimeContractID, runtime.RuntimeContractDigest,
			runtime.RuntimeRequiredFeatures()); err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(), `
INSERT INTO runtime_sessions (
    runtime_session_id, node_id, agent_id, credential_id, worker_id,
    session_epoch, device_certificate_serial, node_version,
    protocol_version, runtime_contract_id, runtime_contract_digest,
    features, capacity, inflight, status, attached_core_instance_id,
    heartbeat_at
) VALUES ($1, $2, $3, $4, 'admin-worker', 1, $5, 'node-admin-v2',
          2, $6, $7, $8, 2, 1, 'active', $9, clock_timestamp())`,
			fixture.sessionID, fixture.nodeID, fixture.agentID,
			fixture.credentialID, serial, runtime.RuntimeContractID,
			runtime.RuntimeContractDigest, runtime.RuntimeRequiredFeatures(),
			fixture.coreInstanceID); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `
INSERT INTO runtime_session_attachments (
    runtime_session_id, core_instance_id, attachment_kind
) VALUES ($1, $2, 'connected')`, fixture.sessionID, fixture.coreInstanceID)
		return err
	})
	require.NoError(t, err)
	return fixture
}

func requireRuntimeNodeAdminState(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture runtimeNodeAdminFixture,
	nodeStatus, sessionStatus string,
	attachmentClosed bool,
) {
	t.Helper()
	var gotNode, gotSession string
	var attachedCore *uuid.UUID
	var detachedAtPresent bool
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT n.status, s.status, s.attached_core_instance_id,
       attachment.detached_at IS NOT NULL
FROM runtime_nodes n
JOIN runtime_sessions s ON s.node_id = n.node_id
JOIN runtime_session_attachments attachment
  ON attachment.runtime_session_id = s.runtime_session_id
WHERE n.node_id = $1 AND s.runtime_session_id = $2`,
		fixture.nodeID, fixture.sessionID,
	).Scan(&gotNode, &gotSession, &attachedCore, &detachedAtPresent))
	require.Equal(t, nodeStatus, gotNode)
	require.Equal(t, sessionStatus, gotSession)
	require.Equal(t, attachmentClosed, detachedAtPresent)
	if attachmentClosed {
		require.Nil(t, attachedCore)
	} else {
		require.NotNil(t, attachedCore)
		require.Equal(t, fixture.coreInstanceID, *attachedCore)
	}
}

func requireRuntimeNodeSignal(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture runtimeNodeAdminFixture,
	eventType string,
	want int32,
) {
	t.Helper()
	var count int32
	var payload []byte
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT COUNT(*)::int, COALESCE(MAX(payload::text), '{}')::bytea
FROM runtime_signal_outbox
WHERE event_type = $1 AND agent_id = $2`, eventType, fixture.agentID).Scan(&count, &payload))
	require.Equal(t, want, count)
	var decoded map[string]string
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, fixture.coreInstanceID.String(), decoded["target_instance_id"])
}
