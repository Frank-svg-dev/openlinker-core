package workflow

import (
	"testing"

	"github.com/google/uuid"

	db "github.com/OpenLinker-ai/openlinker-core/pkg/db/generated"
)

func TestExternalExecutionWorkflowRunMatchesSemanticIdentity(t *testing.T) {
	workflowID, actorUserID := uuid.New(), uuid.New()
	run := db.WorkflowRun{
		WorkflowID:  workflowID,
		UserID:      actorUserID,
		Input:       []byte(`{"count":2,"topic":"Go"}`),
		MaxAttempts: defaultWorkflowRunMaxAttempts,
	}
	if !externalExecutionWorkflowRunMatches(run, workflowID, actorUserID, map[string]interface{}{"topic": "Go", "count": 2}, defaultWorkflowRunMaxAttempts) {
		t.Fatalf("same semantic input should match regardless of key order or Go number type")
	}
	if externalExecutionWorkflowRunMatches(run, workflowID, uuid.New(), map[string]interface{}{"topic": "Go", "count": 2}, defaultWorkflowRunMaxAttempts) {
		t.Fatalf("different actor must not reuse external execution workflow run")
	}
	if externalExecutionWorkflowRunMatches(run, workflowID, actorUserID, map[string]interface{}{"topic": "Rust", "count": 2}, defaultWorkflowRunMaxAttempts) {
		t.Fatalf("different input must not reuse external execution workflow run")
	}
}

func TestExternalExecutionWorkflowRunIDIsCallerIsolated(t *testing.T) {
	requestID := uuid.New()
	first := externalExecutionWorkflowRunID("openlinker-cloud", requestID)
	if first == uuid.Nil || first != externalExecutionWorkflowRunID("openlinker-cloud", requestID) {
		t.Fatalf("run id must be deterministic and non-zero: %s", first)
	}
	if first == externalExecutionWorkflowRunID("another-service", requestID) {
		t.Fatal("same request id from different verified callers must not collide")
	}
}
