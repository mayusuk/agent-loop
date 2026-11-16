package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

const schemaBase = "https://github.com/mayusuk/agent-loop/contract/v1/"

var (
	schemasOnce sync.Once
	schemas     map[string]*jsonschema.Schema
	schemasErr  error
)

// compiled returns the embedded schema called name, compiled.
func compiled(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	schemasOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.Draft = jsonschema.Draft2020
		c.AssertFormat = true
		names := []string{InputSchema, ResultSchema, RecordsSchema, TasksSchema}
		for _, n := range names {
			b, err := Schema(n)
			if err != nil {
				schemasErr = err
				return
			}
			if err := c.AddResource(schemaBase+n, bytes.NewReader(b)); err != nil {
				schemasErr = err
				return
			}
		}
		schemas = map[string]*jsonschema.Schema{}
		for _, n := range names {
			s, err := c.Compile(schemaBase + n)
			if err != nil {
				schemasErr = err
				return
			}
			schemas[n] = s
		}
	})
	if schemasErr != nil {
		t.Fatalf("compile schemas: %v", schemasErr)
	}
	return schemas[name]
}

// schemaCheck validates JSON data against the named schema.
func schemaCheck(t *testing.T, name string, data []byte) error {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	return compiled(t, name).Validate(v)
}

func TestSchemasAreEmbeddedAndCompile(t *testing.T) {
	for _, n := range []string{InputSchema, ResultSchema, RecordsSchema, TasksSchema} {
		if compiled(t, n) == nil {
			t.Errorf("schema %s missing", n)
		}
	}
	if _, err := Schema("nope.schema.json"); err == nil {
		t.Error("Schema returned a file that does not exist")
	}
}

// TestSpecExamplesMatchSchemas checks every example JSON of
// INTEGRATION-SPEC §4–§7 against its schema. The examples are copied from
// the spec into testdata/examples, with placeholders such as "<sha>" and
// "…" filled in where the schema needs a real value.
func TestSpecExamplesMatchSchemas(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "examples", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 19 {
		t.Fatalf("found %d examples, want at least 19", len(files))
	}
	for _, f := range files {
		base := filepath.Base(f)
		var schema string
		switch {
		case strings.HasPrefix(base, "input-"):
			schema = InputSchema
		case strings.HasPrefix(base, "result-"):
			schema = ResultSchema
		case strings.HasPrefix(base, "record-"):
			schema = RecordsSchema
		case base == "tasks.json":
			schema = TasksSchema
		default:
			t.Fatalf("example %s has no schema", base)
		}
		t.Run(base, func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := schemaCheck(t, schema, b); err != nil {
				t.Errorf("%s does not match %s: %v", base, schema, err)
			}
		})
	}
}

// TestSpecResultExamplesAreValid runs each result example through
// ValidateResult with the matching input example and the text files it
// names.
func TestSpecResultExamplesAreValid(t *testing.T) {
	cases := []struct{ input, result string }{
		{"input-implement.json", "result-implement-done.json"},
		{"input-implement.json", "result-implement-blocked.json"},
		{"input-implement.json", "result-implement-failed.json"},
		{"input-implement.json", "result-implement-needs-owner.json"},
		{"input-review.json", "result-review-done.json"},
		{"input-address.json", "result-address-done.json"},
		{"input-mint.json", "result-mint-done.json"},
		{"input-mint.json", "result-mint-complete.json"},
	}
	for _, c := range cases {
		t.Run(c.result, func(t *testing.T) {
			in, err := ParseInput(readExample(t, c.input))
			if err != nil {
				t.Fatal(err)
			}
			out := outDir(t)
			r, err := ValidateResult(in, readExample(t, c.result), out)
			if err != nil {
				t.Fatalf("ValidateResult: %v", err)
			}
			if r.RunID != in.RunID {
				t.Errorf("run_id %q, want %q", r.RunID, in.RunID)
			}
		})
	}
}

func TestTaskListExampleDecodes(t *testing.T) {
	var tl TaskList
	if err := decodeStrict(readExample(t, "tasks.json"), &tl); err != nil {
		t.Fatal(err)
	}
	if tl.Plan != "003-auth" || len(tl.Tasks) != 2 || tl.Tasks[1].DependsOn[0] != "login-api" {
		t.Errorf("decoded %+v", tl)
	}
}

func TestTaskListSchemaRejects(t *testing.T) {
	bad := map[string]string{
		"version 2":       `{"version":2,"plan":"p","tasks":[]}`,
		"bad plan key":    `{"version":1,"plan":"P_1","tasks":[]}`,
		"bad task key":    `{"version":1,"plan":"p","tasks":[{"key":"A","type":"feature","title":"t","body":"b.md"}]}`,
		"long title":      `{"version":1,"plan":"p","tasks":[{"key":"a","type":"feature","title":"` + strings.Repeat("x", 201) + `","body":"b.md"}]}`,
		"no body":         `{"version":1,"plan":"p","tasks":[{"key":"a","type":"feature","title":"t"}]}`,
		"body escapes":    `{"version":1,"plan":"p","tasks":[{"key":"a","type":"feature","title":"t","body":"../x.md"}]}`,
		"unknown field":   `{"version":1,"plan":"p","tasks":[],"extra":1}`,
		"duplicate deps":  `{"version":1,"plan":"p","tasks":[{"key":"a","type":"feature","title":"t","body":"b.md","depends_on":["x","x"]}]}`,
		"no tasks":        `{"version":1,"plan":"p"}`,
		"unknown in task": `{"version":1,"plan":"p","tasks":[{"key":"a","type":"feature","title":"t","body":"b.md","x":1}]}`,
	}
	for name, doc := range bad {
		if err := schemaCheck(t, TasksSchema, []byte(doc)); err == nil {
			t.Errorf("%s: tasks schema accepted %s", name, doc)
		}
	}
}

func readExample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// textFiles are the *_file names the examples and table tests use.
var textFiles = []string{
	"change.md", "summary.md", "f5.md", "mc981.md", "n1.md", "n2.md",
	"r3.md", "r5.md", "m981.md", "proposal.md", "reason.md", "question.md",
}

// outDir returns a fresh AGENT_LOOP_OUT holding textFiles.
func outDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range textFiles {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("# text\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
