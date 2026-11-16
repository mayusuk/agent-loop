package projection

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 123456789, time.UTC)

func openTest(t *testing.T) *DB {
	t.Helper()
	p, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// decision is a valid stage_result decision on item 42, claim 1.
func decision(seq int, at time.Time, to core.State) Decision {
	return Decision{
		WorkflowID: "item/p/42",
		Claim:      1,
		Record: core.DecisionRecord{
			Seq: seq, At: at, Event: "stage_result", From: core.StateInReview, To: to,
			Basis: core.Basis{
				Head: "c0ffee", ReviewCommit: "c0ffee", ReviewKind: contract.KindRound,
				RoundsUsed: 1, RoundAllowance: 3, BlockingFindings: []string{"F1", "F2"},
				UnclearedManual: []string{"M1"},
			},
			Result:  &core.ResultRef{Stage: contract.StageReview, RunID: "item/p/42#1:review:3", Outcome: contract.OutcomeDone},
			Rule:    "review_blocking",
			Actions: []string{"run_stage", "record_decision"},
		},
		Item: ItemRow{Project: "p", Item: "42", State: to, Round: 1, RoundsAllowed: 3, Blocking: 2, Host: "mac", NextAction: "address"},
	}
}

func TestNFR5AC1_DecisionNamesTrackerStateAndStageResult(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	d := decision(1, t0, core.StateAddressing)
	if err := p.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	// A decision with no stage result keeps that explicit.
	d2 := decision(2, t0.Add(time.Minute), core.StateAddressing)
	d2.Record.Event, d2.Record.Result, d2.Record.Actions = "head_moved", nil, nil
	if err := p.RecordDecision(ctx, d2); err != nil {
		t.Fatal(err)
	}
	got, err := p.Decisions(ctx, "p", "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d decisions, want 2", len(got))
	}
	if !reflect.DeepEqual(got[0].Record, d.Record) {
		t.Errorf("decision 1 round trip:\n got %+v\nwant %+v", got[0].Record, d.Record)
	}
	if !reflect.DeepEqual(got[1].Record, d2.Record) {
		t.Errorf("decision 2 round trip:\n got %+v\nwant %+v", got[1].Record, d2.Record)
	}
	if got[0].WorkflowID != "item/p/42" || got[0].Claim != 1 {
		t.Errorf("decision 1 names %s#%d", got[0].WorkflowID, got[0].Claim)
	}
}

func TestNFR5AC1_StageResultDecisionWithoutResultIsRefused(t *testing.T) {
	p := openTest(t)
	cases := map[string]func(*Decision){
		"stage result missing": func(d *Decision) { d.Record.Result = nil },
		"result without run":   func(d *Decision) { d.Record.Result.RunID = "" },
		"result without stage": func(d *Decision) { d.Record.Result.Stage = "" },
		"result no outcome":    func(d *Decision) { d.Record.Result.Outcome = "" },
		"no workflow":          func(d *Decision) { d.WorkflowID = "" },
		"no item":              func(d *Decision) { d.Item.Item = "" },
		"no project":           func(d *Decision) { d.Item.Project = "" },
		"no event":             func(d *Decision) { d.Record.Event = "" },
		"no time":              func(d *Decision) { d.Record.At = time.Time{} },
		"no decision number":   func(d *Decision) { d.Record.Seq = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := decision(1, t0, core.StateAddressing)
			mutate(&d)
			if err := p.RecordDecision(context.Background(), d); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if got, _ := p.Decisions(context.Background(), "p", "42"); len(got) != 0 {
		t.Fatalf("refused decisions were written: %v", got)
	}
}

func TestRecordDecision_RetryIsNoOp(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	d := decision(1, t0, core.StateAddressing)
	for range 3 {
		if err := p.RecordDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := p.Decisions(ctx, "p", "42"); len(got) != 1 {
		t.Fatalf("got %d decisions, want 1", len(got))
	}
}

func TestRecordDecision_LateRetryDoesNotMoveBoardBack(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	if err := p.RecordDecision(ctx, decision(2, t0, core.StateMerging)); err != nil {
		t.Fatal(err)
	}
	// Seq 1 arrives late (for example a retried activity).
	if err := p.RecordDecision(ctx, decision(1, t0.Add(-time.Minute), core.StateAddressing)); err != nil {
		t.Fatal(err)
	}
	b := board(t, p, t0)
	if b[0].State != core.StateMerging {
		t.Fatalf("state %s, want merging", b[0].State)
	}
	// A new claim replaces the row even with a lower decision number.
	d := decision(1, t0.Add(time.Hour), core.StateImplementing)
	d.Claim = 2
	if err := p.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if b := board(t, p, t0.Add(time.Hour)); b[0].State != core.StateImplementing {
		t.Fatalf("state %s after a new claim, want implementing", b[0].State)
	}
}

func TestNewDecision_BuildsRowFromState(t *testing.T) {
	s := core.ItemState{
		State: core.StateAddressing, WorkflowID: "item/p/7", Item: "7", Claim: 3,
		RoundsUsed: 2, RoundsAdded: 1,
		Findings: []core.Finding{
			{ID: "F1", Score: 9, Status: contract.StatusOpen},
			{ID: "F2", Score: 1, Status: contract.StatusOpen},
		},
	}
	pol := core.Policy{MaxRounds: 3, BlockingThreshold: 5}
	rec := core.DecisionRecord{Seq: 4}
	got := NewDecision("p", s, pol, rec, View{Host: "mac", NextAction: "address", WaitReason: ""})
	want := Decision{
		WorkflowID: "item/p/7", Claim: 3, Record: rec,
		Item: ItemRow{Project: "p", Item: "7", State: core.StateAddressing, Round: 2, RoundsAllowed: 4, Blocking: 1, Host: "mac", NextAction: "address"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestOpen_CreatesPrivateFileAndReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	path := filepath.Join(dir, "projection.db")
	p, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RecordDecision(context.Background(), decision(1, t0, core.StateAddressing)); err != nil {
		t.Fatal(err)
	}
	p.Close()
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o700}, {path, 0o600}} {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != c.mode {
			t.Errorf("%s mode %v, want %v", c.path, fi.Mode().Perm(), c.mode)
		}
	}
	p, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got, _ := p.Decisions(context.Background(), "p", "42"); len(got) != 1 {
		t.Fatalf("reopened database has %d decisions, want 1", len(got))
	}
}

func TestOpen_RefusesNewerSchemaAndEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("empty path: want an error")
	}
	path := filepath.Join(t.TempDir(), "projection.db")
	p, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Exec(`UPDATE schema_version SET version = ?`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	p.Close()
	_, err = Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("got %v, want a newer-schema error", err)
	}
}
