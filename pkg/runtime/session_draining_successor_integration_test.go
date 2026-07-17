package runtime_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestRuntimeSessionOfflineIdentityClaimsDrainingNodeWithCompleteEvidence(t *testing.T) {
	pool, fixture := prepareOfflineDrainingRuntimeSession(t)
	principal := runtimeNodeAdminPrincipal(fixture)
	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)
	request := runtimeNodeAdminSessionRequest(fixture, fixture.sessionID, "admin-worker", 1, 9)

	state, err := sessions.CreateOrAttachSession(context.Background(), principal, request)
	require.NoError(t, err)
	require.Equal(t, "draining", state.Session.Status)
	require.Zero(t, state.Session.Capacity)
	require.NotNil(t, state.Attachment)
	require.Equal(t, "resumed", state.Attachment.AttachmentKind)

	var status, reason string
	var capacity, resumeCapacity int32
	var requestedAt, deadlineAt time.Time
	var attachedCore uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT status, capacity, attached_core_instance_id, drain_requested_at,
       drain_deadline_at, drain_reason_code, resume_capacity
FROM runtime_sessions
WHERE runtime_session_id = $1`, fixture.sessionID).Scan(
		&status, &capacity, &attachedCore, &requestedAt, &deadlineAt, &reason,
		&resumeCapacity,
	))
	require.Equal(t, "draining", status)
	require.Zero(t, capacity)
	require.Equal(t, fixture.coreInstanceID, attachedCore)
	require.Equal(t, "ADMIN_REQUESTED", reason)
	require.Equal(t, int32(9), resumeCapacity)
	require.WithinDuration(t, requestedAt.Add(time.Minute), deadlineAt, 100*time.Millisecond)
}

func TestRuntimeSessionDrainingSuccessorUsesStableWorkerAndCurrentCredential(t *testing.T) {
	pool, fixture := prepareOfflineDrainingRuntimeSession(t)

	// Token rotation is allowed: continuity comes from the certified Node,
	// Agent, stable worker and monotonic epoch; the presented token must still
	// be independently valid at the database clock.
	rotatedCredentialID := uuid.New()
	_, err := pool.Exec(context.Background(), `
INSERT INTO agent_tokens (
    id, agent_id, creator_user_id, name, prefix, token_hash, scopes,
    status, redeemed_at
)
SELECT $1, agent_id, creator_user_id, 'successor-rotated-token', $2, $3,
       ARRAY['agent:pull']::text[], 'active_runtime', clock_timestamp()
FROM agent_tokens
WHERE id = $4`, rotatedCredentialID,
		"ol_agent_"+rotatedCredentialID.String()[:8],
		"successor-token-"+rotatedCredentialID.String(), fixture.credentialID)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `
UPDATE agent_tokens
SET status = 'revoked', revoked_at = clock_timestamp(), revocation_kind = 'manual'
WHERE id = $1`, fixture.credentialID)
	require.NoError(t, err)

	principal := runtimeNodeAdminPrincipal(fixture)
	principal.CredentialID = rotatedCredentialID
	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)
	successorID := uuid.New()
	request := runtimeNodeAdminSessionRequest(fixture, successorID, "admin-worker", 2, 7)

	state, err := sessions.CreateOrAttachSession(context.Background(), principal, request)
	require.NoError(t, err)
	require.Equal(t, successorID, state.Session.RuntimeSessionID)
	require.Equal(t, int64(2), state.Session.SessionEpoch)
	require.Equal(t, rotatedCredentialID, state.Session.CredentialID)
	require.Equal(t, "draining", state.Session.Status)
	require.Zero(t, state.Session.Capacity)
	require.NotNil(t, state.Attachment)
	require.Equal(t, "connected", state.Attachment.AttachmentKind)

	type sessionEvidence struct {
		status         string
		capacity       int32
		attachedCore   *uuid.UUID
		disconnectedAt *time.Time
		requestedAt    *time.Time
		deadlineAt     *time.Time
		reason         *string
		resumeCapacity *int32
	}
	readEvidence := func(sessionID uuid.UUID) sessionEvidence {
		t.Helper()
		var evidence sessionEvidence
		require.NoError(t, pool.QueryRow(context.Background(), `
SELECT status, capacity, attached_core_instance_id, disconnected_at,
       drain_requested_at, drain_deadline_at, drain_reason_code,
       resume_capacity
FROM runtime_sessions
WHERE runtime_session_id = $1`, sessionID).Scan(
			&evidence.status, &evidence.capacity, &evidence.attachedCore,
			&evidence.disconnectedAt, &evidence.requestedAt,
			&evidence.deadlineAt, &evidence.reason, &evidence.resumeCapacity,
		))
		return evidence
	}

	predecessor := readEvidence(fixture.sessionID)
	require.Equal(t, "offline", predecessor.status)
	require.Nil(t, predecessor.attachedCore)
	require.NotNil(t, predecessor.disconnectedAt)
	require.Nil(t, predecessor.requestedAt,
		"administrative drain intentionally did not mutate the offline predecessor")
	require.Nil(t, predecessor.resumeCapacity)

	successor := readEvidence(successorID)
	require.Equal(t, "draining", successor.status)
	require.Zero(t, successor.capacity)
	require.NotNil(t, successor.attachedCore)
	require.Equal(t, fixture.coreInstanceID, *successor.attachedCore)
	require.Nil(t, successor.disconnectedAt)
	require.NotNil(t, successor.requestedAt)
	require.NotNil(t, successor.deadlineAt)
	require.NotNil(t, successor.reason)
	require.Equal(t, "ADMIN_REQUESTED", *successor.reason)
	require.NotNil(t, successor.resumeCapacity)
	require.Equal(t, int32(7), *successor.resumeCapacity)
	require.WithinDuration(t, successor.requestedAt.Add(time.Minute), *successor.deadlineAt, 100*time.Millisecond)
}

func TestRuntimeSessionDrainingSuccessorRejectsEveryUnprovenNewIdentity(t *testing.T) {
	pool, fixture := prepareOfflineDrainingRuntimeSession(t)
	principal := runtimeNodeAdminPrincipal(fixture)
	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)

	t.Run("ordinary new worker has no predecessor", func(t *testing.T) {
		request := runtimeNodeAdminSessionRequest(fixture, uuid.New(), "ordinary-new-worker", 2, 3)
		_, err := sessions.CreateOrAttachSession(context.Background(), principal, request)
		require.True(t, runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorPrincipalInactive), "%v", err)
	})

	t.Run("same epoch cannot fork the stable worker", func(t *testing.T) {
		request := runtimeNodeAdminSessionRequest(fixture, uuid.New(), "admin-worker", 1, 3)
		_, err := sessions.CreateOrAttachSession(context.Background(), principal, request)
		require.True(t, runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorSessionConflict), "%v", err)
	})

	t.Run("different certificate cannot inherit the predecessor", func(t *testing.T) {
		request := runtimeNodeAdminSessionRequest(fixture, uuid.New(), "admin-worker", 2, 3)
		wrongPrincipal := principal
		wrongPrincipal.Device.CertificateSerial = "deadbeef"
		_, err := sessions.CreateOrAttachSession(context.Background(), wrongPrincipal, request)
		require.True(t, runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorDeviceMismatch), "%v", err)
	})

	t.Run("revoked token cannot create the successor", func(t *testing.T) {
		_, err := pool.Exec(context.Background(), `
UPDATE agent_tokens
SET status = 'revoked', revoked_at = clock_timestamp(), revocation_kind = 'manual'
WHERE id = $1`, fixture.credentialID)
		require.NoError(t, err)
		request := runtimeNodeAdminSessionRequest(fixture, uuid.New(), "admin-worker", 2, 3)
		_, err = sessions.CreateOrAttachSession(context.Background(), principal, request)
		require.True(t, runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorPrincipalInactive), "%v", err)
	})

	var count int32
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT COUNT(*)::int
FROM runtime_sessions
WHERE node_id = $1`, fixture.nodeID).Scan(&count))
	require.Equal(t, int32(1), count, "no rejected identity may leave a Session row")
}

func TestRuntimeSessionDrainingSuccessorRaceCommitsExactlyOneEpoch(t *testing.T) {
	pool, fixture := prepareOfflineDrainingRuntimeSession(t)
	principal := runtimeNodeAdminPrincipal(fixture)
	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)

	const contenders = 12
	start := make(chan struct{})
	errs := make(chan error, contenders)
	winners := make(chan uuid.UUID, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		sessionID := uuid.New()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			request := runtimeNodeAdminSessionRequest(fixture, sessionID, "admin-worker", 2, 5)
			state, err := sessions.CreateOrAttachSession(context.Background(), principal, request)
			if err == nil {
				winners <- state.Session.RuntimeSessionID
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(winners)

	var successes int
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		require.True(t,
			runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorSessionConflict) ||
				runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorPrincipalInactive),
			"unexpected contender error: %v", err,
		)
	}
	require.Equal(t, 1, successes)
	require.Len(t, winners, 1)
	winnerID := <-winners

	var total, successorCount int32
	var winnerStatus, reason string
	var capacity, resumeCapacity int32
	var requestedAt, deadlineAt time.Time
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT COUNT(*)::int,
       COUNT(*) FILTER (WHERE session_epoch = 2)::int
FROM runtime_sessions
WHERE node_id = $1 AND agent_id = $2 AND worker_id = 'admin-worker'`,
		fixture.nodeID, fixture.agentID).Scan(&total, &successorCount))
	require.Equal(t, int32(2), total)
	require.Equal(t, int32(1), successorCount)
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT status, capacity, drain_requested_at, drain_deadline_at,
       drain_reason_code, resume_capacity
FROM runtime_sessions
WHERE runtime_session_id = $1`, winnerID).Scan(
		&winnerStatus, &capacity, &requestedAt, &deadlineAt, &reason, &resumeCapacity,
	))
	require.Equal(t, "draining", winnerStatus)
	require.Zero(t, capacity)
	require.Equal(t, "ADMIN_REQUESTED", reason)
	require.Equal(t, int32(5), resumeCapacity)
	require.WithinDuration(t, requestedAt.Add(time.Minute), deadlineAt, 100*time.Millisecond)
}

func TestRuntimeSessionDrainingSuccessorMixedEpochRaceCommitsExactlyOne(t *testing.T) {
	pool, fixture := prepareOfflineDrainingRuntimeSession(t)
	principal := runtimeNodeAdminPrincipal(fixture)
	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)

	type winner struct {
		sessionID uuid.UUID
		epoch     int64
	}
	const contenders = 12
	start := make(chan struct{})
	errs := make(chan error, contenders)
	winners := make(chan winner, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		sessionID := uuid.New()
		epoch := int64(2 + i%2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			request := runtimeNodeAdminSessionRequest(fixture, sessionID, "admin-worker", epoch, 5)
			state, err := sessions.CreateOrAttachSession(context.Background(), principal, request)
			if err == nil {
				winners <- winner{sessionID: state.Session.RuntimeSessionID, epoch: state.Session.SessionEpoch}
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(winners)

	var successes int
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		require.True(t, runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorSessionConflict),
			"mixed-epoch contender error: %v", err)
	}
	require.Equal(t, 1, successes)
	require.Len(t, winners, 1)
	won := <-winners
	require.Contains(t, []int64{2, 3}, won.epoch)

	var total, successorCount int32
	var storedEpoch int64
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT COUNT(*)::int,
       COUNT(*) FILTER (WHERE session_epoch > 1)::int,
       MAX(session_epoch) FILTER (WHERE runtime_session_id = $3)
FROM runtime_sessions
WHERE node_id = $1 AND agent_id = $2 AND worker_id = 'admin-worker'`,
		fixture.nodeID, fixture.agentID, won.sessionID).Scan(&total, &successorCount, &storedEpoch))
	require.Equal(t, int32(2), total)
	require.Equal(t, int32(1), successorCount)
	require.Equal(t, won.epoch, storedEpoch)
}

func TestRuntimeSessionDrainingSuccessorCannotSkipLatestIneligiblePredecessor(t *testing.T) {
	pool, fixture := prepareOfflineDrainingRuntimeSession(t)
	principal := runtimeNodeAdminPrincipal(fixture)
	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)

	latestID := uuid.New()
	_, err := pool.Exec(context.Background(), `
INSERT INTO runtime_sessions (
    runtime_session_id, node_id, agent_id, credential_id, worker_id,
    session_epoch, device_certificate_serial, node_version, protocol_version,
    runtime_contract_id, runtime_contract_digest, features, capacity, inflight,
    status, attached_core_instance_id, connected_at, heartbeat_at,
    disconnected_at
)
SELECT $1, node_id, agent_id, credential_id, worker_id,
       2, device_certificate_serial, node_version, protocol_version,
       runtime_contract_id, runtime_contract_digest, features, capacity, 0,
       'closed', NULL, clock_timestamp(), clock_timestamp(), clock_timestamp()
FROM runtime_sessions
WHERE runtime_session_id = $2`, latestID, fixture.sessionID)
	require.NoError(t, err)

	request := runtimeNodeAdminSessionRequest(fixture, uuid.New(), "admin-worker", 3, 5)
	_, err = sessions.CreateOrAttachSession(context.Background(), principal, request)
	require.True(t, runtime.IsRuntimeSessionError(err, runtime.RuntimeSessionErrorPrincipalInactive), "%v", err)

	var total, latestEpoch, createdEpochThree int32
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT COUNT(*)::int, MAX(session_epoch)::int,
       COUNT(*) FILTER (WHERE session_epoch = 3)::int
FROM runtime_sessions
WHERE node_id = $1 AND agent_id = $2 AND worker_id = 'admin-worker'`,
		fixture.nodeID, fixture.agentID).Scan(&total, &latestEpoch, &createdEpochThree))
	require.Equal(t, int32(2), total)
	require.Equal(t, int32(2), latestEpoch)
	require.Zero(t, createdEpochThree,
		"the eligible epoch-1 predecessor must not be selected past the closed epoch-2 predecessor")
}

func prepareOfflineDrainingRuntimeSession(
	t *testing.T,
) (*pgxpool.Pool, runtimeNodeAdminFixture) {
	t.Helper()
	pool := setupTestDB(t)
	requireReliableRuntimeSchema(t, pool)
	resetRuntimeNodeAdminTables(t, pool)
	fixture := insertRuntimeNodeAdminFixture(t, pool)
	_, err := pool.Exec(context.Background(), `
UPDATE runtime_sessions SET inflight = 0 WHERE runtime_session_id = $1`, fixture.sessionID)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `
UPDATE runtime_nodes SET inflight = 0 WHERE node_id = $1`, fixture.nodeID)
	require.NoError(t, err)

	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)
	detached, err := sessions.DetachCutoverSessions(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, detached)

	_, err = newTestService(t, pool).DrainRuntimeNode(context.Background(), fixture.nodeID)
	require.NoError(t, err)
	var nodeStatus, sessionStatus string
	var requestedAt *time.Time
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT node.status, session.status, session.drain_requested_at
FROM runtime_nodes node
JOIN runtime_sessions session ON session.node_id = node.node_id
WHERE session.runtime_session_id = $1`, fixture.sessionID).Scan(
		&nodeStatus, &sessionStatus, &requestedAt,
	))
	require.Equal(t, "draining", nodeStatus)
	require.Equal(t, "offline", sessionStatus)
	require.Nil(t, requestedAt)
	return pool, fixture
}

func runtimeNodeAdminSessionRequest(
	fixture runtimeNodeAdminFixture,
	sessionID uuid.UUID,
	workerID string,
	epoch int64,
	capacity int32,
) runtime.RuntimeSessionRequest {
	features := runtime.RuntimeRequiredFeatures()
	sort.Strings(features)
	return runtime.RuntimeSessionRequest{
		RuntimeSessionIdentity: runtime.RuntimeSessionIdentity{
			RuntimeSessionID: sessionID,
			NodeID:           fixture.nodeID,
			AgentID:          fixture.agentID,
			WorkerID:         workerID,
			SessionEpoch:     epoch,
		},
		NodeVersion:           "node-admin-v2",
		ProtocolVersion:       runtime.RuntimeProtocolVersion,
		RuntimeContractID:     runtime.RuntimeContractID,
		RuntimeContractDigest: runtime.RuntimeContractDigest,
		Features:              features,
		Capacity:              capacity,
		Transport:             runtime.RuntimeTransportWebSocket,
	}
}
