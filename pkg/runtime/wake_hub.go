package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

const (
	runtimeSignalSubscribeRetryMin = 100 * time.Millisecond
	runtimeSignalSubscribeRetryMax = 5 * time.Second
)

var allowedRuntimeSignalTypes = map[string]struct{}{
	"run.available":     {},
	"run.cancel":        {},
	"node.drain":        {},
	"node.revoke":       {},
	"credential.revoke": {},
}

// RuntimeWakeHub broadcasts edge-triggered hints to all local HTTP Pull and
// WebSocket waiters for an Agent. Consumers always poll PostgreSQL as the
// fallback; a wake may be duplicated or missed without affecting correctness.
type RuntimeWakeHub struct {
	mu       sync.Mutex
	channels map[uuid.UUID]chan struct{}
}

func NewRuntimeWakeHub() *RuntimeWakeHub {
	return &RuntimeWakeHub{channels: make(map[uuid.UUID]chan struct{})}
}

func (h *RuntimeWakeHub) Wait(agentID uuid.UUID) <-chan struct{} {
	if h == nil || agentID == uuid.Nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channels[agentID]
	if ch == nil {
		ch = make(chan struct{})
		h.channels[agentID] = ch
	}
	return ch
}

func (h *RuntimeWakeHub) Wake(agentID uuid.UUID) {
	if h == nil || agentID == uuid.Nil {
		return
	}
	h.mu.Lock()
	ch := h.channels[agentID]
	if ch != nil {
		close(ch)
	}
	h.channels[agentID] = make(chan struct{})
	h.mu.Unlock()
}

// StartRuntimeSignalSubscriber supervises the blocking subscription and
// reconnects with bounded backoff. It deliberately does not expose a
// readiness bit: the existing signal-bus Health check fails HA readiness,
// while PostgreSQL polling and reconciliation continue to converge state.
func StartRuntimeSignalSubscriber(
	ctx context.Context,
	bus RuntimeSignalBus,
	instanceID uuid.UUID,
	hub *RuntimeWakeHub,
	service *Service,
) {
	if bus == nil || instanceID == uuid.Nil || hub == nil {
		return
	}
	delay := runtimeSignalSubscribeRetryMin
	for ctx.Err() == nil {
		err := bus.Subscribe(ctx, func(_ context.Context, signal RuntimeSignal) error {
			if signal.TargetInstanceID != nil && *signal.TargetInstanceID != instanceID {
				return nil
			}
			if _, allowed := allowedRuntimeSignalTypes[signal.Type]; !allowed {
				return ErrRuntimeSignalInvalid
			}
			hub.Wake(signal.AgentID)
			if signal.Type == "run.cancel" && signal.RunID != nil && service != nil && service.coreExecutions != nil {
				service.coreExecutions.cancelRun(*signal.RunID)
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, ErrRuntimeSignalBusClosed) {
			log.Warn().Err(err).Msg("runtime signal subscription interrupted")
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < runtimeSignalSubscribeRetryMax/2 {
			delay *= 2
		} else {
			delay = runtimeSignalSubscribeRetryMax
		}
	}
}
