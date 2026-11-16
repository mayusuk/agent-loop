package contract

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func intp(i int) *int       { return &i }
func strp(s string) *string { return &s }

// sampleRecords has one full record of each type (work plan D15).
func sampleRecords() []Record {
	return []Record{
		{Type: RecordChange, Key: "item/eagain/42#1:change", Item: "42", N: 1},
		{Type: RecordReview, V: 1, Key: "item/eagain/42#1:review:r2:1", Item: "42", Change: "57", Commit: sha0,
			Kind: string(KindRound), Round: intp(2), Stale: true,
			Findings: []ReviewFindingState{
				{ID: "F3", Score: 4, Status: StatusAddressed},
				{ID: "F7", Score: 4, Status: StatusOpen, Comment: "1234"},
			},
			ManualRulings: []RecordManualRuling{{Comment: "MC-981", Ruling: RulingOpen}}},
		{Type: RecordFinding, Key: "item/eagain/42#1:review:r2:1:F7", Change: "57", Finding: "F7", Score: 4},
		{Type: RecordReply, Key: "item/eagain/42#1:address:1:F5", Change: "57", Finding: "F5", Kind: string(ReplyDeferred), DeferredTo: strp("61"), Head: sha1},
		{Type: RecordManualReply, Key: "item/eagain/42#1:address:1:MC-981", Change: "57", Comment: "MC-981", Kind: string(ReplyExplained)},
		{Type: RecordStatus, Key: "item/eagain/42#1:escalate:1", Item: "42", Event: EventEscalate, Reason: ReasonRoundLimit, Claim: 1,
			FindingIDs: []string{"F3"}, CommentIDs: []string{"MC-981"}, Commits: []string{sha1}},
		{Type: RecordStatus, Key: "item/eagain/42#1:resume:1", Item: "42", Event: EventResume, Claim: 1, RoundsAdded: intp(2)},
		{Type: RecordApprovedCommits, Key: "item/eagain/42#1:approved:1", Item: "42", Claim: 1, Commits: []string{sha1}},
		{Type: RecordFollowup, Key: "followup:57", Item: "42", Change: "57"},
		{Type: RecordTask, Key: "task:003-auth:login-api", Plan: "003-auth", Task: "login-api"},
		{Type: RecordPlan, Key: "plan:003-auth", Plan: "003-auth", Tasks: []PlanTaskLink{{Task: "login-api", Item: "43"}}},
		{Type: RecordProposal, Key: "proposal:003-auth", Plan: "003-auth"},
	}
}

// TestMarker_RoundTrip proves §6.1: every record type encodes to one line
// and decodes to the same record, and its JSON matches records.schema.json.
func TestMarker_RoundTrip(t *testing.T) {
	seen := map[RecordType]bool{}
	for _, rec := range sampleRecords() {
		seen[rec.Type] = true
		m, err := EncodeMarker(rec)
		if err != nil {
			t.Fatalf("%s: %v", rec.Type, err)
		}
		if !strings.HasPrefix(m, "<!-- agent-loop:v1 {") || !strings.HasSuffix(m, "} -->") || strings.ContainsAny(m, "\r\n") {
			t.Errorf("%s: marker %q is not one line of <!-- agent-loop:v1 {json} -->", rec.Type, m)
		}
		got, err := DecodeMarker(m)
		if err != nil {
			t.Fatalf("%s: decode %q: %v", rec.Type, m, err)
		}
		if !reflect.DeepEqual(got, rec) {
			t.Errorf("%s: round trip\n got %+v\nwant %+v", rec.Type, got, rec)
		}
		js := m[len("<!-- agent-loop:v1 ") : len(m)-len(" -->")]
		if err := schemaCheck(t, RecordsSchema, []byte(js)); err != nil {
			t.Errorf("%s: %s fails records.schema.json: %v", rec.Type, js, err)
		}
	}
	if len(seen) != len(recordTypes) {
		t.Errorf("samples cover %d record types, want %d", len(seen), len(recordTypes))
	}
}

func TestMarker_SpecExamplesDecode(t *testing.T) {
	for _, n := range []string{"record-marker-review.json", "record-review.json", "record-reply.json", "record-manual-reply.json", "record-followup.json"} {
		var compact strings.Builder
		var v any
		if err := json.Unmarshal(readExample(t, n), &v); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		compact.Write(b)
		rec, err := DecodeMarker("<!-- agent-loop:v1 " + compact.String() + " -->")
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if rec.Key == "" {
			t.Errorf("%s: no key", n)
		}
	}
	rec, err := DecodeMarker("<!-- agent-loop:v1 " + string(mustCompact(t, "record-review.json")) + " -->")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Findings) != 3 || rec.Findings[2].Comment == "" || rec.Round == nil || *rec.Round != 2 || len(rec.ManualRulings) != 1 {
		t.Errorf("review record decoded as %+v", rec)
	}
}

func mustCompact(t *testing.T, name string) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(readExample(t, name), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return b
}

// TestMarker_TextCantCloseTheComment checks that text in a record can't end
// the HTML comment early or start a new marker (D14).
func TestMarker_TextCantCloseTheComment(t *testing.T) {
	rec := Record{Type: RecordStatus, Key: "k --> <!-- agent-loop:v1 {} -->\n&", Event: EventEscalate}
	m, err := EncodeMarker(rec)
	if err != nil {
		t.Fatal(err)
	}
	inner := m[len("<!-- agent-loop:v1 ") : len(m)-len(" -->")]
	if strings.Contains(inner, "-->") || strings.Contains(inner, "<") || strings.ContainsAny(m, "\n") {
		t.Errorf("marker %q lets text escape the comment", m)
	}
	got, err := DecodeMarker(m)
	if err != nil || got.Key != rec.Key {
		t.Errorf("decode: %v, key %q", err, got.Key)
	}
}

func TestMarker_MalformedRejected(t *testing.T) {
	bad := map[string]string{
		"empty":              "",
		"no comment":         `agent-loop:v1 {"type":"review","key":"k"}`,
		"other version":      `<!-- agent-loop:v2 {"type":"review","key":"k"} -->`,
		"no space":           `<!--agent-loop:v1 {"type":"review","key":"k"}-->`,
		"two spaces":         `<!-- agent-loop:v1  {"type":"review","key":"k"} -->`,
		"not closed":         `<!-- agent-loop:v1 {"type":"review","key":"k"}`,
		"bad JSON":           `<!-- agent-loop:v1 {"type":"review","key":} -->`,
		"JSON array":         `<!-- agent-loop:v1 ["review"] -->`,
		"JSON string":        `<!-- agent-loop:v1 "review" -->`,
		"no type":            `<!-- agent-loop:v1 {"key":"k"} -->`,
		"unknown type":       `<!-- agent-loop:v1 {"type":"vote","key":"k"} -->`,
		"no key":             `<!-- agent-loop:v1 {"type":"review"} -->`,
		"empty key":          `<!-- agent-loop:v1 {"type":"review","key":""} -->`,
		"v 2":                `<!-- agent-loop:v1 {"type":"review","key":"k","v":2} -->`,
		"wrong field type":   `<!-- agent-loop:v1 {"type":"review","key":"k","round":"two"} -->`,
		"two lines":          "<!-- agent-loop:v1 {\"type\":\"review\",\n\"key\":\"k\"} -->",
		"trailing JSON":      `<!-- agent-loop:v1 {"type":"review","key":"k"} {} -->`,
		"only prefix suffix": `<!-- agent-loop:v1  -->`,
	}
	for name, s := range bad {
		if _, err := DecodeMarker(s); !errors.Is(err, ErrMalformedMarker) {
			t.Errorf("%s: DecodeMarker(%q) = %v, want ErrMalformedMarker", name, s, err)
		}
	}
	for _, rec := range []Record{{Type: "vote", Key: "k"}, {Type: RecordReview}, {Type: RecordReview, Key: "k", V: 3}} {
		if _, err := EncodeMarker(rec); !errors.Is(err, ErrMalformedMarker) {
			t.Errorf("EncodeMarker(%+v) = %v, want ErrMalformedMarker", rec, err)
		}
	}
	if _, err := AppendMarker("x", Record{}); err == nil {
		t.Error("AppendMarker accepted a record with no type")
	}
}

func TestMarker_UnknownFieldsIgnored(t *testing.T) {
	rec, err := DecodeMarker(`<!-- agent-loop:v1 {"type":"followup","key":"followup:57","change":"57","added_in_1_1":true} -->`)
	if err != nil || rec.Change != "57" {
		t.Errorf("decode: %v %+v", err, rec)
	}
}

// TestMarker_FindInBody checks that only a marker on the body's last line
// counts (§6.1, D14).
func TestMarker_FindInBody(t *testing.T) {
	rec := Record{Type: RecordFollowup, Key: "followup:57", Change: "57"}
	body, err := AppendMarker("| F5 | 3 | partially_addressed | link |\n\n", rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, "| F5") || !strings.Contains(body, "|\n\n<!-- agent-loop:v1 ") {
		t.Errorf("body %q", body)
	}
	got, err := FindMarker(body + "\n  \n")
	if err != nil || !reflect.DeepEqual(got, rec) {
		t.Errorf("FindMarker: %v %+v", err, got)
	}
	only, _ := AppendMarker("", rec)
	if got, err := FindMarker(only); err != nil || got.Key != rec.Key {
		t.Errorf("FindMarker on a marker-only body: %v", err)
	}
	if got, err := FindMarker("text\r\n" + only + "\r\n"); err != nil || got.Key != rec.Key {
		t.Errorf("FindMarker with CRLF: %v", err)
	}

	m, _ := EncodeMarker(rec)
	quoted := "Someone pasted:\n" + m + "\nand more text."
	if _, err := FindMarker(quoted); !errors.Is(err, ErrNoMarker) {
		t.Errorf("a marker that isn't on the last line counted: %v", err)
	}
	if _, err := FindMarker("plain text"); !errors.Is(err, ErrNoMarker) {
		t.Errorf("plain text: %v", err)
	}
	if _, err := FindMarker(""); !errors.Is(err, ErrNoMarker) {
		t.Errorf("empty body: %v", err)
	}
	if _, err := FindMarker("text\n<!-- agent-loop:v1 {broken -->"); !errors.Is(err, ErrMalformedMarker) {
		t.Errorf("broken last-line marker: %v, want ErrMalformedMarker", err)
	}
}

func TestRecordsSchemaRejects(t *testing.T) {
	bad := map[string]string{
		"no key":                `{"type":"followup","change":"57"}`,
		"unknown type":          `{"type":"vote","key":"k"}`,
		"review without commit": `{"type":"review","key":"k","item":"42","change":"57","kind":"round","findings":[]}`,
		"review bad score":      `{"type":"review","key":"k","item":"42","change":"57","commit":"c","kind":"round","findings":[{"id":"F1","score":9,"status":"open"}]}`,
		"status bad event":      `{"type":"status","key":"k","event":"poke"}`,
		"status bad reason":     `{"type":"status","key":"k","event":"escalate","reason":"Round Limit"}`,
		"reply bad kind":        `{"type":"reply","key":"k","finding":"F1","kind":"done"}`,
		"change without n":      `{"type":"change","key":"k","item":"42"}`,
		"v 2":                   `{"type":"followup","key":"k","change":"57","v":2}`,
		"plan bad key":          `{"type":"plan","key":"k","plan":"Plan A","tasks":[]}`,
	}
	for name, doc := range bad {
		if err := schemaCheck(t, RecordsSchema, []byte(doc)); err == nil {
			t.Errorf("%s: records schema accepted %s", name, doc)
		}
	}
}
