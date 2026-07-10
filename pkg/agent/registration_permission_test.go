package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/OpenLinker-ai/openlinker-core/pkg/auth"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

func TestPendingAgentTokenRequiresIssueAndAgentsCreate(t *testing.T) {
	userID := uuid.New()
	for _, tc := range []struct {
		name     string
		grants   []auth.Grant
		wantCode httpx.ErrorCode
		wantCall bool
	}{
		{
			name:     "issue alone denied",
			grants:   []auth.Grant{{Permission: "agent-tokens:issue", ResourceType: "agent", Constraints: json.RawMessage(`{}`)}},
			wantCode: httpx.CodePermissionDenied,
		},
		{
			name: "issue plus create allowed",
			grants: []auth.Grant{
				{Permission: "agent-tokens:issue", ResourceType: "agent", Constraints: json.RawMessage(`{}`)},
				{Permission: "agents:create", ResourceType: "agent", Constraints: json.RawMessage(`{}`)},
			},
			wantCall: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &mockRegistrationService{}
			h := NewRegistrationHandler(svc)
			c, _ := newAgentDispatchContext(agentDispatchRequest{
				method: http.MethodPost, target: "/creator/agent-tokens",
				userID: userID.String(), body: `{"name":"bootstrap"}`,
			})
			auth.SetPrincipal(c, &auth.AuthPrincipal{UserID: userID, AuthMethod: auth.AuthMethodUserToken, Grants: tc.grants})
			err := h.CreateAgentToken(c)
			if tc.wantCode != "" {
				var httpErr *httpx.HTTPError
				if !errors.As(err, &httpErr) || httpErr.Code != tc.wantCode {
					t.Fatalf("error = %#v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			called := false
			for _, call := range svc.calls {
				called = called || call == "CreateAgentToken"
			}
			if called != tc.wantCall {
				t.Fatalf("CreateAgentToken called = %v", called)
			}
		})
	}
}

func TestAgentTokenIssueResourceGrantCannotCrossAgent(t *testing.T) {
	userID := uuid.New()
	agentA := uuid.New()
	agentB := uuid.New()
	principal := &auth.AuthPrincipal{
		UserID: userID, AuthMethod: auth.AuthMethodUserToken,
		Grants: []auth.Grant{{Permission: "agent-tokens:issue", ResourceType: "agent", ResourceID: &agentA, Constraints: json.RawMessage(`{}`)}},
	}
	h := NewRegistrationHandler(&mockRegistrationService{})
	c, _ := newAgentDispatchContext(agentDispatchRequest{
		method: http.MethodPost, target: "/creator/agent-tokens",
		userID: userID.String(), body: `{"name":"rotate","agent_id":"` + agentB.String() + `"}`,
	})
	auth.SetPrincipal(c, principal)
	err := h.CreateAgentToken(c)
	var httpErr *httpx.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodePermissionDenied {
		t.Fatalf("cross-Agent error = %#v", err)
	}
}
