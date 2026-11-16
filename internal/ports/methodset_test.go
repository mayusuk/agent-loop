package ports

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// specPath is the source of truth for WorkTracker, CodeHost and the common
// types (INTEGRATION-SPEC §8).
var specPath = filepath.Join("..", "..", "specs", "INTEGRATION-SPEC.md")

var goBlock = regexp.MustCompile("(?s)```go\n(.*?)```")

// specDecls parses every Go code block of INTEGRATION-SPEC §8 and returns its
// type declarations by name.
func specDecls(t *testing.T) map[string]*ast.TypeSpec {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	start := strings.Index(text, "\n## 8. Ports")
	end := strings.Index(text, "\n## 9. ")
	if start < 0 || end < start {
		t.Fatal("INTEGRATION-SPEC §8 not found")
	}
	decls := map[string]*ast.TypeSpec{}
	fset := token.NewFileSet()
	for i, m := range goBlock.FindAllStringSubmatch(text[start:end], -1) {
		f, err := parser.ParseFile(fset, "spec.go", "package spec\n"+m[1], 0)
		if err != nil {
			t.Fatalf("§8 Go block %d doesn't parse: %v", i+1, err)
		}
		collect(f, decls)
	}
	return decls
}

// codeDecls returns the type declarations of this package's non-test files.
func codeDecls(t *testing.T) map[string]*ast.TypeSpec {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string]*ast.TypeSpec{}
	for _, f := range pkgs["ports"].Files {
		collect(f, decls)
	}
	return decls
}

func collect(f *ast.File, into map[string]*ast.TypeSpec) {
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.TYPE {
			for _, s := range g.Specs {
				ts := s.(*ast.TypeSpec)
				into[ts.Name.Name] = ts
			}
		}
	}
}

var spaces = regexp.MustCompile(`[\s;]+`)

// render prints a node without comments, with layout normalised.
func render(t *testing.T, n ast.Node) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), n); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(spaces.ReplaceAllString(buf.String(), " "))
}

// methods returns "Name(params) results" for each method of an interface, in
// declaration order.
func methods(t *testing.T, ts *ast.TypeSpec) []string {
	t.Helper()
	it, ok := ts.Type.(*ast.InterfaceType)
	if !ok {
		t.Fatalf("%s is not an interface", ts.Name.Name)
	}
	var out []string
	for _, m := range it.Methods.List {
		for _, n := range m.Names {
			out = append(out, n.Name+strings.TrimPrefix(render(t, m.Type), "func"))
		}
	}
	return out
}

// TestPortsMatchIntegrationSpec compares WorkTracker and CodeHost with the
// INTEGRATION-SPEC §8 code blocks, method for method and in order, so drift
// on either side fails this test.
func TestPortsMatchIntegrationSpec(t *testing.T) {
	spec, code := specDecls(t), codeDecls(t)
	for _, name := range []string{"WorkTracker", "CodeHost"} {
		s, ok := spec[name]
		if !ok {
			t.Fatalf("INTEGRATION-SPEC §8 has no %s", name)
		}
		c, ok := code[name]
		if !ok {
			t.Fatalf("internal/ports has no %s", name)
		}
		if got, want := methods(t, c), methods(t, s); !reflect.DeepEqual(got, want) {
			t.Errorf("%s drifted from INTEGRATION-SPEC §8:\n got  %s\n want %s",
				name, strings.Join(got, "\n      "), strings.Join(want, "\n      "))
		}
	}
}

// TestCommonTypesMatchIntegrationSpec compares the §8.1 common types with
// their declarations here. ActorKind's values are given in a comment in the
// spec and checked in TestActorKinds.
func TestCommonTypesMatchIntegrationSpec(t *testing.T) {
	spec, code := specDecls(t), codeDecls(t)
	want := []string{"ItemRef", "ChangeRef", "Commit", "Actor", "Marker", "Version", "Cursor"}
	var got []string
	for name := range spec {
		if _, isIface := spec[name].Type.(*ast.InterfaceType); !isIface {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("§8.1 declares %v, this test knows %v; update both", got, sorted)
	}
	for _, name := range want {
		c, ok := code[name]
		if !ok {
			t.Errorf("internal/ports has no %s", name)
			continue
		}
		if g, w := render(t, c.Type), render(t, spec[name].Type); g != w {
			t.Errorf("%s = %s, spec says %s", name, g, w)
		}
	}
}

func TestMarkersMatchIntegrationSpec(t *testing.T) {
	// The §8.1 comment on Marker.
	want := []Marker{"ready", "in_progress", "blocked", "needs_owner", "paused", "aborted",
		"follow_up", "plan", "plan_proposal", "type:feature", "type:bug"}
	got := []Marker{MarkerReady, MarkerInProgress, MarkerBlocked, MarkerNeedsOwner, MarkerPaused, MarkerAborted,
		MarkerFollowUp, MarkerPlan, MarkerPlanProposal, MarkerTypeFeature, MarkerTypeBug}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("markers = %v, want %v", got, want)
	}
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range want {
		if !strings.Contains(string(b), `"`+string(m)+`"`) {
			t.Errorf("marker %q is not in INTEGRATION-SPEC", m)
		}
	}
}

func TestActorKinds(t *testing.T) {
	if ActorHuman == ActorBot || ActorHuman == "" || ActorBot == "" {
		t.Fatal("ActorHuman and ActorBot must be distinct and non-empty")
	}
}

func TestRecordTypesMatchIntegrationSpec(t *testing.T) {
	// The §6.1 table, in order.
	want := []RecordType{"change", "review", "finding", "reply", "manual_reply", "status",
		"approved_commits", "followup", "task", "plan", "proposal"}
	got := []RecordType{RecordChange, RecordReview, RecordFinding, RecordReply, RecordManualReply, RecordStatus,
		RecordApprovedCommits, RecordFollowUp, RecordTask, RecordPlan, RecordProposal}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record types = %v, want %v", got, want)
	}
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range want {
		if !strings.Contains(string(b), "| `"+string(r)+"` |") {
			t.Errorf("record type %q is not a row of INTEGRATION-SPEC §6.1", r)
		}
	}
}

// signatures lists an interface's methods by reflection.
func signatures(iface any) []string {
	typ := reflect.TypeOf(iface).Elem()
	var out []string
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		var in, res []string
		for j := 0; j < m.Type.NumIn(); j++ {
			in = append(in, m.Type.In(j).String())
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			res = append(res, m.Type.Out(j).String())
		}
		out = append(out, m.Name+"("+strings.Join(in, ", ")+") ("+strings.Join(res, ", ")+")")
	}
	return out
}

// TestInternalPortsMethodSets lists the method sets of the TECHNICAL-SPEC §5
// ports. The spec gives a sketch, not compilable Go, so the sets are written
// out here, one method per line of the sketch.
func TestInternalPortsMethodSets(t *testing.T) {
	cases := []struct {
		iface any
		want  []string
	}{
		{(*Workspace)(nil), []string{
			"Prepare(context.Context, ports.RepoRef, ports.Branch) (ports.Dir, error)",
			"Release(context.Context, ports.Dir) (error)",
		}},
		{(*Runner)(nil), []string{
			"Run(context.Context, ports.RunSpec) (ports.RunOutcome, error)",
		}},
		{(*SecretStore)(nil), []string{
			"Revoke(context.Context, ports.Token) (error)",
			"Token(context.Context, ports.Identity, ports.Scope) (ports.Token, error)",
		}},
		{(*Notifier)(nil), []string{
			"Notify(context.Context, ports.Notification) (error)",
		}},
	}
	for _, c := range cases {
		if got := signatures(c.iface); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%T:\n got  %q\n want %q", c.iface, got, c.want)
		}
	}
}
