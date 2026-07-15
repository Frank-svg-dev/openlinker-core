package servicebridge

import (
	"os"
	"strings"
	"testing"
)

func TestHostedServiceContractPinMigrationInvariants(t *testing.T) {
	up, err := os.ReadFile("../../migrations/072_hosted_service_contract_pin.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	down, err := os.ReadFile("../../migrations/072_hosted_service_contract_pin.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	for _, fragment := range []string{"expected_contract_hash TEXT", "input_schema_fingerprint BYTEA", "^hct:v1:[a-f0-9]{64}$", "octet_length(input_schema_fingerprint) = 32"} {
		if !strings.Contains(string(up), fragment) {
			t.Fatalf("up migration missing %q", fragment)
		}
	}
	for _, fragment := range []string{"DROP COLUMN IF EXISTS input_schema_fingerprint", "DROP COLUMN IF EXISTS expected_contract_hash"} {
		if !strings.Contains(string(down), fragment) {
			t.Fatalf("down migration missing %q", fragment)
		}
	}
}
