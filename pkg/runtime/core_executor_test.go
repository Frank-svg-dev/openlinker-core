package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

func TestCoreAttemptRegistrySignalCancelsImmediately(t *testing.T) {
	registry := newCoreAttemptRegistry(&coreCancellationPollFake{}, time.Second)
	execution := testCoreAttemptExecution()
	callCtx, unregister := registry.register(context.Background(), execution)
	registry.cancelRun(execution.identity.RunID)

	select {
	case <-callCtx.Done():
		require.ErrorIs(t, context.Cause(callCtx), errCoreAttemptOwnerCanceled)
	case <-time.After(time.Second):
		t.Fatal("local run.cancel signal did not cancel the Core attempt")
	}
	unregister()
}

func TestCoreAttemptRegistryFallsBackToDatabasePoll(t *testing.T) {
	queries := &coreCancellationPollFake{}
	registry := newCoreAttemptRegistry(queries, 10*time.Millisecond)
	execution := testCoreAttemptExecution()
	callCtx, unregister := registry.register(context.Background(), execution)
	queries.requested.Store(true)

	select {
	case <-callCtx.Done():
		require.ErrorIs(t, context.Cause(callCtx), errCoreAttemptOwnerCanceled)
	case <-time.After(time.Second):
		t.Fatal("database cancellation poll did not stop the Core attempt")
	}
	require.Greater(t, queries.calls.Load(), int32(0))
	unregister()
}

func TestCoreAttemptRegistryCapsCancellationPollAtTwoSeconds(t *testing.T) {
	registry := newCoreAttemptRegistry(&coreCancellationPollFake{}, time.Hour)
	require.Equal(t, defaultCoreCancellationPollInterval, registry.pollEvery)
}

func testCoreAttemptExecution() coreAttemptExecution {
	return coreAttemptExecution{
		identity: RuntimeAttemptIdentity{
			RunID: uuid.New(), AttemptID: uuid.New(), LeaseID: uuid.New(),
			FencingToken: 1, AgentID: uuid.New(),
		},
		attemptNo:  1,
		deadlineAt: time.Now().Add(time.Minute),
	}
}

type coreCancellationPollFake struct {
	requested atomic.Bool
	calls     atomic.Int32
}

func (f *coreCancellationPollFake) CoreAttemptCancellationRequested(
	context.Context,
	db.CoreAttemptCancellationRequestedParams,
) (bool, error) {
	f.calls.Add(1)
	return f.requested.Load(), nil
}
