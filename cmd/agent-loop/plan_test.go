package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/contract/tasklist"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// validTasks is a valid task list for a plan directory named "auth".
const validTasks = `version: 1
plan: auth
tasks:
  - key: login-api
    type: feature
    title: Add login endpoint
    body: tasks/login-api.md
  - key: login-ui
    type: bug
    title: Login screen
    body: tasks/login-ui.md
    depends_on: [login-api]
`

// writePlan writes plans/<key>/ under a temp directory with tasks.yaml and
// the named body files, and returns the plan directory.
func writePlan(t *testing.T, key, tasks string, bodies ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plans", key)
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# Plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.yaml"), []byte(tasks), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, b := range bodies {
		if err := os.WriteFile(filepath.Join(dir, b), []byte("Body of "+b+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeProfile writes cleanProfile plus extra and returns its path.
func writeProfile(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".agent-loop.toml")
	if err := os.WriteFile(p, []byte(extra+cleanProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFR17AC6_PlanValidateValidPlanExitsZero(t *testing.T) {
	dir := writePlan(t, "auth", validTasks, "tasks/login-api.md", "tasks/login-ui.md")
	code, out, errOut := runCLI("plan", "validate", "--profile", writeProfile(t, ""), dir)
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if out != dir+": valid, 2 task(s)\n" {
		t.Errorf("stdout %q", out)
	}
	// A trailing slash, as the CI step passes plans/*/, names the same plan.
	if code, out, _ := runCLI("plan", "validate", "--profile", writeProfile(t, ""), dir+"/"); code != 0 {
		t.Errorf("with a trailing slash: code %d\n%s", code, out)
	}
}

// TestFR17AC6_PlanValidateReadsProfileInCurrentDirectory: without --profile
// or --project, the profile is .agent-loop.toml in the current directory
// (work plan #166 D1).
func TestFR17AC6_PlanValidateReadsProfileInCurrentDirectory(t *testing.T) {
	dir := writePlan(t, "auth", validTasks, "tasks/login-api.md", "tasks/login-ui.md")
	prof := writeProfile(t, "")
	t.Chdir(filepath.Dir(prof))
	if code, out, errOut := runCLI("plan", "validate", dir); code != 0 {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	// The profile's types are the ones used.
	if err := os.WriteFile(prof, []byte(`types = ["feature"]`+"\n[follow_up]\ntype = \"feature\"\n"+cleanProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("plan", "validate", dir)
	if code != 1 || !strings.Contains(out, `error   type: tasks[1].type: type "bug" is not configured (configured: feature)`) {
		t.Errorf("code %d\n%s", code, out)
	}
}

// TestFR17AC6_PlanValidateNamesEachInvalidCase: each INTEGRATION-SPEC §7
// invalid case exits 1 and names the problem's rule and location.
func TestFR17AC6_PlanValidateNamesEachInvalidCase(t *testing.T) {
	big := strings.Repeat("x", tasklist.MaxBody+1)
	cases := []struct {
		name, key, tasks string
		bodies           []string
		setup            func(t *testing.T, dir string)
		want             string
	}{
		{name: "schema", key: "auth", tasks: "version: 2\nplan: auth\ntasks: []\n",
			want: "error   schema: version:"},
		{name: "not YAML", key: "auth", tasks: "tasks: [\n",
			want: "error   schema:"},
		{name: "plan not the directory", key: "other", tasks: validTasks, bodies: []string{"tasks/login-api.md", "tasks/login-ui.md"},
			want: `error   plan: plan: plan "auth" is not the plan directory's name "other"`},
		{name: "duplicate key", key: "auth", tasks: strings.NewReplacer("key: login-ui", "key: login-api", "    depends_on: [login-api]\n", "").Replace(validTasks), bodies: []string{"tasks/login-api.md", "tasks/login-ui.md"},
			want: `error   duplicate_key: tasks[1].key: key "login-api" is also the key of tasks[0]`},
		{name: "type not configured", key: "auth", tasks: strings.Replace(validTasks, "type: bug", "type: chore", 1), bodies: []string{"tasks/login-api.md", "tasks/login-ui.md"},
			want: `error   type: tasks[1].type: type "chore" is not configured`},
		{name: "body missing", key: "auth", tasks: validTasks, bodies: []string{"tasks/login-api.md"},
			want: `error   body_missing: tasks[1].body: body file "tasks/login-ui.md" does not exist`},
		{name: "body over 64 KiB", key: "auth", tasks: validTasks, bodies: []string{"tasks/login-api.md"},
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "tasks/login-ui.md"), []byte(big), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: `error   body_too_large: tasks[1].body: body file "tasks/login-ui.md" is larger than 64 KiB`},
		{name: "body through a symlink", key: "auth", tasks: validTasks, bodies: []string{"tasks/login-api.md"},
			setup: func(t *testing.T, dir string) {
				outside := filepath.Join(t.TempDir(), "secret.md")
				if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "tasks/login-ui.md")); err != nil {
					t.Fatal(err)
				}
			},
			want: `error   body_missing: tasks[1].body: body file "tasks/login-ui.md" is not a regular file`},
		{name: "unknown key", key: "auth", tasks: strings.Replace(validTasks, "[login-api]", "[login-db]", 1), bodies: []string{"tasks/login-api.md", "tasks/login-ui.md"},
			want: `error   unknown_key: tasks[1].depends_on[0]: no task in this plan has key "login-db"`},
		{name: "cycle", key: "auth", tasks: strings.Replace(validTasks, "body: tasks/login-api.md\n", "body: tasks/login-api.md\n    depends_on: [login-ui]\n", 1), bodies: []string{"tasks/login-api.md", "tasks/login-ui.md"},
			want: "error   cycle:"},
	}
	prof := writeProfile(t, "")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writePlan(t, c.key, c.tasks, c.bodies...)
			if c.setup != nil {
				c.setup(t, dir)
			}
			code, out, errOut := runCLI("plan", "validate", "--profile", prof, dir)
			if code != 1 || errOut != "" {
				t.Errorf("code %d, stderr %q", code, errOut)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q in\n%s", c.want, out)
			}
			if !strings.HasSuffix(out, dir+": 1 problem(s)\n") {
				t.Errorf("want one problem and the summary:\n%s", out)
			}
		})
	}
}

// TestFR17AC6_PlanValidateListsEveryProblem: all problems are listed, not
// just the first.
func TestFR17AC6_PlanValidateListsEveryProblem(t *testing.T) {
	tasks := strings.NewReplacer("type: bug", "type: chore", "[login-api]", "[login-db]").Replace(validTasks)
	dir := writePlan(t, "auth", tasks, "tasks/login-api.md")
	code, out, _ := runCLI("plan", "validate", "--profile", writeProfile(t, ""), dir)
	if code != 1 {
		t.Errorf("code %d", code)
	}
	for _, want := range []string{"error   type:", "error   body_missing:", "error   unknown_key:", dir + ": 3 problem(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// TestFR17AC6_PlanValidateTasksFile: a missing, symlinked or non-regular
// tasks.yaml exits 1 and names it (work plan #166 D5).
func TestFR17AC6_PlanValidateTasksFile(t *testing.T) {
	prof := writeProfile(t, "")
	cases := map[string]struct {
		setup func(t *testing.T, dir string)
		want  string
	}{
		"missing": {func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, "tasks.yaml"))) }, "has no tasks.yaml"},
		"symlink": {func(t *testing.T, dir string) {
			real := filepath.Join(t.TempDir(), "tasks.yaml")
			must(t, os.WriteFile(real, []byte(validTasks), 0o600))
			must(t, os.Remove(filepath.Join(dir, "tasks.yaml")))
			must(t, os.Symlink(real, filepath.Join(dir, "tasks.yaml")))
		}, "tasks.yaml is a symbolic link"},
		"directory": {func(t *testing.T, dir string) {
			must(t, os.Remove(filepath.Join(dir, "tasks.yaml")))
			must(t, os.Mkdir(filepath.Join(dir, "tasks.yaml"), 0o700))
		}, "tasks.yaml is not a regular file"},
		"no directory": {func(t *testing.T, dir string) { must(t, os.RemoveAll(dir)) }, "has no tasks.yaml"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writePlan(t, "auth", validTasks, "tasks/login-api.md", "tasks/login-ui.md")
			c.setup(t, dir)
			code, out, errOut := runCLI("plan", "validate", "--profile", prof, dir)
			if code != 1 || out != "" || !strings.HasPrefix(errOut, "agent-loop plan validate: ") || !strings.Contains(errOut, c.want) {
				t.Errorf("code %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestFR17AC6_PlanValidateProfileProblems: a missing or invalid profile
// exits 1 and says why.
func TestFR17AC6_PlanValidateProfileProblems(t *testing.T) {
	dir := writePlan(t, "auth", validTasks, "tasks/login-api.md", "tasks/login-ui.md")
	t.Chdir(t.TempDir())
	code, _, errOut := runCLI("plan", "validate", dir)
	if code != 1 || !strings.Contains(errOut, ".agent-loop.toml") {
		t.Errorf("no profile: code %d, stderr %q", code, errOut)
	}
	bad := writeProfile(t, "wip = \"many\"\n")
	code, _, errOut = runCLI("plan", "validate", "--profile", bad, dir)
	if code != 1 || !strings.Contains(errOut, "wip") {
		t.Errorf("bad profile: code %d, stderr %q", code, errOut)
	}
}

func TestPlanValidateUsage(t *testing.T) {
	for _, args := range [][]string{
		{"plan"},
		{"plan", "frobnicate"},
		{"plan", "validate"},
		{"plan", "validate", "a", "b"},
		{"plan", "validate", "--profile", "p", "--project", "x", "a"},
		{"plan", "validate", "--config", "c", "a"},
		{"plan", "validate", "--no-such-flag", "a"},
	} {
		if code, out, _ := runCLI(args...); code != 2 || out != "" {
			t.Errorf("%v: code %d, stdout %q", args, code, out)
		}
	}
}

func TestPlanAbandonNotImplemented(t *testing.T) {
	code, out, errOut := runCLI("plan", "abandon", "auth")
	if code != 1 || out != "" || errOut != "agent-loop plan abandon: not implemented yet (v0.2, FR-17 AC9)\n" {
		t.Errorf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// externalTasks depends on an item outside the plan.
func externalTasks(ref string) string {
	return strings.Replace(validTasks, "[login-api]", fmt.Sprintf("[login-api, %q]", ref), 1)
}

// TestFR17AC6_PlanValidateExternalRefsUncheckedWithoutProject: without
// --project no tracker is asked, and the output says so (work plan #166
// D2).
func TestFR17AC6_PlanValidateExternalRefsUncheckedWithoutProject(t *testing.T) {
	dir := writePlan(t, "auth", externalTasks("#999"), "tasks/login-api.md", "tasks/login-ui.md")
	code, out, _ := runCLI("plan", "validate", "--profile", writeProfile(t, ""), dir)
	if code != 0 || !strings.Contains(out, "note: 1 external depends_on ref(s) not checked; pass --project <name> to check them on the tracker\n") {
		t.Errorf("code %d\n%s", code, out)
	}
}

// TestFR17AC6_PlanValidateChecksExternalRefsWithProject: with --project,
// external refs are looked up on the project's tracker; "#17" is item 17
// (work plan #166 D2, D3).
func TestFR17AC6_PlanValidateChecksExternalRefsWithProject(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	ref, err := tp.f.NewItem(fake.Owner, "existing", "")
	must(t, err)

	dir := writePlan(t, "auth", externalTasks("#"+string(ref)), "tasks/login-api.md", "tasks/login-ui.md")
	code, out, errOut := runCLI("plan", "validate", "--config", tp.cfg, "--project", "widgets", dir)
	if code != 0 || errOut != "" || strings.Contains(out, "note:") {
		t.Errorf("existing item: code %d\nstdout %s\nstderr %s", code, out, errOut)
	}

	dir = writePlan(t, "auth", externalTasks("#999"), "tasks/login-api.md", "tasks/login-ui.md")
	code, out, _ = runCLI("plan", "validate", "--config", tp.cfg, "--project", "widgets", dir)
	if code != 1 || !strings.Contains(out, `error   missing_item: tasks[1].depends_on[1]: item "#999" does not exist`) {
		t.Errorf("missing item: code %d\n%s", code, out)
	}
}

// failingTracker fails every Item read with a transient error.
type failingTracker struct{ ports.WorkTracker }

func (failingTracker) Item(context.Context, ports.ItemRef) (ports.Item, error) {
	return ports.Item{}, fmt.Errorf("rate limited: %w", ports.ErrTransient)
}

// TestFR17AC6_PlanValidateTrackerErrors: with --project, a tracker that
// can't be opened or read exits 1 and names the error (work plan #166 D2).
func TestFR17AC6_PlanValidateTrackerErrors(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	dir := writePlan(t, "auth", externalTasks("#5"), "tasks/login-api.md", "tasks/login-ui.md")

	openAdapters = func(context.Context, *localProject) (*projectAdapters, []ports.Problem, error) {
		return &projectAdapters{tracker: failingTracker{tp.f.Tracker()}, host: tp.f.Host(), close: func() {}}, nil, nil
	}
	code, _, errOut := runCLI("plan", "validate", "--config", tp.cfg, "--project", "widgets", dir)
	if code != 1 || !strings.Contains(errOut, `can't check item "#5": rate limited`) {
		t.Errorf("read error: code %d, stderr %q", code, errOut)
	}

	openAdapters = func(context.Context, *localProject) (*projectAdapters, []ports.Problem, error) {
		return nil, []ports.Problem{{Severity: ports.SeverityError, Code: "identity_unreadable", Message: "can't read the loop identity"}}, nil
	}
	code, _, errOut = runCLI("plan", "validate", "--config", tp.cfg, "--project", "widgets", dir)
	if code != 1 || !strings.Contains(errOut, "can't open project widgets's tracker to check external refs: can't read the loop identity") {
		t.Errorf("identity problem: code %d, stderr %q", code, errOut)
	}

	openAdapters = func(context.Context, *localProject) (*projectAdapters, []ports.Problem, error) {
		return nil, nil, errors.New("no network")
	}
	code, _, errOut = runCLI("plan", "validate", "--config", tp.cfg, "--project", "widgets", dir)
	if code != 1 || !strings.Contains(errOut, "no network") {
		t.Errorf("open error: code %d, stderr %q", code, errOut)
	}

	code, _, errOut = runCLI("plan", "validate", "--config", tp.cfg, "--project", "gadgets", dir)
	if code != 1 || !strings.Contains(errOut, `project "gadgets" isn't registered`) {
		t.Errorf("unknown project: code %d, stderr %q", code, errOut)
	}
}
