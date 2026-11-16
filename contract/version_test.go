package contract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestContractVersion_Check(t *testing.T) {
	cases := map[string]bool{
		"1.0":   true,
		"1.1":   false, // a newer minor than the core's (§2)
		"0.9":   false,
		"2.0":   false,
		"1":     false,
		"1.":    false,
		".0":    false,
		"01.0":  false,
		"1.00":  false,
		"1.-1":  false,
		"1.0.0": false,
		"v1.0":  false,
		"":      false,
	}
	for v, ok := range cases {
		err := CheckVersion(v)
		if (err == nil) != ok {
			t.Errorf("CheckVersion(%q) = %v, want ok=%v", v, err, ok)
		}
		if err != nil && !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("CheckVersion(%q) error %v does not wrap ErrUnsupportedVersion", v, err)
		}
	}
	if ma, mi, err := ParseVersion("1.12"); err != nil || ma != 1 || mi != 12 {
		t.Errorf("ParseVersion(1.12) = %d, %d, %v", ma, mi, err)
	}
	if ma, mi, err := ParseVersion(Version); err != nil || ma != Major || mi != Minor {
		t.Errorf("Version %q does not match Major.Minor", Version)
	}
}

// TestContractVersion_InputIgnoresUnknownFields proves §2: handlers ignore
// unknown input fields, so a 1.x input with new fields still parses.
func TestContractVersion_InputIgnoresUnknownFields(t *testing.T) {
	var d map[string]any
	if err := json.Unmarshal(readExample(t, "input-review.json"), &d); err != nil {
		t.Fatal(err)
	}
	d["contract_version"] = "1.3"
	d["new_top_level"] = map[string]any{"x": 1}
	d["review"].(map[string]any)["new_review_field"] = "y"
	b, _ := json.Marshal(d)
	in, err := ParseInput(b)
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	if in.Stage != StageReview || in.Review == nil || in.Review.Commit == "" || len(in.Review.RuleFindings) != 2 {
		t.Errorf("parsed %+v", in)
	}
	if err := schemaCheck(t, InputSchema, b); err != nil {
		t.Errorf("input schema rejects unknown fields: %v", err)
	}
}

func TestContractVersion_InputOtherMajorRejected(t *testing.T) {
	for _, v := range []string{"2.0", "0.1", "x", ""} {
		b := []byte(`{"contract_version":"` + v + `","stage":"implement"}`)
		if _, err := ParseInput(b); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("ParseInput with version %q: %v, want ErrUnsupportedVersion", v, err)
		}
	}
	if _, err := ParseInput([]byte(`{`)); err == nil || errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("ParseInput of bad JSON: %v", err)
	}
}

func TestInputExamplesParse(t *testing.T) {
	for _, n := range []string{"input-implement.json", "input-review.json", "input-review-base-update.json", "input-address.json", "input-mint.json"} {
		in, err := ParseInput(readExample(t, n))
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if !strings.Contains(n, string(in.Stage)) {
			t.Errorf("%s: stage %q", n, in.Stage)
		}
	}
	in, _ := ParseInput(readExample(t, "input-implement.json"))
	if in.Change != nil {
		t.Error("implement example has a change")
	}
	in, _ = ParseInput(readExample(t, "input-review-base-update.json"))
	if in.Review.Round != nil || len(in.Review.ResolvedPaths) != 1 {
		t.Errorf("base_update review input %+v", in.Review)
	}
}

func TestInputSchemaRejects(t *testing.T) {
	var base map[string]any
	if err := json.Unmarshal(readExample(t, "input-review.json"), &base); err != nil {
		t.Fatal(err)
	}
	edits := map[string]func(d map[string]any){
		"version 2.0":       func(d map[string]any) { d["contract_version"] = "2.0" },
		"unknown stage":     func(d map[string]any) { d["stage"] = "materialise" },
		"attempt 0":         func(d map[string]any) { d["attempt"] = 0 },
		"bad deadline":      func(d map[string]any) { d["deadline"] = "tomorrow" },
		"review missing":    func(d map[string]any) { delete(d, "review") },
		"no run_id":         func(d map[string]any) { delete(d, "run_id") },
		"bad review kind":   func(d map[string]any) { d["review"].(map[string]any)["kind"] = "x" },
		"duplicate rule id": func(d map[string]any) { d["review"].(map[string]any)["rule_findings"] = []any{"F3", "F3"} },
	}
	for name, edit := range edits {
		var d map[string]any
		b, _ := json.Marshal(base)
		_ = json.Unmarshal(b, &d)
		edit(d)
		b, _ = json.Marshal(d)
		if err := schemaCheck(t, InputSchema, b); err == nil {
			t.Errorf("%s: input schema accepted it", name)
		}
	}
}

func TestDecodeStrictRejectsTrailingData(t *testing.T) {
	var v struct{ A int }
	if err := decodeStrict([]byte(`{"A":1} 2`), &v); err == nil {
		t.Error("trailing data accepted")
	}
	if err := decodeStrict([]byte(`{"A":1}`), &v); err != nil || v.A != 1 {
		t.Errorf("decodeStrict: %v", err)
	}
}
