package runtime

import (
	"context"

	"github.com/google/uuid"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

const runtimeNodeCapacityAvailableSignal = "node.capacity.available"

type runtimeSignalCreator interface {
	CreateRuntimeSignal(context.Context, db.CreateRuntimeSignalParams) (db.RuntimeSignalOutbox, error)
}

// createRuntimeNodeCapacityAvailableSignal records a durable, payload-free
// scheduling hint in the same transaction that releases the Node slot. The
// outbox projection exposes only the Node identity; Run input and output never
// cross the signal bus.
func createRuntimeNodeCapacityAvailableSignal(
	ctx context.Context,
	creator runtimeSignalCreator,
	agentID uuid.UUID,
	nodeID uuid.UUID,
	runID uuid.UUID,
) error {
	payload, err := CanonicalizeRFC8785(map[string]any{"node_id": nodeID.String()})
	if err != nil {
		return err
	}
	_, err = creator.CreateRuntimeSignal(ctx, db.CreateRuntimeSignalParams{
		EventType: runtimeNodeCapacityAvailableSignal,
		AgentID:   agentID,
		RunID:     &runID,
		Payload:   payload,
	})
	return err
}
