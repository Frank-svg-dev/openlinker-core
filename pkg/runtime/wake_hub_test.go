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

func TestRuntimeWakeHubSeparatesDispatchAndControl(t *testing.T) {
	hub := NewRuntimeWakeHub()
	agentID := uuid.New()
	dispatch := hub.WaitDispatch(agentID)
	control := hub.WaitControl(agentID)

	hub.WakeDispatch(agentID)
	select {
	case <-dispatch:
	case <-time.After(time.Second):
		t.Fatal("dispatch wake did not reach its waiter")
	}
	select {
	case <-control:
		t.Fatal("dispatch wake reached the control waiter")
	default:
	}

	dispatch = hub.WaitDispatch(agentID)
	hub.WakeControl(agentID)
	select {
	case <-control:
	case <-time.After(time.Second):
		t.Fatal("control wake did not reach its waiter")
	}
	select {
	case <-dispatch:
		t.Fatal("control wake reached the dispatch waiter")
	default:
	}
}

func TestRuntimeWakeHubScopesCapacityReleaseToNode(t *testing.T) {
	hub := NewRuntimeWakeHub()
	agentID, nodeID, otherNodeID := uuid.New(), uuid.New(), uuid.New()
	agentDispatch := hub.WaitDispatch(agentID)
	nodeDispatch := hub.WaitNodeDispatch(nodeID)
	otherNodeDispatch := hub.WaitNodeDispatch(otherNodeID)

	hub.WakeNodeDispatch(nodeID)
	select {
	case <-nodeDispatch:
	case <-time.After(time.Second):
		t.Fatal("Node capacity wake did not reach its waiter")
	}
	select {
	case <-agentDispatch:
		t.Fatal("Node capacity wake changed Agent demand")
	default:
	}
	select {
	case <-otherNodeDispatch:
		t.Fatal("Node capacity wake reached another Node")
	default:
	}
}

func TestRuntimeSignalSubscriberReconnectBroadcastsRecoveryWake(t *testing.T) {
	instanceID, agentID := uuid.New(), uuid.New()
	bus := &runtimeSignalSubscriberFake{failures: 1}
	hub := NewRuntimeWakeHub()
	dispatch := hub.WaitDispatch(agentID)
	control := hub.WaitControl(agentID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartRuntimeSignalSubscriber(ctx, bus, instanceID, hub, nil)
	}()

	select {
	case <-dispatch:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber reconnect did not recover dispatch waiters")
	}
	select {
	case <-control:
	case <-time.After(time.Second):
		t.Fatal("subscriber reconnect did not recover control waiters")
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
	wake := hub.WaitControl(agentID)
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

func TestRuntimeAvailableSignalOnlyWakesDispatch(t *testing.T) {
	instanceID, agentID := uuid.New(), uuid.New()
	bus := &runtimeSignalSubscriberFake{
		signal: RuntimeSignal{
			SignalID: uuid.New(), Type: "run.available", AgentID: agentID,
			TargetInstanceID: &instanceID,
		},
		delivered: make(chan struct{}),
	}
	hub := NewRuntimeWakeHub()
	dispatchWake := hub.WaitDispatch(agentID)
	controlWake := hub.WaitControl(agentID)
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
	case <-dispatchWake:
	case <-time.After(time.Second):
		t.Fatal("run.available did not wake dispatch")
	}
	select {
	case <-controlWake:
		t.Fatal("run.available woke control")
	default:
	}
	cancel()
	<-done
}

func TestRuntimeNodeCapacitySignalOnlyWakesItsNode(t *testing.T) {
	instanceID, agentID, nodeID := uuid.New(), uuid.New(), uuid.New()
	bus := &runtimeSignalSubscriberFake{
		signal: RuntimeSignal{
			SignalID: uuid.New(), Type: runtimeNodeCapacityAvailableSignal,
			AgentID: agentID, NodeID: &nodeID, TargetInstanceID: &instanceID,
		},
		delivered: make(chan struct{}),
	}
	hub := NewRuntimeWakeHub()
	nodeWake := hub.WaitNodeDispatch(nodeID)
	agentWake := hub.WaitDispatch(agentID)
	controlWake := hub.WaitControl(agentID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartRuntimeSignalSubscriber(ctx, bus, instanceID, hub, nil)
	}()

	select {
	case <-nodeWake:
	case <-time.After(time.Second):
		t.Fatal("node.capacity.available did not wake its Node")
	}
	select {
	case <-agentWake:
		t.Fatal("node.capacity.available created Agent demand")
	case <-controlWake:
		t.Fatal("node.capacity.available woke control")
	default:
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

func TestRuntimeCredentialRevocationSignalIsAllowlistedAndScoped(t *testing.T) {
	instanceID := uuid.New()
	require.NoError(t, ValidateRuntimeSignal(RuntimeSignal{
		SignalID:         uuid.New(),
		Type:             "credential.revoke",
		AgentID:          uuid.New(),
		TargetInstanceID: &instanceID,
	}))
}

func TestRuntimeCredentialRevocationSignalWakesTargetCore(t *testing.T) {
	instanceID, agentID := uuid.New(), uuid.New()
	bus := &runtimeSignalSubscriberFake{signal: RuntimeSignal{
		SignalID:         uuid.New(),
		Type:             "credential.revoke",
		AgentID:          agentID,
		TargetInstanceID: &instanceID,
	}}
	hub := NewRuntimeWakeHub()
	controlWake := hub.WaitControl(agentID)
	dispatchWake := hub.WaitDispatch(agentID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartRuntimeSignalSubscriber(ctx, bus, instanceID, hub, nil)
	}()

	select {
	case <-controlWake:
	case <-time.After(time.Second):
		t.Fatal("credential revocation did not wake the target Core")
	}
	select {
	case <-dispatchWake:
		t.Fatal("credential revocation woke the dispatch waiter")
	default:
	}
	cancel()
	<-done
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
