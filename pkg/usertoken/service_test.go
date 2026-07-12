package usertoken

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/OpenLinker-ai/openlinker-core/pkg/auth"
	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

func TestNormalizeGrantsAllowsZeroAndRejectsRemovedTaskScopes(t *testing.T) {
	grants, err := normalizeGrantRequests(nil)
	if err != nil || len(grants) != 0 {
		t.Fatalf("zero grants = %#v, %v", grants, err)
	}
	grants, err = grantsFromLegacyScopes([]string{"tasks:create", "agents:run"})
	if err != nil {
		t.Fatal(err)
	}
	permissions := permissionsFromGrants(grants)
	if len(permissions) != 2 || permissions[0] != "agents:run" || permissions[1] != "tasks:create" {
		t.Fatalf("permissions = %#v", permissions)
	}
	if _, err := grantsFromLegacyScopes([]string{"tasks:write"}); err == nil {
		t.Fatal("removed task write permission must be rejected")
	}
	if _, err := normalizeGrantRequests([]GrantRequest{{Permission: "tasks:publish"}}); err == nil {
		t.Fatal("removed public task marketplace permission must be rejected")
	}
	if _, err := normalizeGrantRequests([]GrantRequest{{Permission: "tasks:work"}}); err == nil {
		t.Fatal("removed task worker permission must be rejected")
	}
	if _, err := normalizeGrantRequests([]GrantRequest{{Permission: "tasks:review"}}); err == nil {
		t.Fatal("removed task review permission must be rejected")
	}
}

func TestNormalizeGrantsRejectsCloudAndUnsupportedResourceScope(t *testing.T) {
	if _, err := normalizeGrantRequests([]GrantRequest{{Permission: "cloud:usage:read"}}); err == nil {
		t.Fatal("Core must reject Cloud permissions")
	}
	resourceID := uuid.NewString()
	if _, err := normalizeGrantRequests([]GrantRequest{{Permission: "tasks:read", ResourceID: &resourceID}}); err == nil {
		t.Fatal("tasks:read resource IDs are not enabled in the first release")
	}
}

func TestGrantShrinkRejectsExpansionAndAllowsWildcardToSpecific(t *testing.T) {
	agentA := uuid.New()
	agentB := uuid.New()
	wildcard := []auth.Grant{{Permission: "agents:run", ResourceType: "agent"}}
	specificA := []auth.Grant{{Permission: "agents:run", ResourceType: "agent", ResourceID: &agentA}}
	specificB := []auth.Grant{{Permission: "agents:run", ResourceType: "agent", ResourceID: &agentB}}
	if !isGrantShrink(wildcard, specificA) {
		t.Fatal("wildcard to one resource is a shrink")
	}
	if isGrantShrink(specificA, wildcard) || isGrantShrink(specificA, specificB) {
		t.Fatal("specific to wildcard/another resource is expansion")
	}
	if !isGrantShrink(specificA, nil) {
		t.Fatal("deleting all grants is a valid shrink")
	}
}

func TestExpansionConflictUsesStableCode(t *testing.T) {
	err := expansionConflict("TOKEN_PERMISSION_EXPANSION")
	if err.Status != 409 || err.Code != httpx.CodePermissionExpansionRequiresNewToken {
		t.Fatalf("expansion error = %#v", err)
	}
	details, _ := err.Details.(map[string]any)
	if details["replacement_required"] != true {
		t.Fatalf("details = %#v", details)
	}
}

func TestInvalidTokenSentinelIsStable(t *testing.T) {
	if !errors.Is(ErrInvalidToken, ErrInvalidToken) {
		t.Fatal("invalid token sentinel")
	}
}
