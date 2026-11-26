package tasklist

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mayusuk/agent-loop/contract"
)

// validYAML is the INTEGRATION-SPEC §7 example.
const validYAML = `version: 1
plan: 003-auth                 # must equal the directory name
tasks:
  - key: login-api             # unique in the plan, [a-z0-9-]{1,64}, never reused
    type: feature              # a configured type: feature | bug
    title: Add login endpoint  # at most 200 characters
    body: tasks/login-api.md   # path relative to the plan directory
    depends_on: []             # task keys in this plan, or existing item refs ("#17", "PROJ-12")
  - key: login-ui
    type: feature
    title: Login screen
    body: tasks/login-ui.md
    depends_on: [login-api]
`

var types = []string{"feature", "bug"}

func bodies() fstest.MapFS {
	return fstest.MapFS{
		"tasks/login-api.md": {Data: []byte("# Login API\n")},
		"tasks/login-ui.md":  {Data: []byte("# Login UI\n")},
		"tasks/c.md":         {Data: []byte("c\n")},
	}
}

// existing is an itemExists that knows #17 and PROJ-12.
func existing(ref string) bool { return ref == "#17" || ref == "PROJ-12" }

// third appends a third task to validYAML.
func third(fields string) string {
	return validYAML + "  - key: c\n    type: bug\n    title: C\n    body: tasks/c.md\n" + fields
}

func TestFR17AC6_ValidListPasses(t *testing.T) {
	tl, probs := ValidateTasks("003-auth", []byte(third("    depends_on: [login-ui, \"#17\", PROJ-12]\n")), bodies(), types, existing)
	if len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	want := contract.TaskList{Version: 1, Plan: "003-auth", Tasks: []contract.Task{
		{Key: "login-api", Type: "feature", Title: "Add login endpoint", Body: "tasks/login-api.md"},
		{Key: "login-ui", Type: "feature", Title: "Login screen", Body: "tasks/login-ui.md", DependsOn: []string{"login-api"}},
		{Key: "c", Type: "bug", Title: "C", Body: "tasks/c.md", DependsOn: []string{"login-ui", "#17", "PROJ-12"}},
	}}
	if !reflect.DeepEqual(tl, want) {
		t.Errorf("task list\n got %+v\nwant %+v", tl, want)
	}
}

func TestFR17AC6_EmptyTaskListAndCleanBodyPathPass(t *testing.T) {
	if _, probs := ValidateTasks("p", []byte("version: 1\nplan: p\ntasks: []\n"), fstest.MapFS{}, types, nil); len(probs) > 0 {
		t.Errorf("empty list: %v", probs)
	}
	y := strings.Replace(validYAML, "body: tasks/login-ui.md", "body: ./tasks//login-ui.md", 1)
	tl, probs := ValidateTasks("003-auth", []byte(y), bodies(), types, nil)
	if len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	if tl.Tasks[1].Body != "tasks/login-ui.md" {
		t.Errorf("body %q, want the cleaned path", tl.Tasks[1].Body)
	}
}

type tcase struct {
	name string
	yaml string
	// dir is the plan directory name ("003-auth" when empty).
	dir    string
	bodies fstest.MapFS
	// want lists the expected problems as "rule path".
	want []string
}

func run(t *testing.T, cases []tcase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.dir
			if dir == "" {
				dir = "003-auth"
			}
			fs := c.bodies
			if fs == nil {
				fs = bodies()
			}
			tl, probs := ValidateTasks(dir, []byte(c.yaml), fs, types, existing)
			var got []string
			for _, p := range probs {
				got = append(got, string(p.Rule)+" "+p.Path)
				if p.Message == "" {
					t.Errorf("problem %+v has no message", p)
				}
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("problems\n got %q\nwant %q\nfull: %v", got, c.want, probs)
			}
			if len(probs) > 0 && !reflect.DeepEqual(tl, contract.TaskList{}) {
				t.Errorf("an invalid list returned %+v", tl)
			}
		})
	}
}

func TestFR17AC6_SchemaMismatch(t *testing.T) {
	run(t, []tcase{
		{name: "not YAML", yaml: "version: [1\n", want: []string{"schema "}},
		{name: "empty file", yaml: "", want: []string{"schema "}},
		{name: "comments only", yaml: "# nothing\n", want: []string{"schema "}},
		{name: "two documents", yaml: validYAML + "---\n" + validYAML, want: []string{"schema "}},
		{name: "not a mapping", yaml: "- 1\n", want: []string{"schema "}},
		{name: "version 2", yaml: strings.Replace(validYAML, "version: 1", "version: 2", 1), want: []string{"schema version"}},
		{name: "version string", yaml: strings.Replace(validYAML, "version: 1", `version: "1"`, 1), want: []string{"schema version"}},
		{name: "missing tasks", yaml: "version: 1\nplan: 003-auth\n", want: []string{"schema tasks"}},
		{name: "unknown top-level key", yaml: validYAML + "extra: 1\n", want: []string{"schema extra"}},
		{name: "repeated mapping key", yaml: validYAML + "plan: 003-auth\n", want: []string{"schema plan"}},
		{name: "non-string mapping key", yaml: validYAML + "1: x\n", want: []string{"schema "}},
		{name: "alias", yaml: "version: 1\nplan: &p 003-auth\ntasks: []\nx: *p\n", want: []string{"schema plan", "schema x"}},
		{name: "merge key", yaml: "version: 1\nplan: 003-auth\ntasks: []\n<<: {a: 1}\n", want: []string{"schema "}},
		{name: "infinite number", yaml: strings.Replace(validYAML, "version: 1", "version: .inf", 1), want: []string{"schema version"}},
		{name: "bad plan key", yaml: strings.Replace(validYAML, "plan: 003-auth", "plan: 003_Auth", 1), dir: "003_Auth", want: []string{"schema plan"}},
		{name: "bad task key", yaml: strings.Replace(validYAML, "key: login-ui", "key: Login-UI", 1), want: []string{"schema tasks[1].key"}},
		{name: "key is a number", yaml: strings.Replace(validYAML, "key: login-ui", "key: 7", 1), want: []string{"schema tasks[1].key"}},
		{name: "title over 200 characters", yaml: strings.Replace(validYAML, "title: Login screen", "title: "+strings.Repeat("x", 201), 1), want: []string{"schema tasks[1].title"}},
		{name: "title of 200 characters", yaml: strings.Replace(validYAML, "title: Login screen", "title: "+strings.Repeat("é", 200), 1)},
		{name: "missing body", yaml: strings.Replace(validYAML, "    body: tasks/login-ui.md\n", "", 1), want: []string{"schema tasks[1].body"}},
		{name: "absolute body", yaml: strings.Replace(validYAML, "body: tasks/login-ui.md", "body: /etc/passwd", 1), want: []string{"schema tasks[1].body"}},
		{name: "body escapes the plan", yaml: strings.Replace(validYAML, "body: tasks/login-ui.md", "body: tasks/../../x.md", 1), want: []string{"schema tasks[1].body"}},
		{name: "unknown task key", yaml: validYAML + "    labels: [x]\n", want: []string{"schema tasks[1].labels"}},
		{name: "repeated depends_on entry", yaml: strings.Replace(validYAML, "[login-api]", "[login-api, login-api]", 1), want: []string{"schema tasks[1].depends_on"}},
		{name: "task not a mapping", yaml: validYAML + "  - x\n", want: []string{"schema tasks[2]"}},
	})
}

func TestFR17AC6_PlanNotTheDirectory(t *testing.T) {
	run(t, []tcase{{name: "other directory", yaml: validYAML, dir: "004-billing", want: []string{"plan plan"}}})
}

func TestFR17AC6_DuplicateKey(t *testing.T) {
	run(t, []tcase{{
		name: "key repeated",
		yaml: validYAML + "  - key: login-api\n    type: bug\n    title: Again\n    body: tasks/c.md\n",
		want: []string{"duplicate_key tasks[2].key"},
	}})
}

func TestFR17AC6_TypeNotConfigured(t *testing.T) {
	run(t, []tcase{{name: "chore", yaml: strings.Replace(validYAML, "type: feature\n    title: Login screen", "type: chore\n    title: Login screen", 1), want: []string{"type tasks[1].type"}}})
}

func TestFR17AC6_BodyFile(t *testing.T) {
	big := bodies()
	big["tasks/login-ui.md"] = &fstest.MapFile{Data: []byte(strings.Repeat("x", MaxBody+1))}
	exact := bodies()
	exact["tasks/login-ui.md"] = &fstest.MapFile{Data: []byte(strings.Repeat("x", MaxBody))}
	dir := bodies()
	delete(dir, "tasks/login-ui.md")
	dir["tasks/login-ui.md/x"] = &fstest.MapFile{Data: []byte("x")}
	missing := bodies()
	delete(missing, "tasks/login-ui.md")
	run(t, []tcase{
		{name: "missing", yaml: validYAML, bodies: missing, want: []string{"body_missing tasks[1].body"}},
		{name: "a directory", yaml: validYAML, bodies: dir, want: []string{"body_missing tasks[1].body"}},
		{name: "names the plan directory", yaml: strings.Replace(validYAML, "body: tasks/login-ui.md", "body: tasks/..", 1), want: []string{"schema tasks[1].body"}},
		{name: "dot", yaml: strings.Replace(validYAML, "body: tasks/login-ui.md", "body: ./", 1), want: []string{"body_missing tasks[1].body"}},
		{name: "over 64 KiB", yaml: validYAML, bodies: big, want: []string{"body_too_large tasks[1].body"}},
		{name: "exactly 64 KiB", yaml: validYAML, bodies: exact},
	})
}

func TestFR17AC6_DependsOn(t *testing.T) {
	run(t, []tcase{
		{name: "unknown key", yaml: strings.Replace(validYAML, "[login-api]", "[login-apx]", 1), want: []string{"unknown_key tasks[1].depends_on[0]"}},
		{name: "missing external item", yaml: strings.Replace(validYAML, "[login-api]", `[login-api, "#18", PROJ-13]`, 1), want: []string{"missing_item tasks[1].depends_on[1]", "missing_item tasks[1].depends_on[2]"}},
		{name: "existing external item", yaml: strings.Replace(validYAML, "[login-api]", `[login-api, "#17"]`, 1)},
	})
}

func TestFR17AC6_ExternalItemsNotCheckedWithoutTracker(t *testing.T) {
	y := strings.Replace(validYAML, "[login-api]", `["#18"]`, 1)
	if _, probs := ValidateTasks("003-auth", []byte(y), bodies(), types, nil); len(probs) > 0 {
		t.Errorf("nil itemExists: %v", probs)
	}
}

func TestFR17AC6_Cycle(t *testing.T) {
	run(t, []tcase{
		{name: "two tasks", yaml: strings.Replace(validYAML, "depends_on: []", "depends_on: [login-ui]", 1), want: []string{"cycle tasks[0].depends_on"}},
		{name: "depends on itself", yaml: strings.Replace(validYAML, "[login-api]", "[login-ui]", 1), want: []string{"cycle tasks[1].depends_on"}},
		{name: "three tasks", yaml: strings.Replace(third("    depends_on: [login-ui]\n"), "depends_on: []", "depends_on: [c]", 1), want: []string{"cycle tasks[0].depends_on"}},
	})
	_, probs := ValidateTasks("003-auth", []byte(strings.Replace(third("    depends_on: [login-ui]\n"), "depends_on: []", "depends_on: [c]", 1)), bodies(), types, nil)
	if len(probs) != 1 || probs[0].Task != "login-api" || !strings.Contains(probs[0].Message, `"login-api", "login-ui" and "c"`) {
		t.Errorf("three-task cycle: %+v", probs)
	}
}

// TestFR17AC6_ReturnsEveryProblem checks that one file with many problems
// reports each of them, in the documented order.
func TestFR17AC6_ReturnsEveryProblem(t *testing.T) {
	y := `version: 1
plan: 003-auth
tasks:
  - key: a
    type: chore
    title: ` + strings.Repeat("x", 201) + `
    body: tasks/none.md
    depends_on: [b, nope, "#18"]
  - key: b
    type: feature
    title: B
    body: tasks/c.md
    depends_on: [a]
  - key: a
    type: bug
    title: A again
    body: tasks/c.md
`
	_, probs := ValidateTasks("004-x", []byte(y), bodies(), types, existing)
	var got []string
	for _, p := range probs {
		got = append(got, string(p.Rule)+" "+p.Path)
	}
	want := []string{
		"schema tasks[0].title",
		"plan plan",
		"duplicate_key tasks[2].key",
		"type tasks[0].type",
		"body_missing tasks[0].body",
		"unknown_key tasks[0].depends_on[1]",
		"missing_item tasks[0].depends_on[2]",
		"cycle tasks[0].depends_on",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("problems\n got %q\nwant %q", got, want)
	}
	if probs[0].Error() != probs[0].Path+": "+probs[0].Message {
		t.Errorf("Error() = %q", probs[0].Error())
	}
}

// TestFR17AC6_TimestampTitleIsAString checks that a YAML timestamp is read
// as its text (work plan #154 D3).
func TestFR17AC6_TimestampTitleIsAString(t *testing.T) {
	y := strings.Replace(validYAML, "title: Login screen", "title: 2026-11-26", 1)
	tl, probs := ValidateTasks("003-auth", []byte(y), bodies(), types, nil)
	if len(probs) > 0 || tl.Tasks[1].Title != "2026-11-26" {
		t.Errorf("timestamp title: %v %+v", probs, tl)
	}
}

func TestProblemErrorWithoutPath(t *testing.T) {
	if got := (Problem{Message: "the file is empty"}).Error(); got != "the file is empty" {
		t.Errorf("Error() = %q", got)
	}
}
