package runtime

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

const runtimePresenceTTL = RuntimePresenceTTL

func (h *RuntimeHTTPController) refreshPresence(
	ctx context.Context,
	state RuntimeSessionState,
	connectionID string,
) {
	if h == nil || h.dependencies.Presence == nil || h.dependencies.CoreInstanceID == uuid.Nil {
		return
	}
	session := state.Session
	attachment := state.Attachment
	if session.AttachedCoreInstanceID == nil ||
		*session.AttachedCoreInstanceID != h.dependencies.CoreInstanceID ||
		(session.Status != "active" && session.Status != "draining") ||
		attachment == nil || attachment.TransportReason == nil {
		return
	}
	presence := RuntimePresence{
		CoreInstanceID: h.dependencies.CoreInstanceID,
		NodeID:         session.NodeID, AgentID: session.AgentID,
		RuntimeSessionID: session.RuntimeSessionID,
		ConnectionID:     connectionID, WorkerID: session.WorkerID,
		Capacity: session.Capacity, Inflight: session.Inflight,
		NodeVersion:        session.NodeVersion,
		Transport:          RuntimeTransport(attachment.Transport),
		TransportReason:    RuntimeTransportReason(*attachment.TransportReason),
		TransportChangedAt: attachment.TransportChangedAt,
	}
	if err := h.dependencies.Presence.Refresh(ctx, presence, runtimePresenceTTL); err != nil {
		log.Warn().Err(err).Str("runtime_session_id", session.RuntimeSessionID.String()).
			Msg("runtime advisory presence refresh failed")
	}
}

func (h *RuntimeHTTPController) removePresence(
	ctx context.Context,
	state RuntimeSessionState,
	connectionID string,
) {
	if h == nil || h.dependencies.Presence == nil || h.dependencies.CoreInstanceID == uuid.Nil {
		return
	}
	session := state.Session
	attachment := state.Attachment
	if attachment == nil || attachment.TransportReason == nil {
		return
	}
	presence := RuntimePresence{
		CoreInstanceID: h.dependencies.CoreInstanceID,
		NodeID:         session.NodeID, AgentID: session.AgentID,
		RuntimeSessionID: session.RuntimeSessionID,
		ConnectionID:     connectionID, WorkerID: session.WorkerID,
		Capacity: session.Capacity, Inflight: session.Inflight,
		NodeVersion:        session.NodeVersion,
		Transport:          RuntimeTransport(attachment.Transport),
		TransportReason:    RuntimeTransportReason(*attachment.TransportReason),
		TransportChangedAt: attachment.TransportChangedAt,
	}
	if err := h.dependencies.Presence.Remove(ctx, presence); err != nil {
		log.Warn().Err(err).Str("runtime_session_id", session.RuntimeSessionID.String()).
			Msg("runtime advisory presence removal failed")
	}
}
