package db

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunReplaySourceLockAndDeadLetterInventoryQueries(t *testing.T) {
	userID := uuid.New()
	runID := uuid.New()
	agentID := uuid.New()
	attemptID := uuid.New()
	deadLetterID := uuid.New()
	replayID := uuid.New()
	now := time.Date(2026, 7, 12, 3, 4, 5, 0, time.UTC)
	errorCode := "RUNTIME_RETRY_EXHAUSTED"
	errorMessage := "retry budget exhausted"
	errorDetail := "upstream unavailable"
	reason := "temporary upstream failures"

	dbtx := &fakeDBTX{row: fakeRow{values: []any{runID, true}}}
	q := New(dbtx)
	locked, err := q.LockReplaySourceForCreate(context.Background(), LockReplaySourceForCreateParams{
		ID:     runID,
		UserID: userID,
	})
	if err != nil || locked.ID != runID || !locked.InputAvailable {
		t.Fatalf("LockReplaySourceForCreate = %#v, %v", locked, err)
	}
	requireSQLName(t, dbtx.queryRowSQL, "LockReplaySourceForCreate")
	if !reflect.DeepEqual(dbtx.queryRowArgs, []any{runID, userID}) {
		t.Fatalf("LockReplaySourceForCreate args = %#v", dbtx.queryRowArgs)
	}

	input := []byte(`{"prompt":"retry"}`)
	metadata := []byte(`{"trace_id":"trace-1"}`)
	dbtx.row = fakeRow{values: []any{input, metadata}}
	payload, err := q.GetRunReplayPayload(context.Background(), runID)
	if err != nil || !reflect.DeepEqual(payload.Input, input) ||
		!reflect.DeepEqual(payload.RequestMetadata, metadata) {
		t.Fatalf("GetRunReplayPayload = %#v, %v", payload, err)
	}
	requireSQLName(t, dbtx.queryRowSQL, "GetRunReplayPayload")
	if !reflect.DeepEqual(dbtx.queryRowArgs, []any{runID}) {
		t.Fatalf("GetRunReplayPayload args = %#v", dbtx.queryRowArgs)
	}

	rows := &fakeRows{rows: [][]any{{
		deadLetterID,
		runID,
		agentID,
		"agent-one",
		"Agent One",
		"failed",
		"dead_letter",
		int32(3),
		int32(3),
		&attemptID,
		int32(3),
		&errorCode,
		&errorMessage,
		&errorDetail,
		errorCode,
		&reason,
		&now,
		now,
		(*uuid.UUID)(nil),
		[]uuid.UUID{replayID},
	}}}
	dbtx.queryRows = rows
	listed, err := q.ListRunDeadLetters(context.Background(), ListRunDeadLettersParams{Limit: 25, Offset: 5})
	if err != nil {
		t.Fatalf("ListRunDeadLetters error = %v", err)
	}
	requireSQLName(t, dbtx.querySQL, "ListRunDeadLetters")
	if !rows.closed || len(listed) != 1 || listed[0].RunID != runID ||
		listed[0].FinalAttemptID == nil || *listed[0].FinalAttemptID != attemptID ||
		len(listed[0].ReplayedAsRunIDs) != 1 || listed[0].ReplayedAsRunIDs[0] != replayID {
		t.Fatalf("ListRunDeadLetters scan = %#v closed=%v", listed, rows.closed)
	}
	if !reflect.DeepEqual(dbtx.queryArgs, []any{int32(25), int32(5)}) {
		t.Fatalf("ListRunDeadLetters args = %#v", dbtx.queryArgs)
	}
	for _, required := range []string{
		"final_attempt.error_detail_redacted",
		"replay.replay_of_run_id = r.id",
		"ORDER BY dlq.created_at DESC, dlq.id DESC",
	} {
		if !strings.Contains(dbtx.querySQL, required) {
			t.Fatalf("ListRunDeadLetters SQL missing %q: %s", required, dbtx.querySQL)
		}
	}
}
