package runtime_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestRuntimeSessionCutoverDetachClosesExactCoreAttachmentAndIsIdempotent(t *testing.T) {
	pool := setupTestDB(t)
	requireReliableRuntimeSchema(t, pool)
	resetRuntimeNodeAdminTables(t, pool)
	fixture := insertRuntimeNodeAdminFixture(t, pool)
	_, err := pool.Exec(context.Background(), `
UPDATE runtime_sessions SET inflight = 0 WHERE runtime_session_id = $1`, fixture.sessionID)
	require.NoError(t, err)

	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)
	detached, err := sessions.DetachCutoverSessions(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, detached)

	var status string
	var attached bool
	var attachmentClosed bool
	var reason *string
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT session.status,
       session.attached_core_instance_id IS NOT NULL,
       attachment.detached_at IS NOT NULL,
       attachment.disconnect_reason
FROM runtime_sessions session
JOIN runtime_session_attachments attachment
  ON attachment.runtime_session_id = session.runtime_session_id
WHERE session.runtime_session_id = $1
  AND attachment.id = $2`, fixture.sessionID, fixture.attachmentID).Scan(
		&status, &attached, &attachmentClosed, &reason,
	))
	require.Equal(t, "offline", status)
	require.False(t, attached)
	require.True(t, attachmentClosed)
	require.NotNil(t, reason)
	require.Equal(t, "core_cutover_handoff", *reason)

	detached, err = sessions.DetachCutoverSessions(context.Background())
	require.NoError(t, err)
	require.Zero(t, detached)
}

func TestRuntimeSessionCutoverDetachRejectsInflightAndRollsBack(t *testing.T) {
	pool := setupTestDB(t)
	requireReliableRuntimeSchema(t, pool)
	resetRuntimeNodeAdminTables(t, pool)
	fixture := insertRuntimeNodeAdminFixture(t, pool)

	sessions := runtime.NewRuntimeSessionService(pool, fixture.coreInstanceID)
	detached, err := sessions.DetachCutoverSessions(context.Background())
	require.ErrorContains(t, err, "not safely detachable")
	require.Zero(t, detached)
	requireRuntimeNodeAdminState(t, pool, fixture, "active", "active", false)
}
