package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	sha0 = "9f2c1e4b7a3d5f6e8c0b1a2d3e4f5a6b7c8d9e0f" // input head
	sha1 = "2b7d3c4e5f60718293a4b5c6d7e8f90a1b2c3d4e" // new head
)

// doc is a JSON document as generic maps, so a test can change any field.
type doc = map[string]any

func load(t *testing.T, name string) doc {
	t.Helper()
	var d doc
	if err := json.Unmarshal(readExample(t, name), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// set sets the value at a dotted path ("done.rulings.0.status").
func set(d doc, path string, v any) {
	parts := strings.Split(path, ".")
	var cur any = d
	for _, p := range parts[:len(parts)-1] {
		cur = step(cur, p)
	}
	last := parts[len(parts)-1]
	switch c := cur.(type) {
	case map[string]any:
		c[last] = v
	case []any:
		i, _ := strconv.Atoi(last)
		c[i] = v
	}
}

// del removes the key at a dotted path.
func del(d doc, path string) {
	parts := strings.Split(path, ".")
	var cur any = d
	for _, p := range parts[:len(parts)-1] {
		cur = step(cur, p)
	}
	delete(cur.(map[string]any), parts[len(parts)-1])
}

func step(cur any, p string) any {
	switch c := cur.(type) {
	case map[string]any:
		return c[p]
	case []any:
		i, _ := strconv.Atoi(p)
		return c[i]
	}
	panic("bad path " + p)
}

// inputFor returns the input example for a stage, decoded.
func inputFor(t *testing.T, stage Stage) doc {
	t.Helper()
	return load(t, "input-"+string(stage)+".json")
}

// resultFor returns the valid done result example for a stage.
func resultFor(t *testing.T, stage Stage) doc {
	t.Helper()
	return load(t, "result-"+string(stage)+"-done.json")
}

func asOutcome(r doc, outcome string, obj any) doc {
	for _, k := range []string{"done", "complete", "blocked", "failed", "needs_owner", "usage"} {
		delete(r, k)
	}
	r["result"] = outcome
	r[outcome] = obj
	return r
}

type vcase struct {
	name  string
	stage Stage
	// edit changes the input and the valid result example for the stage.
	edit func(in, r doc, out string)
	// raw, when set, replaces the result bytes.
	raw string
	// want is a substring of the error, or "" for a valid result.
	want string
	// shape marks a failing case that result.schema.json must reject too.
	shape bool
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFR18AC3_ValidateResult covers every rule of INTEGRATION-SPEC §5 with a
// passing and a failing case. A failing result becomes failed with a reason.
func TestFR18AC3_ValidateResult(t *testing.T) {
	var fileT *testing.T // set per case, for edits that write files
	cases := []vcase{
		// §5.1 shape and version.
		{name: "implement done is valid", stage: StageImplement},
		{name: "review done is valid", stage: StageReview},
		{name: "address done is valid", stage: StageAddress},
		{name: "mint done is valid", stage: StageMint},
		{name: "empty file", stage: StageImplement, raw: " ", want: "not a JSON object"},
		{name: "not JSON", stage: StageImplement, raw: "{nope", want: "not valid JSON"},
		{name: "array", stage: StageImplement, raw: "[]", want: "not a JSON object"},
		{name: "trailing data", stage: StageImplement, raw: `{"contract_version":"1.0","run_id":"item/eagain/42#1:implement:1:a","result":"failed","failed":{"reason":"x"}} {}`, want: "not valid JSON"},
		{name: "contract_version missing", stage: StageImplement, edit: func(in, r doc, _ string) { del(r, "contract_version") }, want: "contract_version is missing", shape: true},
		{name: "contract 1.1 is unsupported", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "contract_version", "1.1") }, want: "unsupported contract version", shape: true},
		{name: "contract 2.0 is unsupported", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "contract_version", "2.0") }, want: "unsupported contract version", shape: true},
		{name: "contract 1 is malformed", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "contract_version", "1") }, want: "unsupported contract version", shape: true},
		{name: "contract 1.00 is malformed", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "contract_version", "1.00") }, want: "unsupported contract version", shape: true},
		{name: "unknown top-level field", stage: StageImplement, edit: func(in, r doc, _ string) { r["extra"] = 1 }, want: `unknown field "extra"`, shape: true},
		{name: "unknown field in done", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.extra", 1) }, want: `unknown field "extra"`, shape: true},
		{name: "unknown field in nested object", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.0.extra", 1) }, want: `unknown field "extra"`, shape: true},
		// §2: keys match exactly; Go's case-insensitive matching must not let a case variant through (review R1).
		{name: "wrong-case object and field keys", stage: StageImplement, raw: `{"contract_version":"1.0","run_id":"item/eagain/42#1:implement:1:a","result":"blocked","Blocked":{"Reason_File":"reason.md"}}`, want: `unknown field "Blocked"`, shape: true},
		{name: "all-caps top-level keys", stage: StageImplement, raw: `{"CONTRACT_VERSION":"1.0","RUN_ID":"item/eagain/42#1:implement:1:a","RESULT":"blocked","BLOCKED":{"reason_file":"reason.md"}}`, want: "contract_version is missing", shape: true},
		{name: "wrong-case version key beside the right one", stage: StageImplement, edit: func(in, r doc, _ string) { r["Contract_Version"] = "1.0" }, want: `unknown field "Contract_Version"`, shape: true},
		{name: "wrong-case done key", stage: StageImplement, edit: func(in, r doc, _ string) { r["Done"] = r["done"]; delete(r, "done") }, want: `unknown field "Done"`, shape: true},
		{name: "wrong-case run_id key", stage: StageImplement, edit: func(in, r doc, _ string) { r["Run_ID"] = r["run_id"]; delete(r, "run_id") }, want: `unknown field "Run_ID"`, shape: true},
		{name: "wrong-case key in done", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.Head", sha1); del(r, "done.head") }, want: `unknown field "Head"`, shape: true},
		{name: "wrong-case key in nested object", stage: StageImplement, edit: func(in, r doc, _ string) {
			set(r, "done.change.Body_File", "change.md")
			del(r, "done.change.body_file")
		}, want: `unknown field "Body_File"`, shape: true},
		{name: "wrong-case key in array item", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.0.Status", "open"); del(r, "done.rulings.0.status") }, want: `unknown field "Status"`, shape: true},
		{name: "wrong-case key in usage", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.Cost", 1) }, want: `unknown field "Cost"`, shape: true},
		{name: "unknown result", stage: StageImplement, edit: func(in, r doc, _ string) { r["result"] = "maybe"; delete(r, "done") }, want: "not a known result", shape: true},
		{name: "run_id differs", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "run_id", "item/eagain/42#1:implement:1:b") }, want: "run_id"},
		{name: "stage without handler", stage: StageImplement, edit: func(in, r doc, _ string) { set(in, "stage", "materialise") }, want: "has no handler"},

		// FR-18 AC1: results allowed per stage.
		{name: "implement blocked allowed", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"reason_file": "reason.md"}) }},
		{name: "address blocked allowed", stage: StageAddress, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"reason_file": "reason.md"}) }},
		{name: "review blocked not allowed", stage: StageReview, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"reason_file": "reason.md"}) }, want: `"blocked" is not allowed from review`},
		{name: "mint blocked not allowed", stage: StageMint, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"reason_file": "reason.md"}) }, want: "not allowed from mint"},
		{name: "mint complete allowed", stage: StageMint, edit: func(in, r doc, _ string) { asOutcome(r, "complete", doc{}) }},
		{name: "implement complete not allowed", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "complete", doc{}) }, want: "not allowed from implement"},
		{name: "address complete not allowed", stage: StageAddress, edit: func(in, r doc, _ string) { asOutcome(r, "complete", doc{}) }, want: "not allowed from address"},
		{name: "review failed allowed", stage: StageReview, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{"reason": "x"}) }},
		{name: "review needs_owner allowed", stage: StageReview, edit: func(in, r doc, _ string) { asOutcome(r, "needs_owner", doc{"question_file": "question.md"}) }},

		// §5.1: only the named object, and it must be there.
		{name: "done without done object", stage: StageImplement, edit: func(in, r doc, _ string) { del(r, "done") }, want: "no done object", shape: true},
		{name: "complete without complete object", stage: StageMint, edit: func(in, r doc, _ string) { asOutcome(r, "complete", doc{}); del(r, "complete") }, want: "no complete object", shape: true},
		{name: "failed with a done object too", stage: StageImplement, edit: func(in, r doc, _ string) { r["result"] = "failed"; r["failed"] = doc{"reason": "x"} }, want: "a done object is present", shape: true},
		{name: "null object counts as present", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{"reason": "x"}); r["blocked"] = nil }, want: "blocked is not an object", shape: true},
		{name: "null done", stage: StageImplement, edit: func(in, r doc, _ string) { r["done"] = nil }, want: "done is not an object", shape: true},
		{name: "complete with a field", stage: StageMint, edit: func(in, r doc, _ string) { asOutcome(r, "complete", doc{"x": 1}) }, want: `unknown field "x"`, shape: true},

		// §5.1 usage.
		{name: "usage with every field", stage: StageImplement},
		{name: "usage empty", stage: StageImplement, edit: func(in, r doc, _ string) { r["usage"] = doc{} }},
		{name: "usage negative cost", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.cost", -1) }, want: "usage.cost", shape: true},
		{name: "usage negative tokens", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.output_tokens", -1) }, want: "usage.output_tokens", shape: true},
		{name: "usage negative input tokens", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.input_tokens", -5) }, want: "usage.input_tokens", shape: true},
		{name: "usage fractional tokens", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.input_tokens", 1.5) }, want: "usage", shape: true},
		{name: "usage empty currency", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.currency", "") }, want: "usage.currency", shape: true},
		{name: "usage unknown field", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "usage.minutes", 3) }, want: `unknown field "minutes"`, shape: true},

		// §5.1 blocked, failed, needs_owner.
		{name: "blocked with dependency", stage: StageImplement, edit: func(in, r doc, _ string) {
			asOutcome(r, "blocked", doc{"reason_file": "reason.md", "dependency": "17"})
		}},
		{name: "blocked with null dependency", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"reason_file": "reason.md", "dependency": nil}) }},
		{name: "blocked without reason_file", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"dependency": "17"}) }, want: "blocked.reason_file is missing", shape: true},
		{name: "blocked with empty dependency", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "blocked", doc{"reason_file": "reason.md", "dependency": ""}) }, want: "blocked.dependency is empty", shape: true},
		{name: "failed reason of 500 characters", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{"reason": strings.Repeat("é", 500)}) }},
		{name: "failed reason of 501 characters", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{"reason": strings.Repeat("é", 501)}) }, want: "longer than 500", shape: true},
		{name: "failed reason empty", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{"reason": ""}) }, want: "failed.reason is empty", shape: true},
		{name: "failed reason missing", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{}) }, want: "failed.reason is empty", shape: true},
		{name: "failed reason two lines", stage: StageImplement, edit: func(in, r doc, _ string) { asOutcome(r, "failed", doc{"reason": "a\nb"}) }, want: "more than one line", shape: true},
		{name: "needs_owner valid", stage: StageAddress, edit: func(in, r doc, _ string) { asOutcome(r, "needs_owner", doc{"question_file": "question.md"}) }},
		{name: "needs_owner without question_file", stage: StageAddress, edit: func(in, r doc, _ string) { asOutcome(r, "needs_owner", doc{}) }, want: "needs_owner.question_file is missing", shape: true},

		// §5.1 text files in AGENT_LOOP_OUT.
		{name: "file in a subdirectory", stage: StageImplement, edit: func(in, r doc, out string) {
			writeFile(fileT, filepath.Join(out, "sub", "c.md"), []byte("x"))
			set(r, "done.change.body_file", "sub/c.md")
		}},
		{name: "file of exactly 64 KiB", stage: StageImplement, edit: func(in, r doc, out string) {
			writeFile(fileT, filepath.Join(out, "big.md"), []byte(strings.Repeat("a", MaxTextFile)))
			set(r, "done.change.body_file", "big.md")
		}},
		{name: "empty file", stage: StageImplement, edit: func(in, r doc, out string) {
			writeFile(fileT, filepath.Join(out, "empty.md"), nil)
			set(r, "done.change.body_file", "empty.md")
		}},
		{name: "file over 64 KiB", stage: StageImplement, edit: func(in, r doc, out string) {
			writeFile(fileT, filepath.Join(out, "big.md"), []byte(strings.Repeat("a", MaxTextFile+1)))
			set(r, "done.change.body_file", "big.md")
		}, want: "larger than 64 KiB"},
		{name: "file not UTF-8", stage: StageImplement, edit: func(in, r doc, out string) {
			writeFile(fileT, filepath.Join(out, "bad.md"), []byte{0xff, 0xfe})
			set(r, "done.change.body_file", "bad.md")
		}, want: "not UTF-8"},
		{name: "file missing", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.change.body_file", "nope.md") }, want: "can't be read"},
		{name: "file is a directory", stage: StageImplement, edit: func(in, r doc, out string) {
			writeFile(fileT, filepath.Join(out, "dir", "x.md"), []byte("x"))
			set(r, "done.change.body_file", "dir")
		}, want: "not a regular file"},
		{name: "absolute file path", stage: StageImplement, edit: func(in, r doc, out string) { set(r, "done.change.body_file", filepath.Join(out, "change.md")) }, want: "not relative", shape: true},
		{name: "file path with ..", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.change.body_file", "sub/../../change.md") }, want: "outside AGENT_LOOP_OUT", shape: true},
		{name: "symlink inside out dir", stage: StageImplement, edit: func(in, r doc, out string) {
			if err := os.Symlink("change.md", filepath.Join(out, "link.md")); err != nil {
				fileT.Fatal(err)
			}
			set(r, "done.change.body_file", "link.md")
		}},
		{name: "symlink to outside out dir", stage: StageImplement, edit: func(in, r doc, out string) {
			outside := filepath.Join(fileT.TempDir(), "secret.md")
			writeFile(fileT, outside, []byte("x"))
			if err := os.Symlink(outside, filepath.Join(out, "link.md")); err != nil {
				fileT.Fatal(err)
			}
			set(r, "done.change.body_file", "link.md")
		}, want: "outside AGENT_LOOP_OUT"},

		// §5.2 implement.
		{name: "implement SHA-256 head", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.head", strings.Repeat("ab", 32)) }},
		{name: "implement short head", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.head", "9f2c") }, want: "not a full commit SHA", shape: true},
		{name: "implement uppercase head", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.head", strings.ToUpper(sha1)) }, want: "not a full commit SHA", shape: true},
		{name: "implement without change", stage: StageImplement, edit: func(in, r doc, _ string) { del(r, "done.change") }, want: "done.change is missing"}, // the schema reads {head} as an address done
		{name: "implement empty title", stage: StageImplement, edit: func(in, r doc, _ string) { set(r, "done.change.title", " ") }, want: "done.change.title is empty"},
		{name: "implement without body_file", stage: StageImplement, edit: func(in, r doc, _ string) { del(r, "done.change.body_file") }, want: "done.change.body_file is missing", shape: true},
		{name: "implement given a review done", stage: StageImplement, edit: func(in, r doc, _ string) { r["done"] = resultFor(fileT, StageReview)["done"] }, want: "unknown field"},

		// §5.2 review: commit and kind equal the input.
		{name: "review commit differs", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.commit", sha1) }, want: "done.commit"},
		{name: "review kind differs", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.kind", "owner_push") }, want: "done.kind"},
		{name: "review base_update kind matches", stage: StageReview, edit: func(in, r doc, _ string) {
			set(in, "review.kind", "base_update")
			set(r, "done.kind", "base_update")
		}},
		{name: "review unknown kind", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.kind", "other") }, want: "done.kind", shape: true},
		{name: "review without summary_file", stage: StageReview, edit: func(in, r doc, _ string) { del(r, "done.summary_file") }, want: "done.summary_file is missing", shape: true},
		{name: "review without review input", stage: StageReview, edit: func(in, r doc, _ string) { delete(in, "review") }, want: "input has no review object"},

		// §5.2 review: exactly one ruling per finding to rule (FR-4 AC4, AC8).
		{name: "review nothing to rule", stage: StageReview, edit: func(in, r doc, _ string) {
			set(in, "review.rule_findings", []any{})
			set(in, "review.rule_manual", []any{})
			del(r, "done.rulings")
			set(r, "done.manual_rulings", nil)
		}},
		{name: "review ruling missing", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings", []any{step(step(r, "done"), "rulings").([]any)[0]}) }, want: "no ruling for F5"},
		{name: "review rulings omitted", stage: StageReview, edit: func(in, r doc, _ string) { del(r, "done.rulings") }, want: "no ruling for F3, F5"},
		{name: "review ruling for a finding not to rule", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.0.finding", "F9") }, want: `"F9", which is not in the input`},
		{name: "review two rulings for one finding", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.1.finding", "F3") }, want: `"F3" twice`},
		{name: "review ruling status withdrawn", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.0.status", "withdrawn") }},
		{name: "review ruling status open", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.0.status", "open") }},
		{name: "review ruling bad status", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.0.status", "fixed") }, want: "not a finding status", shape: true},
		{name: "review ruling bad note_file", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.rulings.1.note_file", "nope.md") }, want: "done.rulings[1].note_file"},
		{name: "review manual ruling missing", stage: StageReview, edit: func(in, r doc, _ string) { del(r, "done.manual_rulings") }, want: "no ruling for MC-981"},
		{name: "review manual ruling addressed", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.manual_rulings.0.ruling", "addressed") }},
		{name: "review manual ruling bad value", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.manual_rulings.0.ruling", "withdrawn") }, want: "not addressed or open", shape: true},
		{name: "review manual ruling for a comment not to rule", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.manual_rulings.0.comment", "MC-1") }, want: `"MC-1", which is not in the input`},
		{name: "review manual ruling bad note_file", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.manual_rulings.0.note_file", "../x.md") }, want: "outside AGENT_LOOP_OUT", shape: true},

		// §5.2 review: new findings.
		{name: "review no new findings", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings", []any{}) }},
		{name: "review score 1", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.score", 1) }},
		{name: "review score 5", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.score", 5) }},
		{name: "review score 0", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.score", 0) }, want: "score 0 is not from 1 to 5", shape: true},
		{name: "review score 6", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.score", 6) }, want: "score 6 is not from 1 to 5", shape: true},
		{name: "review score not an integer", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.score", 4.5) }, want: "score", shape: true},
		{name: "review score missing", stage: StageReview, edit: func(in, r doc, _ string) { del(r, "done.new_findings.0.score") }, want: "score 0", shape: true},
		{name: "review path without line", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.line", nil) }},
		{name: "review path and line omitted", stage: StageReview, edit: func(in, r doc, _ string) {
			del(r, "done.new_findings.0.line")
			del(r, "done.new_findings.0.path")
		}},
		{name: "review line without path", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.path", nil) }, want: "line needs a path", shape: true},
		{name: "review line 0", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.line", 0) }, want: "line 0 is less than 1", shape: true},
		{name: "review absolute path", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.path", "/etc/passwd") }, want: "not a repository-relative path", shape: true},
		{name: "review path with ..", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.path", "api/../../x.go") }, want: "not a repository-relative path", shape: true},
		{name: "review path with .", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.path", "./api/login.go") }, want: "not a repository-relative path", shape: true},
		{name: "review empty path", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.0.path", "") }, want: "not a repository-relative path", shape: true},
		{name: "review finding empty title", stage: StageReview, edit: func(in, r doc, _ string) { set(r, "done.new_findings.1.title", "") }, want: "title is empty", shape: true},
		{name: "review finding without body_file", stage: StageReview, edit: func(in, r doc, _ string) { del(r, "done.new_findings.1.body_file") }, want: "new_findings[1].body_file is missing", shape: true},

		// §5.2 address: replies.
		{name: "address head not a SHA", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.head", "HEAD") }, want: "not a full commit SHA", shape: true},
		{name: "address reply bad kind", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.0.kind", "done") }, want: "not a reply kind", shape: true},
		{name: "address reply not_valid", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.0.kind", "not_valid") }},
		{name: "address reply needs_info", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.0.kind", "needs_info") }},
		// FR-5 AC5: a deferred reply without deferred_to doesn't count as a reply; FR-5 AC2 decides (D23).
		{name: "address deferred without deferred_to, head moved", stage: StageAddress, edit: func(in, r doc, _ string) { del(r, "done.replies.1.deferred_to") }},
		{name: "address deferred with null deferred_to, head moved", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.1.deferred_to", nil) }},
		{name: "address deferred with blank deferred_to, head moved", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.1.deferred_to", "") }},
		{name: "address deferred without deferred_to, head unchanged", stage: StageAddress, edit: func(in, r doc, _ string) {
			del(r, "done.replies.1.deferred_to")
			set(r, "done.head", sha0)
		}, want: "F5 have no reply (FR-5 AC2)"},
		{name: "address deferred with blank deferred_to, head unchanged", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(r, "done.replies.1.deferred_to", " ")
			set(r, "done.head", sha0)
		}, want: "F5 have no reply (FR-5 AC2)"},
		{name: "address deferred_to on a fixed reply", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.0.deferred_to", "61") }, want: "deferred_to is set on a fixed reply", shape: true},
		{name: "address null deferred_to on a fixed reply", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.0.deferred_to", nil) }},
		{name: "address reply without body_file", stage: StageAddress, edit: func(in, r doc, _ string) { del(r, "done.replies.0.body_file") }, want: "replies[0].body_file is missing", shape: true},
		{name: "address reply to a finding not to answer", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.0.finding", "F9") }, want: `"F9", which is not in the input`},
		{name: "address two replies to one finding", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies.1", step(step(step(r, "done"), "replies"), "0")) }, want: `"F3" twice`},
		{name: "address manual reply deferred", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(r, "done.manual_replies.0.kind", "deferred")
			set(r, "done.manual_replies.0.deferred_to", "61")
		}},
		{name: "address manual reply deferred without deferred_to", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.manual_replies.0.kind", "deferred") }, want: "manual comments MC-981 have no reply"},
		{name: "address manual reply to a comment not to answer", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.manual_replies.0.comment", "MC-1") }, want: `"MC-1", which is not in the input`},
		{name: "address without address input", stage: StageAddress, edit: func(in, r doc, _ string) { delete(in, "address") }, want: "input has no address object"},

		// §5.2 address: FR-5 AC2.
		{name: "address head moved and a finding has no reply", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.replies", []any{step(step(r, "done"), "replies").([]any)[0]}) }},
		{name: "address head moved and no replies at all", stage: StageAddress, edit: func(in, r doc, _ string) {
			del(r, "done.replies")
			set(in, "address.answer_manual", []any{})
			del(r, "done.manual_replies")
		}},
		{name: "address head unchanged and every finding replied", stage: StageAddress, edit: func(in, r doc, _ string) { set(r, "done.head", sha0) }},
		{name: "address head unchanged and a finding has no reply", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(r, "done.head", sha0)
			set(r, "done.replies", []any{step(step(r, "done"), "replies").([]any)[0]})
		}, want: "F5 have no reply (FR-5 AC2)"},
		{name: "address manual comment has no reply", stage: StageAddress, edit: func(in, r doc, _ string) { del(r, "done.manual_replies") }, want: "manual comments MC-981 have no reply"},

		// §5.2 address: base_update mode (FR-5 AC6).
		{name: "address base_update merge", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(in, "address", doc{"mode": "base_update", "answer_findings": []any{}, "answer_manual": []any{}})
			del(r, "done.replies")
			del(r, "done.manual_replies")
		}},
		{name: "address base_update with replies", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(in, "address", doc{"mode": "base_update", "answer_findings": []any{}, "answer_manual": []any{}})
			del(r, "done.manual_replies")
		}, want: "must not reply"},
		{name: "address base_update with manual replies", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(in, "address", doc{"mode": "base_update", "answer_findings": []any{}, "answer_manual": []any{}})
			del(r, "done.replies")
		}, want: "must not reply"},
		{name: "address base_update without a new head", stage: StageAddress, edit: func(in, r doc, _ string) {
			set(in, "address", doc{"mode": "base_update", "answer_findings": []any{}, "answer_manual": []any{}})
			del(r, "done.replies")
			del(r, "done.manual_replies")
			set(r, "done.head", sha0)
		}, want: "must push a merge commit"},

		// §5.2 mint.
		{name: "mint plan_key of 64 characters", stage: StageMint, edit: func(in, r doc, _ string) { set(r, "done.plan_key", strings.Repeat("a", 64)) }},
		{name: "mint plan_key of 65 characters", stage: StageMint, edit: func(in, r doc, _ string) { set(r, "done.plan_key", strings.Repeat("a", 65)) }, want: "done.plan_key", shape: true},
		{name: "mint plan_key uppercase", stage: StageMint, edit: func(in, r doc, _ string) { set(r, "done.plan_key", "003-Auth") }, want: "done.plan_key", shape: true},
		{name: "mint plan_key empty", stage: StageMint, edit: func(in, r doc, _ string) { set(r, "done.plan_key", "") }, want: "done.plan_key", shape: true},
		{name: "mint head not a SHA", stage: StageMint, edit: func(in, r doc, _ string) { set(r, "done.head", "") }, want: "not a full commit SHA", shape: true},
		{name: "mint without proposal", stage: StageMint, edit: func(in, r doc, _ string) { del(r, "done.proposal") }, want: "done.proposal is missing", shape: true},
		{name: "mint empty proposal title", stage: StageMint, edit: func(in, r doc, _ string) { set(r, "done.proposal.title", "") }, want: "done.proposal.title is empty", shape: true},
		{name: "mint proposal without body_file", stage: StageMint, edit: func(in, r doc, _ string) { del(r, "done.proposal.body_file") }, want: "done.proposal.body_file is missing", shape: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fileT = t
			out := outDir(t)
			ind := inputFor(t, c.stage)
			rd := resultFor(t, c.stage)
			if c.edit != nil {
				c.edit(ind, rd, out)
			}
			inb, _ := json.Marshal(ind)
			in, err := ParseInput(inb)
			if err != nil {
				t.Fatalf("ParseInput: %v", err)
			}
			data := []byte(c.raw)
			if c.raw == "" {
				data, _ = json.Marshal(rd)
			}
			r, err := ValidateResult(in, data, out)
			schemaErr := schemaCheck(t, ResultSchema, data)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				if r.Result != Outcome(rd["result"].(string)) {
					t.Errorf("result %q, want %q", r.Result, rd["result"])
				}
				if schemaErr != nil {
					t.Errorf("ValidateResult accepted a result the schema rejects: %v", schemaErr)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got a valid result", c.want)
			}
			if !errors.Is(err, ErrInvalidResult) {
				t.Errorf("error %v does not wrap ErrInvalidResult", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err, c.want)
			}
			checkFailedResult(t, in, r)
			if c.shape && schemaErr == nil {
				t.Errorf("result.schema.json accepted a result with a shape error")
			}
		})
	}
}

// checkFailedResult checks the failed result that replaces an invalid one.
func checkFailedResult(t *testing.T, in Input, r Result) {
	t.Helper()
	if r.Result != OutcomeFailed || r.Failed == nil || r.Done != nil {
		t.Fatalf("invalid result became %+v, want failed", r)
	}
	if r.RunID != in.RunID || r.ContractVersion != Version {
		t.Errorf("failed result run_id %q version %q", r.RunID, r.ContractVersion)
	}
	if err := checkReason(r.Failed.Reason); err != nil {
		t.Errorf("failed reason %q: %v", r.Failed.Reason, err)
	}
	if !strings.HasPrefix(r.Failed.Reason, "invalid result: ") {
		t.Errorf("failed reason %q does not say the result was invalid", r.Failed.Reason)
	}
}

func TestFR18AC3_FailedReasonIsShortenedToOneLine(t *testing.T) {
	in := Input{RunID: "r"}
	r := failedResult(in, errors.New("line one\nline two "+strings.Repeat("x", 600)))
	if utf8.RuneCountInString(r.Failed.Reason) != MaxFailedReason {
		t.Errorf("reason has %d characters, want %d", utf8.RuneCountInString(r.Failed.Reason), MaxFailedReason)
	}
	if err := checkReason(r.Failed.Reason); err != nil {
		t.Error(err)
	}
}

// TestContractVersion_NewerMinorResultIsUnsupported proves §2: a result for
// contract 1.1 given to a 1.0 core is reported as unsupported, even when it
// carries fields 1.0 doesn't know.
func TestContractVersion_NewerMinorResultIsUnsupported(t *testing.T) {
	in, err := ParseInput(readExample(t, "input-implement.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, StageImplement)
	r["contract_version"] = "1.1"
	r["field_from_1_1"] = true
	data, _ := json.Marshal(r)
	got, err := ValidateResult(in, data, outDir(t))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("error %v, want ErrUnsupportedVersion", err)
	}
	checkFailedResult(t, in, got)
	if !strings.Contains(got.Failed.Reason, "unsupported contract version: 1.1") {
		t.Errorf("reason %q does not name the version", got.Failed.Reason)
	}
}

func TestFR5AC5_MissingRepliesWithClosedDeferredItem(t *testing.T) {
	in, err := ParseInput(readExample(t, "input-address.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseResult(StageAddress, readExample(t, "result-address-done.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, m := MissingReplies(in, r.Done.Address, func(string) bool { return true })
	if len(f) != 0 || len(m) != 0 {
		t.Errorf("all open: missing %v %v, want none", f, m)
	}
	f, m = MissingReplies(in, r.Done.Address, func(item string) bool { return item != "61" })
	if len(f) != 1 || f[0] != "F5" || len(m) != 0 {
		t.Errorf("item 61 closed: missing %v %v, want [F5] []", f, m)
	}
	r.Done.Address.ManualReplies[0].Kind = ReplyDeferred
	to := "61"
	r.Done.Address.ManualReplies[0].DeferredTo = &to
	_, m = MissingReplies(in, r.Done.Address, func(item string) bool { return item != "61" })
	if len(m) != 1 || m[0] != "MC-981" {
		t.Errorf("manual deferred to closed item: missing %v, want [MC-981]", m)
	}
	if f, m := MissingReplies(Input{}, r.Done.Address, nil); f != nil || m != nil {
		t.Errorf("no address input: %v %v", f, m)
	}
}

func TestFR18AC1_AllowedResults(t *testing.T) {
	want := map[Stage]map[Outcome]bool{
		StageImplement: {OutcomeDone: true, OutcomeBlocked: true, OutcomeFailed: true, OutcomeNeedsOwner: true},
		StageReview:    {OutcomeDone: true, OutcomeFailed: true, OutcomeNeedsOwner: true},
		StageAddress:   {OutcomeDone: true, OutcomeBlocked: true, OutcomeFailed: true, OutcomeNeedsOwner: true},
		StageMint:      {OutcomeDone: true, OutcomeComplete: true, OutcomeFailed: true, OutcomeNeedsOwner: true},
		"materialise":  {},
	}
	all := []Outcome{OutcomeDone, OutcomeComplete, OutcomeBlocked, OutcomeFailed, OutcomeNeedsOwner}
	for s, ok := range want {
		for _, o := range all {
			if Allowed(s, o) != ok[o] {
				t.Errorf("Allowed(%s, %s) = %v, want %v", s, o, !ok[o], ok[o])
			}
		}
	}
}

func TestResultMarshalRoundTrips(t *testing.T) {
	for _, s := range []Stage{StageImplement, StageReview, StageAddress, StageMint} {
		data := readExample(t, "result-"+string(s)+"-done.json")
		r, err := ParseResult(s, data)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		r2, err := ParseResult(s, b)
		if err != nil {
			t.Fatalf("%s: re-parse: %v", s, err)
		}
		b2, _ := json.Marshal(r2)
		if string(b) != string(b2) {
			t.Errorf("%s: round trip changed\n%s\n%s", s, b, b2)
		}
		if err := schemaCheck(t, ResultSchema, b); err != nil {
			t.Errorf("%s: marshalled result fails the schema: %v", s, err)
		}
	}
	if _, err := json.Marshal(Done{}); err == nil {
		t.Error("an empty Done marshalled")
	}
	if _, err := json.Marshal(Done{Implement: &ImplementDone{}, Mint: &MintDone{}}); err == nil {
		t.Error("a Done with two stage objects marshalled")
	}
}

func TestFR18AC3_MissingOutDirAndBadObjects(t *testing.T) {
	in, err := ParseInput(readExample(t, "input-implement.json"))
	if err != nil {
		t.Fatal(err)
	}
	data := readExample(t, "result-implement-done.json")
	if _, err := ValidateResult(in, data, filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "AGENT_LOOP_OUT") {
		t.Errorf("missing out dir: %v", err)
	}
	for _, raw := range []string{
		`{"contract_version":"1.0","run_id":"item/eagain/42#1:implement:1:a","result":"failed","failed":"x"}`,
		`{"contract_version":"1.0","run_id":"item/eagain/42#1:implement:1:a","result":"failed","failed":{"reason":"x"},"usage":[]}`,
		`{"contract_version":"1.0","run_id":"item/eagain/42#1:implement:1:a","result":"failed","failed":{"reason":"x"},"usage":{"cost":"1"}}`,
		`{"contract_version":1.0,"run_id":"r","result":"failed","failed":{"reason":"x"}}`,
	} {
		r, err := ValidateResult(in, []byte(raw), outDir(t))
		if err == nil {
			t.Errorf("accepted %s", raw)
			continue
		}
		checkFailedResult(t, in, r)
	}
}
