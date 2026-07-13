package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	assertNoVersionedRuntimeExports(t, repositoryRoot)
}

func assertNoVersionedRuntimeExports(t *testing.T, repositoryRoot string) {
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
			if relativeRuntimeBoundaryPath(repositoryRoot, path) == "pkg/db/generated" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, declaration := range parsed.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				if value.Recv == nil && ast.IsExported(value.Name.Name) && strings.Contains(value.Name.Name, "V2") {
					t.Errorf("public function %s in %s contains a Runtime version label", value.Name.Name, relativeRuntimeBoundaryPath(repositoryRoot, path))
				}
			case *ast.GenDecl:
				for _, spec := range value.Specs {
					switch item := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range item.Names {
							if ast.IsExported(name.Name) && strings.Contains(name.Name, "V2") {
								t.Errorf("public value %s in %s contains a Runtime version label", name.Name, relativeRuntimeBoundaryPath(repositoryRoot, path))
							}
						}
					case *ast.TypeSpec:
						if !ast.IsExported(item.Name.Name) {
							continue
						}
						if strings.Contains(item.Name.Name, "V2") {
							t.Errorf("public type %s in %s contains a Runtime version label", item.Name.Name, relativeRuntimeBoundaryPath(repositoryRoot, path))
						}
						ast.Inspect(item.Type, func(node ast.Node) bool {
							field, ok := node.(*ast.Field)
							if !ok {
								return true
							}
							for _, name := range field.Names {
								if ast.IsExported(name.Name) && strings.Contains(name.Name, "V2") {
									t.Errorf("public field %s.%s in %s contains a Runtime version label", item.Name.Name, name.Name, relativeRuntimeBoundaryPath(repositoryRoot, path))
								}
							}
							return true
						})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect Runtime public Go exports: %v", err)
	}
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
