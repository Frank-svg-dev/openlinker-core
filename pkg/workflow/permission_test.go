package workflow

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/OpenLinker-ai/openlinker-core/pkg/auth"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

func TestCompareRunsChecksBothWorkflowResourceGrants(t *testing.T) {
	userID := uuid.New()
	baseRunID := uuid.New()
	otherRunID := uuid.New()
	workflowA := uuid.New()
	workflowB := uuid.New()
	svc := &mockWorkflowService{runResponses: map[uuid.UUID]*WorkflowRunResponse{
		baseRunID:  {ID: baseRunID.String(), WorkflowID: workflowA.String()},
		otherRunID: {ID: otherRunID.String(), WorkflowID: workflowB.String()},
	}}
	h := NewHandler(svc)
	c, _ := newWorkflowDispatchContext(&workflowDispatchRequest{
		method: http.MethodGet, target: "/workflow-runs/compare",
		userID: userID.String(), params: map[string]string{"id": baseRunID.String(), "other_id": otherRunID.String()},
	})
	auth.SetPrincipal(c, &auth.AuthPrincipal{
		UserID: userID, AuthMethod: auth.AuthMethodUserToken,
		Grants: []auth.Grant{{Permission: "workflows:read", ResourceType: "workflow", ResourceID: &workflowA, Constraints: json.RawMessage(`{}`)}},
	})
	err := h.CompareRuns(c)
	var httpErr *httpx.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodePermissionDenied {
		t.Fatalf("cross-workflow compare error = %#v", err)
	}
	if svc.compareBaseRunID != uuid.Nil || svc.compareCandidateRunID != uuid.Nil {
		t.Fatal("compare service must not run when candidate Workflow is outside the grant")
	}
}
