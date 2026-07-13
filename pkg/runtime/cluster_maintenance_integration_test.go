package runtime_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

func TestRuntimeClusterMaintenanceAllowsCommittedReplayButRejectsNewRun(t *testing.T) {
	pool := setupTestDB(t)
	svc := newTestService(t, pool)
	userID := insertRuntimeUser(t, pool)
	creatorID := insertCreator(t, pool)
	agentID := insertAgent(t, pool, creatorID, "https://runtime.invalid/queued", 0, "approved")
	_, err := pool.Exec(context.Background(), `
UPDATE agents
SET connection_mode = 'runtime',
    endpoint_url = 'openlinker-runtime://' || id::text
WHERE id = $1`, agentID)
	require.NoError(t, err)

	for _, mode := range []string{"draining", "hard_maintenance"} {
		t.Run(mode, func(t *testing.T) {
			setRuntimeClusterMode(t, pool, "normal")
			key := "maintenance-replay/" + mode
			request := &runtime.RunRequest{
				AgentID:          agentID.String(),
				Input:            map[string]any{"prompt": "finish accepted work"},
				IdempotencyKey:   key,
				CreationProtocol: "rest",
				CreationMethod:   "runs.create",
			}
			created, createErr := svc.StartRun(context.Background(), userID, request, "api")
			require.NoError(t, createErr)
			require.NotEmpty(t, created.RunID)

			var before int
			require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE user_id = $1`, userID).Scan(&before))
			setRuntimeClusterMode(t, pool, mode)

			replay, replayErr := svc.StartRun(context.Background(), userID, request, "api")
			require.NoError(t, replayErr)
			require.Equal(t, created.RunID, replay.RunID)
			require.True(t, replay.Replayed)

			newRequest := *request
			newRequest.IdempotencyKey = key + "/new-intent"
			_, newErr := svc.StartRun(context.Background(), userID, &newRequest, "api")
			var httpErr *httpx.HTTPError
			require.ErrorAs(t, newErr, &httpErr)
			require.Equal(t, http.StatusServiceUnavailable, httpErr.Status)
			require.Equal(t, httpx.CodeServiceUnavailable, httpErr.Code)

			var after int
			require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE user_id = $1`, userID).Scan(&after))
			require.Equal(t, before, after, "maintenance gate must not insert a Run")
		})
	}
}
