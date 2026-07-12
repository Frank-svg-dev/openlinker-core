package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRuntimeWakeHubBroadcastsAndRearms(t *testing.T) {
	hub := NewRuntimeWakeHub()
	agentID := uuid.New()
	first := hub.Wait(agentID)
	require.Equal(t, first, hub.Wait(agentID))

	hub.Wake(agentID)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("wake did not reach the registered waiter")
	}

	second := hub.Wait(agentID)
	require.NotEqual(t, first, second)
	select {
	case <-second:
		t.Fatal("replacement wake channel was already closed")
	default:
	}
}

func TestRuntimeSignalSubscriberReconnectsAndWakesAfterTransportFailure(t *testing.T) {
	instanceID, agentID := uuid.New(), uuid.New()
	bus := &runtimeSignalSubscriberFake{
		failures: 1,
		signal: RuntimeSignal{
			SignalID: uuid.New(), Type: "run.available", AgentID: agentID,
			TargetInstanceID: &instanceID,
		},
	}
	hub := NewRuntimeWakeHub()
	wake := hub.Wait(agentID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartRuntimeSignalSubscriber(ctx, bus, instanceID, hub, nil)
	}()

	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not reconnect and deliver the wake hint")
	}
	require.GreaterOrEqual(t, bus.subscribeCalls(), 2)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not stop with its context")
	}
}

func TestRuntimeSignalSubscriberRejectsAnotherCoreTarget(t *testing.T) {
	instanceID, otherInstanceID, agentID := uuid.New(), uuid.New(), uuid.New()
	bus := &runtimeSignalSubscriberFake{
		signal: RuntimeSignal{
			SignalID: uuid.New(), Type: "run.cancel", AgentID: agentID,
			TargetInstanceID: &otherInstanceID,
		},
		delivered: make(chan struct{}),
	}
	hub := NewRuntimeWakeHub()
	wake := hub.Wait(agentID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartRuntimeSignalSubscriber(ctx, bus, instanceID, hub, nil)
	}()

	select {
	case <-bus.delivered:
	case <-time.After(time.Second):
		t.Fatal("fake subscription was not reached")
	}
	select {
	case <-wake:
		t.Fatal("signal targeted at another Core woke the local waiter")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestRuntimeSignalTypeMustBeExplicitlyAllowlisted(t *testing.T) {
	err := ValidateRuntimeSignal(RuntimeSignal{
		SignalID: uuid.New(), Type: "run.payload", AgentID: uuid.New(),
	})
	require.ErrorIs(t, err, ErrRuntimeSignalInvalid)
}

type runtimeSignalSubscriberFake struct {
	mu        sync.Mutex
	calls     int
	failures  int
	signal    RuntimeSignal
	delivered chan struct{}
}

func (f *runtimeSignalSubscriberFake) Publish(context.Context, RuntimeSignal) error { return nil }
func (f *runtimeSignalSubscriberFake) Health(context.Context) error                 { return nil }
func (f *runtimeSignalSubscriberFake) Close() error                                 { return nil }

func (f *runtimeSignalSubscriberFake) Subscribe(ctx context.Context, handler RuntimeSignalHandler) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call <= f.failures {
		return errors.New("signal transport unavailable")
	}
	if f.signal.SignalID != uuid.Nil {
		if err := handler(ctx, f.signal); err != nil {
			return err
		}
		if f.delivered != nil {
			close(f.delivered)
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *runtimeSignalSubscriberFake) subscribeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
