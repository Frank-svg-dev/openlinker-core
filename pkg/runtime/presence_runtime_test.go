package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

func TestRuntimeV2ControllerPresenceFollowsAttachHeartbeatAndClose(t *testing.T) {
	coreID := uuid.New()
	store := &runtimePresenceStoreFake{}
	controller := NewRuntimeHTTPController(RuntimeHTTPDependencies{
		Presence: store, CoreInstanceID: coreID,
	})
	state := RuntimeSessionState{Session: db.RuntimeSession{
		RuntimeSessionID: uuid.New(), NodeID: uuid.New(), AgentID: uuid.New(),
		WorkerID: "worker-1", Capacity: 4, Inflight: 1, NodeVersion: "2.0.0",
		Status: "active", AttachedCoreInstanceID: &coreID,
	}}

	controller.refreshPresence(context.Background(), state, "pull:session")
	state.Session.Inflight = 2
	controller.refreshPresence(context.Background(), state, "pull:session")
	controller.removePresence(context.Background(), state, "pull:session")

	require.Len(t, store.refreshes, 2)
	require.Len(t, store.removals, 1)
	require.Equal(t, runtimePresenceTTL, store.ttls[0])
	require.Equal(t, int32(2), store.refreshes[1].Inflight)
	require.Equal(t, "pull:session", store.removals[0].ConnectionID)

	otherCore := uuid.New()
	state.Session.AttachedCoreInstanceID = &otherCore
	controller.refreshPresence(context.Background(), state, "pull:other")
	require.Len(t, store.refreshes, 2, "presence attached to another Core is never advertised locally")
}

type runtimePresenceStoreFake struct {
	refreshes []RuntimePresence
	removals  []RuntimePresence
	ttls      []time.Duration
}

func (f *runtimePresenceStoreFake) Refresh(_ context.Context, presence RuntimePresence, ttl time.Duration) error {
	f.refreshes = append(f.refreshes, presence)
	f.ttls = append(f.ttls, ttl)
	return nil
}

func (f *runtimePresenceStoreFake) ListByAgent(context.Context, uuid.UUID) ([]RuntimePresence, error) {
	return nil, nil
}

func (f *runtimePresenceStoreFake) Remove(_ context.Context, presence RuntimePresence) error {
	f.removals = append(f.removals, presence)
	return nil
}
