package externalexecution

import (
	"strings"
)

// externalInputSchemaCompatible proves that an external caller's generic
// object schema is accepted by the target capability. It fails closed for
// constraints this bounded compatibility checker cannot prove.
func externalInputSchemaCompatible(external, capability map[string]interface{}) bool {
	if !schemaAllowsType(external["type"], "object") || !schemaAllowsType(capability["type"], "object") {
		return false
	}
	externalProps, ok := schemaProperties(external)
	if !ok {
		return false
	}
	capabilityProps, ok := schemaProperties(capability)
	if !ok {
		return false
	}
	externalRequired := schemaRequiredSet(external)
	for key := range schemaRequiredSet(capability) {
		if _, exists := externalProps[key]; !exists {
			return false
		}
		if _, required := externalRequired[key]; !required {
			return false
		}
	}
	additionalAllowed := true
	if raw, exists := capability["additionalProperties"]; exists {
		allowed, ok := raw.(bool)
		if !ok {
			return false
		}
		additionalAllowed = allowed
	}
	if schemaHasUnsupportedConstraints(capability, "minProperties", "maxProperties", "propertyNames", "dependentRequired", "dependentSchemas", "allOf", "anyOf", "oneOf", "not", "if", "then", "else") {
		return false
	}
	for key, externalProperty := range externalProps {
		capabilityProperty, exists := capabilityProps[key]
		if !exists {
			if !additionalAllowed {
				return false
			}
			continue
		}
		externalSchema, externalOK := externalProperty.(map[string]interface{})
		capabilitySchema, capabilityOK := capabilityProperty.(map[string]interface{})
		if !externalOK || !capabilityOK || !schemaTypeSubset(externalSchema["type"], capabilitySchema["type"]) || !schemaConstraintsCompatible(externalSchema, capabilitySchema) {
			return false
		}
	}
	return true
}

func schemaConstraintsCompatible(external, capability map[string]interface{}) bool {
	if schemaHasUnsupportedConstraints(capability, "const", "pattern", "minLength", "maxLength", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "allOf", "anyOf", "oneOf", "not", "if", "then", "else") {
		return false
	}
	if capabilityFormat, constrained := capability["format"]; constrained {
		externalFormat, ok := external["format"].(string)
		expectedFormat, expectedOK := capabilityFormat.(string)
		if !ok || !expectedOK || externalFormat != expectedFormat {
			return false
		}
	}
	if _, constrained := capability["enum"]; constrained {
		externalEnum, externalOK := schemaStringEnum(external["enum"])
		capabilityEnum, capabilityOK := schemaStringEnum(capability["enum"])
		if !externalOK || !capabilityOK || len(externalEnum) == 0 || len(capabilityEnum) == 0 {
			return false
		}
		for value := range externalEnum {
			if _, allowed := capabilityEnum[value]; !allowed {
				return false
			}
		}
	}
	return true
}

func schemaHasUnsupportedConstraints(schema map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if _, exists := schema[key]; exists {
			return true
		}
	}
	return false
}

func schemaStringEnum(raw interface{}) (map[string]struct{}, bool) {
	values := map[string]struct{}{}
	switch items := raw.(type) {
	case []interface{}:
		for _, item := range items {
			value, ok := item.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return nil, false
			}
			values[value] = struct{}{}
		}
	case []string:
		for _, value := range items {
			if strings.TrimSpace(value) == "" {
				return nil, false
			}
			values[value] = struct{}{}
		}
	default:
		return nil, false
	}
	return values, true
}

func schemaProperties(schema map[string]interface{}) (map[string]interface{}, bool) {
	raw, exists := schema["properties"]
	if !exists {
		return map[string]interface{}{}, true
	}
	properties, ok := raw.(map[string]interface{})
	return properties, ok
}

func schemaRequiredSet(schema map[string]interface{}) map[string]struct{} {
	set := map[string]struct{}{}
	switch raw := schema["required"].(type) {
	case []interface{}:
		for _, item := range raw {
			if key, ok := item.(string); ok {
				set[key] = struct{}{}
			}
		}
	case []string:
		for _, key := range raw {
			set[key] = struct{}{}
		}
	}
	return set
}

func schemaTypeSubset(listingRaw, capabilityRaw interface{}) bool {
	listingTypes := schemaTypes(listingRaw)
	capabilityTypes := schemaTypes(capabilityRaw)
	if len(listingTypes) == 0 || len(capabilityTypes) == 0 {
		return false
	}
	for listingType := range listingTypes {
		if _, allowed := capabilityTypes[listingType]; allowed {
			continue
		}
		if listingType == "integer" {
			if _, allowed := capabilityTypes["number"]; allowed {
				continue
			}
		}
		return false
	}
	return true
}

func schemaAllowsType(raw interface{}, expected string) bool {
	_, ok := schemaTypes(raw)[expected]
	return ok
}

func schemaTypes(raw interface{}) map[string]struct{} {
	result := map[string]struct{}{}
	switch value := raw.(type) {
	case string:
		result[value] = struct{}{}
	case []interface{}:
		for _, item := range value {
			if itemType, ok := item.(string); ok {
				result[itemType] = struct{}{}
			}
		}
	case []string:
		for _, itemType := range value {
			result[itemType] = struct{}{}
		}
	}
	return result
}
