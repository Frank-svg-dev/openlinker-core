package runtime_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestResolveRunReplayRecoversCommittedRunAfterAgentRetirement(t *testing.T) {
	pool := setupTestDB(t)
	requireReliableRuntimeSchema(t, pool)
	svc := newTestService(t, pool)
	userID := insertRuntimeUser(t, pool)
	creatorID := insertCreator(t, pool)

	agentID := insertAgent(t, pool, creatorID, "openlinker-runtime://replay-resolution", 0, "approved")
	_, err := pool.Exec(context.Background(), `UPDATE agents SET connection_mode = 'runtime' WHERE id = $1`, agentID)
	require.NoError(t, err)
	request := &runtime.RunRequest{
		AgentID:          agentID.String(),
		Input:            map[string]any{"query": "recover me"},
		Metadata:         map[string]any{"trace_id": "replay-resolution"},
		IdempotencyKey:   "resolve-run-replay/committed",
		CreationProtocol: "rest",
		CreationMethod:   "runs.create",
	}

	committed, err := svc.StartRun(context.Background(), userID, request, "web")
	require.NoError(t, err)
	require.Equal(t, "running", committed.Status)

	// Disabling is the product's logical-delete operation for Agents. Mutating
	// both eligibility and endpoint proves replay recovery does not depend on
	// current callability.
	_, err = pool.Exec(context.Background(), `
		UPDATE agents
		SET lifecycle_status = 'disabled', endpoint_url = 'openlinker-runtime://retired'
		WHERE id = $1`, agentID)
	require.NoError(t, err)

	replayed, found, err := svc.LookupRunByCreationRequest(context.Background(), userID, request, "web")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, replayed.Replayed)
	require.Equal(t, committed.RunID, replayed.RunID)
	require.Equal(t, committed.Status, replayed.Status)

	conflicting := *request
	conflicting.Input = map[string]any{"query": "different semantics"}
	_, _, err = svc.LookupRunByCreationRequest(context.Background(), userID, &conflicting, "web")
	var httpErr *httpx.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusConflict, httpErr.Status)
	require.Equal(t, httpx.ErrorCode(runtime.IdempotencyErrorKeyReused), httpErr.Code)
}

func TestResolveRunReplayMissingIdentityHasNoMutableAgentSideEffects(t *testing.T) {
	pool := setupTestDB(t)
	requireReliableRuntimeSchema(t, pool)
	svc := newTestService(t, pool)
	userID := insertRuntimeUser(t, pool)

	var runsBefore int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM runs`).Scan(&runsBefore))

	resp, found, err := svc.LookupRunByCreationRequest(context.Background(), userID, &runtime.RunRequest{
		AgentID:          uuid.NewString(), // deliberately absent from the Agent registry
		Input:            map[string]any{"query": "missing"},
		IdempotencyKey:   "resolve-run-replay/missing",
		CreationProtocol: "rest",
		CreationMethod:   "runs.create",
	}, "web")
	require.NoError(t, err)
	require.False(t, found)
	require.Nil(t, resp)

	var runsAfter int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM runs`).Scan(&runsAfter))
	require.Equal(t, runsBefore, runsAfter, "a replay miss must not create a Run")
}
