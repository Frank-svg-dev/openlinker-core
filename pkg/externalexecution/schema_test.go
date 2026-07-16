package externalexecution

import (
	"encoding/json"
	"testing"
)

func TestExternalInputSchemaCompatible(t *testing.T) {
	capability := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"topic": map[string]interface{}{"type": "string"},
			"count": map[string]interface{}{"type": "number"},
		},
		"required":             []interface{}{"topic"},
		"additionalProperties": false,
	}
	compatible := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"topic": map[string]interface{}{"type": "string"},
			"count": map[string]interface{}{"type": "number"},
		},
		"required": []interface{}{"topic"},
	}
	if !externalInputSchemaCompatible(compatible, capability) {
		t.Fatalf("expected controlled schema to be compatible")
	}

	missingRequired := map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"topic": map[string]interface{}{"type": "string"}},
	}
	if externalInputSchemaCompatible(missingRequired, capability) {
		t.Fatalf("Agent-required field must also be required by the listing")
	}

	wrongType := map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"topic": map[string]interface{}{"type": "number"}},
		"required":   []interface{}{"topic"},
	}
	if externalInputSchemaCompatible(wrongType, capability) {
		t.Fatalf("incompatible field type should be rejected")
	}
}

func TestNormalizeExternalInputSchemaRejectsHostedControlArrays(t *testing.T) {
	_, err := normalizeExternalInputSchema(json.RawMessage(`[{"key":"topic","type":"text"}]`))
	if err == nil {
		t.Fatal("Core External Execution must accept only generic JSON Schema objects")
	}
}

func TestExternalInputSchemaCompatibleRejectsUnprovenConstraints(t *testing.T) {
	listing := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"channel": map[string]interface{}{"type": "string", "enum": []interface{}{"email", "web"}},
			"source":  map[string]interface{}{"type": "string", "format": "uri"},
		},
	}
	compatible := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"channel": map[string]interface{}{"type": "string", "enum": []interface{}{"email", "web", "api"}},
			"source":  map[string]interface{}{"type": "string", "format": "uri"},
		},
		"additionalProperties": false,
	}
	if !externalInputSchemaCompatible(listing, compatible) {
		t.Fatal("listing enum subset and matching format should be compatible")
	}

	restricted := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"channel": map[string]interface{}{"type": "string", "pattern": "^email$"},
			"source":  map[string]interface{}{"type": "string", "format": "email"},
		},
		"additionalProperties": false,
	}
	if externalInputSchemaCompatible(listing, restricted) {
		t.Fatal("constraints not guaranteed by the controlled form must fail closed")
	}
}
