package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	runtimeBoundaryVersionedIdentifier = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*V[0-9]+[A-Za-z0-9_]*\b`)
	runtimeBoundaryVersionedFile       = regexp.MustCompile(`_v[0-9]+(?:_|\.|$)`)
)

func TestRuntimePublicBoundaryUsesCanonicalNamesAndPaths(t *testing.T) {
	repositoryRoot := runtimeRepositoryRoot(t)
	forbidden := []string{
		"/api/v1/agent-runtime/" + "v2",
		"/agent-runtime/" + "v2/",
		"core-runtime." + "v2.json",
		"\"runtime_" + "v2\"",
		"--pre-" + "v2-ok",
		"\"pre_" + "v2_noop\"",
		"\"agent_" + "node\"",
		"'agent_" + "node'",
	}

	err := filepath.WalkDir(repositoryRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != repositoryRoot && runtimeBoundaryIgnoredDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		relativePath := relativeRuntimeBoundaryPath(repositoryRoot, path)
		if runtimeBoundaryVersionedRuntimeFile(relativePath) {
			t.Errorf("active Runtime path %s contains a generation suffix", relativePath)
		}
		for _, value := range forbidden {
			if strings.Contains(relativePath, value) {
				t.Errorf("active path %s contains retired public boundary %q", relativePath, value)
			}
		}
		if !runtimeBoundaryTextFile(path) {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(body)
		if extension := strings.ToLower(filepath.Ext(path)); extension == ".sql" && runtimeBoundaryRuntimeSource(relativePath) {
			if identifier := runtimeBoundaryVersionedIdentifier.FindString(text); identifier != "" {
				t.Errorf("active source %s contains generation-labeled identifier %s", relativePath, identifier)
			}
		}
		for _, value := range forbidden {
			if strings.Contains(text, value) {
				t.Errorf("active source %s contains retired public boundary %q", relativePath, value)
			}
		}
		if filepath.Base(path) != "CHANGELOG.md" && !strings.HasSuffix(path, "_test.go") &&
			strings.Contains(strings.ToLower(text), "runtime "+"v2") {
			t.Errorf("active product copy %s exposes a Runtime version label", relativePath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan Runtime public boundary: %v", err)
	}

	assertNoVersionedRuntimeIdentifiers(t, repositoryRoot)
}

func assertNoVersionedRuntimeIdentifiers(t *testing.T, repositoryRoot string) {
	t.Helper()
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(repositoryRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != repositoryRoot && runtimeBoundaryIgnoredDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		relativePath := relativeRuntimeBoundaryPath(repositoryRoot, path)
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if runtimeBoundaryVersionedRuntimeIdentifier(relativePath, identifier.Name) {
				t.Errorf(
					"active identifier %s in %s contains a Runtime generation label",
					identifier.Name,
					relativePath,
				)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("inspect Runtime public Go exports: %v", err)
	}
}

func runtimeBoundaryVersionedRuntimeFile(path string) bool {
	if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sql") {
		return false
	}
	if !runtimeBoundaryVersionedFile.MatchString(filepath.Base(path)) {
		return false
	}
	return runtimeBoundaryRuntimeSource(path)
}

func runtimeBoundaryVersionedRuntimeIdentifier(path, identifier string) bool {
	if !runtimeBoundaryVersionedIdentifier.MatchString(identifier) {
		return false
	}
	// This is a stable fingerprint-envelope schema version, not a Runtime
	// implementation generation.
	if identifier == "RunFingerprintSchemaV1" {
		return false
	}
	return runtimeBoundaryRuntimeSource(path) ||
		strings.Contains(identifier, "Runtime") || strings.Contains(identifier, "runtime")
}

func runtimeBoundaryRuntimeSource(path string) bool {
	return strings.HasPrefix(path, "pkg/runtime/") ||
		strings.HasPrefix(path, "cmd/runtime-loadtest/") ||
		strings.HasPrefix(path, "pkg/db/queries/runtime") ||
		strings.HasPrefix(path, "pkg/db/generated/runtime")
}

func runtimeRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func runtimeBoundaryIgnoredDirectory(name string) bool {
	switch name {
	case ".git", "bin", "migrations", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func runtimeBoundaryTextFile(path string) bool {
	name := filepath.Base(path)
	if name == "Makefile" || name == "Dockerfile" || strings.HasPrefix(name, ".env") {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".sql", ".md", ".json", ".yaml", ".yml", ".toml", ".sh":
		return true
	default:
		return false
	}
}

func relativeRuntimeBoundaryPath(repositoryRoot, path string) string {
	relative, err := filepath.Rel(repositoryRoot, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}
