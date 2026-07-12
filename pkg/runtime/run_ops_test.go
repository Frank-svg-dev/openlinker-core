package runtime

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

func TestDecodeReplayObjectRequiresOneJSONObject(t *testing.T) {
	value, ok := decodeReplayObject([]byte(`{"count":9007199254740993,"nested":{"ok":true}}`))
	require.True(t, ok)
	require.Equal(t, "9007199254740993", value["count"].(interface{ String() string }).String())

	for _, raw := range [][]byte{
		nil,
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(`{"ok":true} {"extra":true}`),
		[]byte(`{"broken"`),
	} {
		_, ok := decodeReplayObject(raw)
		require.False(t, ok, string(raw))
	}
}

func TestRuntimeDeadLetterListItemUsesOnlyRedactedEvidence(t *testing.T) {
	deadLetterID := uuid.New()
	runID := uuid.New()
	agentID := uuid.New()
	attemptID := uuid.New()
	originalID := uuid.New()
	replayID := uuid.New()
	errorCode := "RUNTIME_RETRY_EXHAUSTED"
	errorMessage := "retry budget exhausted"
	errorDetail := "redacted detail"
	reason := "redacted reason"
	now := time.Date(2026, 7, 12, 3, 4, 5, 0, time.UTC)

	item := runtimeDeadLetterListItem(db.ListRunDeadLettersRow{
		DeadLetterID:        deadLetterID,
		RunID:               runID,
		AgentID:             agentID,
		AgentSlug:           "agent-one",
		AgentName:           "Agent One",
		Status:              "failed",
		DispatchState:       "dead_letter",
		AttemptCount:        3,
		MaxAttempts:         3,
		FinalAttemptID:      &attemptID,
		FinalAttemptNo:      3,
		ErrorCode:           &errorCode,
		ErrorMessage:        &errorMessage,
		ErrorDetailRedacted: &errorDetail,
		ReasonCode:          errorCode,
		ReasonRedacted:      &reason,
		DeadLetteredAt:      &now,
		CreatedAt:           now,
		ReplayOfRunID:       &originalID,
		ReplayedAsRunIDs:    []uuid.UUID{replayID},
	})
	require.Equal(t, deadLetterID.String(), item.DeadLetterID)
	require.Equal(t, attemptID.String(), item.FinalAttemptID)
	require.Equal(t, originalID.String(), item.ReplayOfRunID)
	require.Equal(t, []string{replayID.String()}, item.ReplayedAsRunIDs)
	require.Equal(t, errorDetail, item.ErrorDetail)
	require.Equal(t, reason, item.Reason)
}
