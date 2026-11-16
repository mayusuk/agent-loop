package core

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// contractPath is the only module package tree core may import
// (TECHNICAL-SPEC §3.1).
const contractPath = "github.com/mayusuk/agent-loop/contract"

// allowedImport reports whether core may import path: the standard library
// or contract/ and its subpackages. A path is standard library when its first
// element has no dot, as module paths outside the standard library must have
// one. "C" (cgo) is not allowed.
func allowedImport(path string) bool {
	if path == contractPath || strings.HasPrefix(path, contractPath+"/") {
		return true
	}
	if path == "C" {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// forbiddenImports parses every .go file under root, test files included and
// testdata directories excluded, and returns "file: import" for each import
// core may not use, sorted, with file relative to root.
func forbiddenImports(root string) ([]string, error) {
	var bad []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if !allowedImport(p) {
				bad = append(bad, filepath.ToSlash(rel)+": "+p)
			}
		}
		return nil
	})
	sort.Strings(bad)
	return bad, err
}

// TestCoreImportsOnlyStdlibAndContract is the guard: internal/core imports
// only the standard library and contract/ (TECHNICAL-SPEC §3.1).
func TestCoreImportsOnlyStdlibAndContract(t *testing.T) {
	bad, err := forbiddenImports(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Errorf("internal/core may import only the standard library and contract/: %s", b)
	}
}

func TestImportGuardFailsOnForbiddenFixture(t *testing.T) {
	bad, err := forbiddenImports(filepath.Join("testdata", "forbidden"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"bad.go: github.com/mayusuk/agent-loop/contractx",
		"bad.go: github.com/mayusuk/agent-loop/internal/ports",
		"bad_test.go: go.temporal.io/sdk/workflow",
		"cgo.go: C",
		"sub/deep.go: github.com/other/module",
	}
	if !reflect.DeepEqual(bad, want) {
		t.Fatalf("forbidden imports:\n got %q\nwant %q", bad, want)
	}
}

func TestImportGuardPassesAllowedFixture(t *testing.T) {
	bad, err := forbiddenImports(filepath.Join("testdata", "allowed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("allowed fixture reported forbidden imports: %q", bad)
	}
}

func TestImportGuardReportsUnparsableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package x\nimport (\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := forbiddenImports(dir); err == nil {
		t.Fatal("want a parse error, got nil")
	}
}

func TestAllowedImport(t *testing.T) {
	cases := map[string]bool{
		"fmt":                                    true,
		"net/http":                               true,
		"encoding/json":                          true,
		contractPath:                             true,
		contractPath + "/v1":                     true,
		contractPath + "x":                       false,
		"github.com/mayusuk/agent-loop/internal": false,
		"github.com/mayusuk/agent-loop":          false,
		"go.temporal.io/sdk/workflow":            false,
		"golang.org/x/sync/errgroup":             false,
		"C":                                      false,
	}
	for path, want := range cases {
		if got := allowedImport(path); got != want {
			t.Errorf("allowedImport(%q) = %v, want %v", path, got, want)
		}
	}
}
