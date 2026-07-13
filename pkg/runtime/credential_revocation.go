package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runtimeCredentialRevocationRetries = 3

// ErrRuntimeCredentialSessionScopeChanged asks the API caller to retry after
// bounded lock-order retries could not capture a concurrently created Session.
var ErrRuntimeCredentialSessionScopeChanged = errors.New("runtime credential session scope changed")

type lockedRuntimeCredentialSession struct {
	runtimeSessionID uuid.UUID
	nodeID           uuid.UUID
	agentID          uuid.UUID
	coreInstanceID   *uuid.UUID
}

// RevokeAgentCredential closes every Session owned by one Agent Token before
// revoking it. The transaction follows the global principal lock order:
// Session -> Node -> Token -> Attachment. Runtime Nodes are only locked for
// serialization; they remain available to Sessions using other credentials.
func RevokeAgentCredential(
	ctx context.Context,
	pool *pgxpool.Pool,
	creatorID uuid.UUID,
	credentialID uuid.UUID,
) (bool, error) {
	if pool == nil || creatorID == uuid.Nil || credentialID == uuid.Nil {
		return false, errors.New("runtime credential revocation is not configured")
	}
	for attempt := 0; attempt < runtimeCredentialRevocationRetries; attempt++ {
		revoked, err := revokeAgentCredentialOnce(ctx, pool, creatorID, credentialID)
		if !errors.Is(err, ErrRuntimeCredentialSessionScopeChanged) {
			return revoked, err
		}
	}
	return false, ErrRuntimeCredentialSessionScopeChanged
}

func revokeAgentCredentialOnce(
	ctx context.Context,
	pool *pgxpool.Pool,
	creatorID uuid.UUID,
	credentialID uuid.UUID,
) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Global principal lock order: Session -> Node -> Token -> Attachment.
	sessions, err := lockRuntimeCredentialSessions(ctx, tx, credentialID)
	if err != nil {
		return false, err
	}
	if err = lockRuntimeCredentialNodes(ctx, tx, runtimeCredentialNodeIDs(sessions)); err != nil {
		return false, err
	}

	var ownerID uuid.UUID
	var agentID *uuid.UUID
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `
SELECT creator_user_id, agent_id, revoked_at
FROM agent_tokens
WHERE id = $1
FOR UPDATE`, credentialID).Scan(&ownerID, &agentID, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (ownerID != creatorID || revokedAt != nil)) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	sessionIDs := runtimeCredentialSessionIDs(sessions)
	if err = lockRuntimeCredentialAttachments(ctx, tx, sessionIDs); err != nil {
		return false, err
	}
	changed, err := runtimeCredentialSessionScopeChanged(ctx, tx, credentialID, sessionIDs)
	if err != nil {
		return false, err
	}
	if changed {
		return false, ErrRuntimeCredentialSessionScopeChanged
	}

	if len(sessionIDs) > 0 {
		if _, err = tx.Exec(ctx, `
UPDATE runtime_session_attachments
SET detached_at = clock_timestamp(), disconnect_reason = 'credential_revoked'
WHERE runtime_session_id = ANY($1::uuid[])
  AND detached_at IS NULL`, sessionIDs); err != nil {
			return false, err
		}
		if _, err = tx.Exec(ctx, `
UPDATE runtime_sessions
SET status = 'revoked',
    capacity = GREATEST(capacity, inflight),
    attached_core_instance_id = NULL,
    disconnected_at = COALESCE(disconnected_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE runtime_session_id = ANY($1::uuid[])
  AND status IN ('active', 'draining', 'offline')`, sessionIDs); err != nil {
			return false, err
		}
	}
	// Attempts and Session/Node inflight counters are durable execution
	// evidence. Do not rewrite them during credential revocation; the expiry
	// reconciler fences the attempts and releases both slots exactly once.

	result, err := tx.Exec(ctx, `
UPDATE agent_tokens
SET revoked_at = clock_timestamp(),
    status = 'revoked',
    revocation_kind = 'manual'
WHERE id = $1
  AND creator_user_id = $2
  AND revoked_at IS NULL`, credentialID, creatorID)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, nil
	}
	if agentID != nil {
		if err = enqueueRuntimeCredentialRevocationSignals(ctx, tx, sessions, *agentID); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func lockRuntimeCredentialSessions(
	ctx context.Context,
	tx pgx.Tx,
	credentialID uuid.UUID,
) ([]lockedRuntimeCredentialSession, error) {
	rows, err := tx.Query(ctx, `
SELECT runtime_session_id, node_id, agent_id, attached_core_instance_id
FROM runtime_sessions
WHERE credential_id = $1
  AND status IN ('active', 'draining', 'offline')
ORDER BY runtime_session_id ASC
FOR UPDATE`, credentialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]lockedRuntimeCredentialSession, 0)
	for rows.Next() {
		var session lockedRuntimeCredentialSession
		if err = rows.Scan(
			&session.runtimeSessionID,
			&session.nodeID,
			&session.agentID,
			&session.coreInstanceID,
		); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func lockRuntimeCredentialNodes(ctx context.Context, tx pgx.Tx, nodeIDs []uuid.UUID) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT node_id
FROM runtime_nodes
WHERE node_id = ANY($1::uuid[])
ORDER BY node_id ASC
FOR UPDATE`, nodeIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	locked := 0
	for rows.Next() {
		var ignored uuid.UUID
		if err = rows.Scan(&ignored); err != nil {
			return err
		}
		locked++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if locked != len(nodeIDs) {
		return errors.New("runtime credential node disappeared")
	}
	return nil
}

func lockRuntimeCredentialAttachments(ctx context.Context, tx pgx.Tx, sessionIDs []uuid.UUID) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT id
FROM runtime_session_attachments
WHERE runtime_session_id = ANY($1::uuid[])
  AND detached_at IS NULL
ORDER BY id ASC
FOR UPDATE`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ignored uuid.UUID
		if err = rows.Scan(&ignored); err != nil {
			return err
		}
	}
	return rows.Err()
}

func runtimeCredentialSessionScopeChanged(
	ctx context.Context,
	tx pgx.Tx,
	credentialID uuid.UUID,
	locked []uuid.UUID,
) (bool, error) {
	var changed bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM runtime_sessions
    WHERE credential_id = $1
      AND status IN ('active', 'draining', 'offline')
      AND NOT (runtime_session_id = ANY($2::uuid[]))
)`, credentialID, locked).Scan(&changed)
	return changed, err
}

func enqueueRuntimeCredentialRevocationSignals(
	ctx context.Context,
	tx pgx.Tx,
	sessions []lockedRuntimeCredentialSession,
	agentID uuid.UUID,
) error {
	coreIDs := runtimeCredentialCoreIDs(sessions)
	for _, coreID := range coreIDs {
		payload, err := json.Marshal(map[string]string{"target_instance_id": coreID.String()})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `
INSERT INTO runtime_signal_outbox (event_type, agent_id, payload, available_at)
VALUES ('credential.revoke', $1, $2, clock_timestamp())`, agentID, payload); err != nil {
			return err
		}
	}
	return nil
}

func runtimeCredentialSessionIDs(sessions []lockedRuntimeCredentialSession) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.runtimeSessionID)
	}
	return ids
}

func runtimeCredentialNodeIDs(sessions []lockedRuntimeCredentialSession) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(sessions))
	ids := make([]uuid.UUID, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := seen[session.nodeID]; ok {
			continue
		}
		seen[session.nodeID] = struct{}{}
		ids = append(ids, session.nodeID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func runtimeCredentialCoreIDs(sessions []lockedRuntimeCredentialSession) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(sessions))
	ids := make([]uuid.UUID, 0, len(sessions))
	for _, session := range sessions {
		if session.coreInstanceID == nil || *session.coreInstanceID == uuid.Nil {
			continue
		}
		if _, ok := seen[*session.coreInstanceID]; ok {
			continue
		}
		seen[*session.coreInstanceID] = struct{}{}
		ids = append(ids, *session.coreInstanceID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}
