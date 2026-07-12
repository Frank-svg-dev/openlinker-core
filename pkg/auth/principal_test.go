package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

func TestAuthPrincipalAllowsWildcardAndSpecificResources(t *testing.T) {
	agentA := uuid.New()
	agentB := uuid.New()
	principal := &AuthPrincipal{
		UserID: uuid.New(), AuthMethod: AuthMethodUserToken,
		Grants: []Grant{
			{Permission: "runs:read", ResourceType: "run", Constraints: json.RawMessage(`{}`)},
			{Permission: "agents:run", ResourceType: "agent", ResourceID: &agentA, Constraints: json.RawMessage(`{}`)},
		},
	}
	if !principal.Allows("runs:read", "run", nil) {
		t.Fatal("wildcard grant should allow the permission")
	}
	if !principal.Allows("agents:run", "agent", &agentA) {
		t.Fatal("specific grant should allow the same Agent")
	}
	if principal.Allows("agents:run", "agent", nil) || principal.Allows("agents:run", "agent", &agentB) {
		t.Fatal("specific grant must not become wildcard or cover another Agent")
	}
	jwt := &AuthPrincipal{UserID: uuid.New(), AuthMethod: AuthMethodJWT}
	if !jwt.Allows("anything", "resource", &agentB) {
		t.Fatal("JWT behavior must remain first-party/unrestricted at the token evaluator")
	}
}

func TestRequirePermissionReturnsStableResourceDetails(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/run", nil), httptest.NewRecorder())
	agentID := uuid.New()
	SetPrincipal(c, &AuthPrincipal{UserID: uuid.New(), AuthMethod: AuthMethodUserToken, Grants: []Grant{}})

	err := RequirePermission(c, "agents:run", "agent", &agentID)
	var httpErr *httpx.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if httpErr.Code != httpx.CodePermissionDenied {
		t.Fatalf("code = %s", httpErr.Code)
	}
	details, _ := httpErr.Details.(map[string]any)
	if details["required_resource_type"] != "agent" || details["required_resource_id"] != agentID.String() {
		t.Fatalf("details = %#v", details)
	}
}

func TestRequirePermissionRejectsRemovedTaskWriteScope(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/tasks", nil), httptest.NewRecorder())
	c.Set(string(httpx.CtxKeyAuthMethod), AuthMethodUserToken)
	c.Set(string(httpx.CtxKeyAuthScopes), []string{"tasks:write"})

	err := RequirePermission(c, "tasks:create", "task", nil)
	var httpErr *httpx.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if httpErr.Code != httpx.CodePermissionDenied {
		t.Fatalf("code = %s", httpErr.Code)
	}
}

func TestCorePermissionCatalogHasAllowAndDenyCoverage(t *testing.T) {
	catalog := map[string]string{
		"agents:read": "agent", "agents:run": "agent", "agents:create": "agent",
		"runs:read": "run", "runs:cancel": "run",
		"tasks:read": "task", "tasks:create": "task", "tasks:run": "task",
		"workflows:read": "workflow", "workflows:manage": "workflow", "workflows:run": "workflow",
		"agent-tokens:read": "agent", "agent-tokens:issue": "agent", "agent-tokens:revoke": "agent",
	}
	for permission, resourceType := range catalog {
		t.Run(permission, func(t *testing.T) {
			allowed := &AuthPrincipal{UserID: uuid.New(), AuthMethod: AuthMethodUserToken, Grants: []Grant{{Permission: permission, ResourceType: resourceType}}}
			denied := &AuthPrincipal{UserID: uuid.New(), AuthMethod: AuthMethodUserToken, Grants: []Grant{}}
			if !allowed.Allows(permission, resourceType, nil) {
				t.Fatalf("%s should allow matching wildcard grant", permission)
			}
			if denied.Allows(permission, resourceType, nil) {
				t.Fatalf("%s should deny without grant", permission)
			}
		})
	}
}
