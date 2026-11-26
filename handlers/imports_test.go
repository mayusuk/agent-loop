// Package handlers holds the starter handlers, agent-loop-claude
// (PRODUCT-SPEC D38). This file is its import guard: the core ships no
// handlers (D34), so in one repository (v0.2 plan D14) the handlers import
// nothing from this module but contract/, and nothing else imports them.
package handlers

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

const (
	modulePath   = "github.com/mayusuk/agent-loop"
	contractPath = modulePath + "/contract"
	handlersPath = modulePath + "/handlers"
)

// within reports whether import path p is pkg or one of its subpackages.
func within(p, pkg string) bool {
	return p == pkg || strings.HasPrefix(p, pkg+"/")
}

// badImports parses every .go file under root, a tree laid out like this
// module (test files included, testdata directories skipped), and returns
// "file: import" for each import that breaks D34, sorted, with file relative
// to root:
//   - a file under handlers/ imports a package of this module other than
//     contract/... or handlers/...;
//   - a file outside handlers/ imports handlers/....
//
// The standard library and other modules are not this guard's concern
// (work plan #179 D4).
func badImports(root string) ([]string, error) {
	var bad []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		inHandlers := strings.HasPrefix(rel, "handlers/")
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			switch {
			case inHandlers && within(p, modulePath) && !within(p, contractPath) && !within(p, handlersPath):
				bad = append(bad, rel+": "+p)
			case !inHandlers && within(p, handlersPath):
				bad = append(bad, rel+": "+p)
			}
		}
		return nil
	})
	sort.Strings(bad)
	return bad, err
}

// TestD34_HandlersAndCoreStaySeparate is the guard, run on this repository:
// handlers/ imports only contract/ from this module, and nothing outside
// handlers/ imports it (D34, v0.2 plan D14).
func TestD34_HandlersAndCoreStaySeparate(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	bad, err := badImports(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Errorf("imports that break D34 (handlers/ may import only contract/ from this module, and only handlers/ may import handlers/):\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestD34_GuardCatchesBothDirections: on a fixture tree the guard reports a
// handler importing the core, a core package and a test importing the
// handlers, and nothing for the imports D34 allows.
func TestD34_GuardCatchesBothDirections(t *testing.T) {
	bad, err := badImports(filepath.Join("testdata", "importguard"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cmd/tool/main_test.go: github.com/mayusuk/agent-loop/handlers/internal/run",
		"handlers/internal/bad/bad.go: github.com/mayusuk/agent-loop/internal/core",
		"handlers/internal/bad/bad_test.go: github.com/mayusuk/agent-loop/conformance",
		"internal/core/core.go: github.com/mayusuk/agent-loop/handlers",
	}
	if !reflect.DeepEqual(bad, want) {
		t.Errorf("badImports(fixture) =\n  %s\nwant\n  %s", strings.Join(bad, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestD34_GuardDoesNotTakeLookalikePaths: a module path that only starts
// with contract/'s or handlers/'s path is not part of it, and one that only
// starts with this module's path is another module.
func TestD34_GuardDoesNotTakeLookalikePaths(t *testing.T) {
	for p, want := range map[string]bool{
		contractPath:                         true,
		contractPath + "/v1":                 true,
		modulePath + "/contractx":            false,
		modulePath + "-handlers/internal/x":  true, // another module,
		handlersPath + "/internal/run":       true,
		modulePath + "/handlersx/internal/x": false,
	} {
		inModule := within(p, modulePath)
		allowed := within(p, contractPath) || within(p, handlersPath)
		if got := allowed || !inModule; got != want {
			t.Errorf("%s: allowed in handlers/ = %v, want %v", p, got, want)
		}
	}
}
