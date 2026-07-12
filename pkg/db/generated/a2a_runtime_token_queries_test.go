package db

import (
	"strings"
	"testing"
)

func TestRuntimeTokenReadsAndTouchesRejectExpiredCredentials(t *testing.T) {
	queries := map[string]string{
		"auth lookup": listActiveAgentRuntimeTokensByPrefix,
		"touch":       touchAgentRuntimeToken,
	}
	for name, query := range queries {
		if !strings.Contains(query, "expires_at IS NULL OR") ||
			!strings.Contains(query, "expires_at > clock_timestamp()") {
			t.Fatalf("%s query does not fail closed for expired credentials:\n%s", name, query)
		}
	}
	if !strings.Contains(touchAgentRuntimeToken, "status = 'active_runtime'") ||
		!strings.Contains(touchAgentRuntimeToken, "revoked_at IS NULL") {
		t.Fatalf("token touch can revive an inactive credential:\n%s", touchAgentRuntimeToken)
	}
}
