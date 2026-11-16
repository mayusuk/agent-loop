package ports

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// eventRenames maps the spec's event names to their Go names where two
// events share a name (D5).
var eventRenames = map[string]map[string]string{
	"ItemEvent":   {"Closed": "ItemClosed", "Reopened": "ItemReopened"},
	"ChangeEvent": {"Closed": "ChangeClosed", "Reopened": "ChangeReopened"},
}

var specVariant = regexp.MustCompile("`([A-Z][A-Za-z]*)(?:\\{([^}]*)\\})?`")

// specEventVariants reads "`<event>` is one of ..." in INTEGRATION-SPEC §8
// and returns each variant's Go name (after D5) with its field names.
func specEventVariants(t *testing.T, event string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "- `" + event + "` is one of "
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, prefix) {
			line = strings.TrimPrefix(l, prefix)
		}
	}
	if line == "" {
		t.Fatalf("INTEGRATION-SPEC has no line %q", prefix)
	}
	line, _, _ = strings.Cut(line, ". ") // the variant list ends the first sentence
	out := map[string][]string{}
	for _, m := range specVariant.FindAllStringSubmatch(line, -1) {
		name := m[1]
		if r, ok := eventRenames[event][name]; ok {
			name = r
		}
		fields := []string{}
		if m[2] != "" {
			for _, f := range strings.Split(m[2], ",") {
				fields = append(fields, strings.Fields(f)[0])
			}
		}
		out[name] = fields
	}
	return out
}

// codeEventVariants returns every type in this package with the marker
// method (itemEvent or changeEvent), with its field names.
func codeEventVariants(t *testing.T, marker string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	structs := map[string]*ast.StructType{}
	var receivers []string
	for _, f := range pkgs["ports"].Files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok {
						if st, ok := ts.Type.(*ast.StructType); ok {
							structs[ts.Name.Name] = st
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != marker {
					continue
				}
				typ := d.Recv.List[0].Type
				if st, ok := typ.(*ast.StarExpr); ok {
					typ = st.X
				}
				receivers = append(receivers, typ.(*ast.Ident).Name)
			}
		}
	}
	out := map[string][]string{}
	for _, r := range receivers {
		st, ok := structs[r]
		if !ok {
			t.Fatalf("%s has %s() but isn't a struct", r, marker)
		}
		fields := []string{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fields = append(fields, n.Name)
			}
		}
		out[r] = fields
	}
	return out
}

func keys(m map[string][]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// TestEventVariantsMatchIntegrationSpec compares the event variants, and
// each one's fields, with INTEGRATION-SPEC §8.2 and §8.3. A variant added or
// removed on either side fails this test.
func TestEventVariantsMatchIntegrationSpec(t *testing.T) {
	for event, marker := range map[string]string{"ItemEvent": "itemEvent", "ChangeEvent": "changeEvent"} {
		spec, code := specEventVariants(t, event), codeEventVariants(t, marker)
		if !reflect.DeepEqual(keys(code), keys(spec)) {
			t.Errorf("%s variants:\n got  %v\n want %v", event, keys(code), keys(spec))
			continue
		}
		for name, want := range spec {
			if got := code[name]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s fields = %v, spec says %v", name, got, want)
			}
		}
	}
}

func TestSpecEventVariantsParse(t *testing.T) {
	got := specEventVariants(t, "ItemEvent")
	if f, ok := got["AssigneeChanged"]; !ok || !reflect.DeepEqual(f, []string{"Actor", "Human"}) {
		t.Errorf("AssigneeChanged = %v, %v", f, ok)
	}
	if f, ok := got["ItemReopened"]; !ok || len(f) != 0 {
		t.Errorf("ItemReopened = %v, %v", f, ok)
	}
	if len(got) != 6 || len(specEventVariants(t, "ChangeEvent")) != 10 {
		t.Errorf("parsed %d item and %d change variants, want 6 and 10", len(got), len(specEventVariants(t, "ChangeEvent")))
	}
}

func TestEventDetailTypeSwitch(t *testing.T) {
	e := ItemEvent{Item: "42", Version: "v2", Detail: MarkerChanged{Actor: Actor{ID: "1", Kind: ActorHuman}, Marker: MarkerReady, On: true}}
	if mc, ok := e.Detail.(MarkerChanged); !ok || mc.Marker != MarkerReady || !mc.On {
		t.Fatalf("type switch on detail lost data: %+v", e.Detail)
	}
}
