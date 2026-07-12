package runtime

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

const runtimePresenceTTL = 60 * time.Second

func (h *RuntimeV2HTTPController) refreshPresence(
	ctx context.Context,
	state RuntimeSessionState,
	connectionID string,
) {
	if h == nil || h.dependencies.Presence == nil || h.dependencies.CoreInstanceID == uuid.Nil {
		return
	}
	session := state.Session
	if session.AttachedCoreInstanceID == nil ||
		*session.AttachedCoreInstanceID != h.dependencies.CoreInstanceID ||
		(session.Status != "active" && session.Status != "draining") {
		return
	}
	presence := RuntimePresence{
		CoreInstanceID: h.dependencies.CoreInstanceID,
		NodeID:         session.NodeID, AgentID: session.AgentID,
		RuntimeSessionID: session.RuntimeSessionID,
		ConnectionID:     connectionID, WorkerID: session.WorkerID,
		Capacity: session.Capacity, Inflight: session.Inflight,
		NodeVersion: session.NodeVersion,
	}
	if err := h.dependencies.Presence.Refresh(ctx, presence, runtimePresenceTTL); err != nil {
		log.Warn().Err(err).Str("runtime_session_id", session.RuntimeSessionID.String()).
			Msg("runtime advisory presence refresh failed")
	}
}

func (h *RuntimeV2HTTPController) removePresence(
	ctx context.Context,
	state RuntimeSessionState,
	connectionID string,
) {
	if h == nil || h.dependencies.Presence == nil || h.dependencies.CoreInstanceID == uuid.Nil {
		return
	}
	session := state.Session
	presence := RuntimePresence{
		CoreInstanceID: h.dependencies.CoreInstanceID,
		NodeID:         session.NodeID, AgentID: session.AgentID,
		RuntimeSessionID: session.RuntimeSessionID,
		ConnectionID:     connectionID, WorkerID: session.WorkerID,
		Capacity: session.Capacity, Inflight: session.Inflight,
		NodeVersion: session.NodeVersion,
	}
	if err := h.dependencies.Presence.Remove(ctx, presence); err != nil {
		log.Warn().Err(err).Str("runtime_session_id", session.RuntimeSessionID.String()).
			Msg("runtime advisory presence removal failed")
	}
}
