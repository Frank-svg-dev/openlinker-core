package externalexecution

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCurrentExecutionBoundaryHasNoCommercialOrLegacyBridgeSemantics(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\bhosted\b|hostedcontract|buyer_user|seller_user|external_order|subject_user_id|target_owner_user_id`)
	roots := []string{".", "../workflow", "../executioncontract"}
	files := []string{"../coreapi/api.go", "../db/queries/workflows.sql", "../db/generated/workflows.sql.go"}

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			files = append(files, filepath.Join(root, entry.Name()))
		}
	}
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if match := forbidden.Find(content); match != nil {
			t.Fatalf("%s contains forbidden current-boundary term %q", path, match)
		}
	}
}
