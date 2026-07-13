package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

func TestRuntimeDeadlineReconcilerRejectsInvalidConfigurationAndBatch(t *testing.T) {
	_, err := (*RuntimeDeadlineReconciler)(nil).ReconcileBatch(t.Context(), 1)
	require.ErrorIs(t, err, ErrRuntimeReconcilerNotConfigured)

	reconciler := &RuntimeDeadlineReconciler{retryPlanner: fixedResultRetryPlanner{}}
	_, err = reconciler.ReconcileBatch(t.Context(), 1)
	require.ErrorIs(t, err, ErrRuntimeReconcilerNotConfigured)

	configured := &RuntimeDeadlineReconciler{pool: nil, retryPlanner: fixedResultRetryPlanner{}}
	_, err = configured.ReconcileBatch(t.Context(), 0)
	require.True(t, errors.Is(err, ErrRuntimeReconcilerNotConfigured))
}

func TestRuntimeReconcileTerminalPrecedence(t *testing.T) {
	now := time.Date(2026, 7, 11, 20, 0, 0, 0, time.UTC)
	base := db.RuntimeReconcileLockedRunRow{
		ID: uuid.New(), DatabaseNow: now,
		DispatchDeadlineAt: now.Add(time.Minute), RunDeadlineAt: now.Add(2 * time.Minute),
		OfferCount: 1, MaxOfferCount: 2, AttemptCount: 1, MaxAttempts: 3,
	}

	require.Nil(t, runtimeDeadlineTerminalForRun(base))
	require.Nil(t, runtimeOfferTerminalForRun(base, db.RunAttempt{OfferExpiresAt: now}))
	require.Nil(t, runtimeExecutionTerminalForRun(base))

	dispatchDue := base
	dispatchDue.DispatchDeadlineAt = now
	terminal := runtimeExecutionTerminalForRun(dispatchDue)
	require.NotNil(t, terminal)
	require.Equal(t, "timeout", terminal.status)
	require.Equal(t, "RUNTIME_DISPATCH_TIMEOUT", terminal.errorCode)

	runDue := dispatchDue
	runDue.RunDeadlineAt = now
	terminal = runtimeExecutionTerminalForRun(runDue)
	require.NotNil(t, terminal)
	require.Equal(t, "RUN_DEADLINE_EXCEEDED", terminal.errorCode)

	exhausted := base
	exhausted.AttemptCount = exhausted.MaxAttempts
	terminal = runtimeExecutionTerminalForRun(exhausted)
	require.NotNil(t, terminal)
	require.Equal(t, "failed", terminal.status)
	require.Equal(t, "dead_letter", terminal.dispatchState)
	require.Equal(t, RuntimeResultClassificationDeadLetter, terminal.classification)

	offerExhausted := base
	offerExhausted.OfferCount = offerExhausted.MaxOfferCount
	terminal = runtimeOfferTerminalForRun(offerExhausted, db.RunAttempt{OfferExpiresAt: now})
	require.NotNil(t, terminal)
	require.Equal(t, "RUNTIME_DISPATCH_TIMEOUT", terminal.errorCode)
}
