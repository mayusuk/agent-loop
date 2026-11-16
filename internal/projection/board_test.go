package projection

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

func board(t *testing.T, p *DB, now time.Time) []BoardRow {
	t.Helper()
	b, err := p.Board(context.Background(), "", now)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func record(t *testing.T, p *DB, d Decision) {
	t.Helper()
	if err := p.RecordDecision(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func itemDecision(project, item string, claim, seq int, at time.Time, row ItemRow) Decision {
	row.Project, row.Item = project, item
	return Decision{
		WorkflowID: "item/" + project + "/" + item, Claim: claim,
		Record: core.DecisionRecord{Seq: seq, At: at, Event: "head_moved", To: row.State},
		Item:   row,
	}
}

func TestFR11AC1_BoardShowsEachClaimedItem(t *testing.T) {
	p := openTest(t)
	// Item 42 enters in_review, then a second decision keeps it there with
	// new counts: the time in state runs from the first.
	record(t, p, itemDecision("p", "42", 1, 1, t0, ItemRow{State: core.StateInReview, Round: 1, RoundsAllowed: 3, Host: "mac", NextAction: "review"}))
	record(t, p, itemDecision("p", "42", 1, 2, t0.Add(10*time.Minute), ItemRow{State: core.StateInReview, Round: 2, RoundsAllowed: 3, Blocking: 2, Host: "mac", NextAction: "review"}))
	// Item 7 waits on the daily limit.
	record(t, p, itemDecision("p", "7", 1, 1, t0.Add(20*time.Minute), ItemRow{State: core.StateWaitingLimit, RoundsAllowed: 3, NextAction: "implement", WaitReason: "daily_limit"}))
	// Item 9 merged and item 10 closed: off the board.
	record(t, p, itemDecision("p", "9", 1, 1, t0, ItemRow{State: core.StateMerged}))
	record(t, p, itemDecision("p", "10", 1, 1, t0, ItemRow{State: core.StateClosed}))
	// Another project.
	record(t, p, itemDecision("q", "1", 1, 1, t0, ItemRow{State: core.StateEscalated, NextAction: "owner", WaitReason: "round_limit"}))

	now := t0.Add(time.Hour)
	got, err := p.Board(context.Background(), "p", now)
	if err != nil {
		t.Fatal(err)
	}
	want := []BoardRow{
		{ItemRow: ItemRow{Project: "p", Item: "42", State: core.StateInReview, Round: 2, RoundsAllowed: 3, Blocking: 2, Host: "mac", NextAction: "review", EnteredAt: t0}, TimeInState: time.Hour},
		{ItemRow: ItemRow{Project: "p", Item: "7", State: core.StateWaitingLimit, RoundsAllowed: 3, NextAction: "implement", WaitReason: "daily_limit", EnteredAt: t0.Add(20 * time.Minute)}, TimeInState: 40 * time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("board:\n got %+v\nwant %+v", got, want)
	}

	all := board(t, p, now)
	if len(all) != 3 || all[0].Project != "p" || all[2].Project != "q" {
		t.Fatalf("all projects: %+v", all)
	}
}

func TestFR11AC1_StateChangeResetsTimeInState(t *testing.T) {
	p := openTest(t)
	record(t, p, itemDecision("p", "42", 1, 1, t0, ItemRow{State: core.StateInReview}))
	record(t, p, itemDecision("p", "42", 1, 2, t0.Add(time.Minute), ItemRow{State: core.StateAddressing}))
	b := board(t, p, t0.Add(3*time.Minute))
	if b[0].State != core.StateAddressing || b[0].TimeInState != 2*time.Minute {
		t.Fatalf("got %s for %v, want addressing for 2m", b[0].State, b[0].TimeInState)
	}
	// A board time before the entry never gives a negative duration.
	if b := board(t, p, t0); b[0].TimeInState != 0 {
		t.Fatalf("time in state %v, want 0", b[0].TimeInState)
	}
	// A merge takes the item off the board.
	record(t, p, itemDecision("p", "42", 1, 3, t0.Add(4*time.Minute), ItemRow{State: core.StateMerged}))
	if b := board(t, p, t0.Add(5*time.Minute)); len(b) != 0 {
		t.Fatalf("merged item still on the board: %+v", b)
	}
}

func TestFR11AC2_ItemLogRecordsStartsResultsRetriesEscalationsAndMerge(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	run := func(id string, attempt, start, end int, result string) {
		t.Helper()
		if err := p.StartRun(ctx, StageRun{RunID: id, Project: "p", Item: "42", Stage: contract.StageImplement, Attempt: attempt, Host: "mac", Started: at(start)}); err != nil {
			t.Fatal(err)
		}
		if err := p.FinishRun(ctx, id, at(end), result, "/logs/"+id+".log"); err != nil {
			t.Fatal(err)
		}
	}
	dec := func(seq, m int, event string, to core.State, actions ...string) {
		t.Helper()
		d := itemDecision("p", "42", 1, seq, at(m), ItemRow{State: to})
		d.Record.Event, d.Record.Actions = event, actions
		if event == "stage_result" {
			d.Record.Result = &core.ResultRef{Stage: contract.StageImplement, RunID: "r" + event, Outcome: contract.OutcomeFailed}
		}
		record(t, p, d)
	}
	run("r1", 1, 0, 5, "failed")
	dec(1, 5, "stage_result", core.StateImplementing, "run_stage") // retry
	run("r2", 2, 5, 9, "timed_out")
	dec(2, 9, "stage_timed_out", core.StateEscalated, "post_status", "notify") // escalation
	dec(3, 20, "merge_result", core.StateMerged, "notify")                     // merge

	log, err := p.ItemLog(ctx, "p", "42")
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		at   time.Time
		what string
	}
	var got []line
	for _, e := range log {
		switch {
		case e.Decision != nil:
			got = append(got, line{e.At, "decision " + e.Decision.Record.Event + " -> " + string(e.Decision.Record.To)})
		case e.RunEnd:
			got = append(got, line{e.At, "end " + e.Run.RunID + " " + e.Run.Result})
		default:
			got = append(got, line{e.At, "start " + e.Run.RunID})
		}
	}
	want := []line{
		{at(0), "start r1"},
		{at(5), "start r2"},
		{at(5), "decision stage_result -> implementing"},
		{at(5), "end r1 failed"},
		{at(9), "decision stage_timed_out -> escalated"},
		{at(9), "end r2 timed_out"},
		{at(20), "decision merge_result -> merged"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log:\n got %v\nwant %v", got, want)
	}
	runs, _ := p.Runs(ctx, "p", "42")
	if runs[1].Attempt != 2 || runs[1].LogPath != "/logs/r2.log" || runs[1].Host != "mac" {
		t.Fatalf("run 2: %+v", runs[1])
	}
}

func TestStageRuns_RetryAndErrors(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	r := StageRun{RunID: "r1", Project: "p", Item: "42", Stage: contract.StageReview, Kind: "round", Attempt: 1, Started: t0, LogPath: "/a"}
	for range 2 {
		if err := p.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := p.Runs(ctx, "p", "42")
	if len(runs) != 1 || !runs[0].Ended.IsZero() || runs[0].Result != "" || runs[0].Kind != "round" {
		t.Fatalf("running run: %+v", runs)
	}
	// An empty log path keeps the one from the start.
	if err := p.FinishRun(ctx, "r1", t0.Add(time.Minute), "done", ""); err != nil {
		t.Fatal(err)
	}
	if runs, _ := p.Runs(ctx, "p", "42"); runs[0].LogPath != "/a" || runs[0].Result != "done" {
		t.Fatalf("finished run: %+v", runs[0])
	}
	if err := p.FinishRun(ctx, "nope", t0, "done", ""); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if err := p.FinishRun(ctx, "r1", time.Time{}, "done", ""); err == nil {
		t.Fatal("zero end: want an error")
	}
	if err := p.FinishRun(ctx, "r1", t0, "", ""); err == nil {
		t.Fatal("no result: want an error")
	}
	for name, bad := range map[string]StageRun{
		"no id":    {Project: "p", Item: "1", Stage: "review", Started: t0},
		"no stage": {RunID: "x", Project: "p", Item: "1", Started: t0},
		"no start": {RunID: "x", Project: "p", Item: "1", Stage: "review"},
	} {
		if err := p.StartRun(ctx, bad); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestDailyCount_RebuiltFromTheDaysAgentRuns(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	day := time.Date(2026, 11, 16, 12, 0, 0, 0, ny)
	runs := []struct {
		id      string
		project string
		stage   contract.Stage
		at      time.Time
	}{
		{"before", "p", contract.StageImplement, time.Date(2026, 11, 15, 23, 59, 59, 0, ny)},
		{"midnight", "p", contract.StageImplement, time.Date(2026, 11, 16, 0, 0, 0, 0, ny)},
		{"review", "p", contract.StageReview, time.Date(2026, 11, 16, 8, 0, 0, 0, ny)},
		{"address", "p", contract.StageAddress, time.Date(2026, 11, 16, 23, 59, 59, 0, ny)},
		{"merge", "p", core.StageMerge, time.Date(2026, 11, 16, 9, 0, 0, 0, ny)},
		{"other project", "q", contract.StageReview, time.Date(2026, 11, 16, 9, 0, 0, 0, ny)},
		{"next day", "p", contract.StageReview, time.Date(2026, 11, 17, 0, 0, 0, 0, ny)},
	}
	for _, r := range runs {
		if err := p.StartRun(ctx, StageRun{RunID: r.id, Project: r.project, Item: "1", Stage: r.stage, Started: r.at.UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := p.DailyCount(ctx, "p", day, ny); err != nil || n != 3 {
		t.Fatalf("count %d, %v; want 3", n, err)
	}
	// In UTC, 16 Nov holds "before" (04:59:59Z), "midnight" and "review",
	// and "address" (04:59:59Z on 17 Nov) moves to the next day.
	if n, _ := p.DailyCount(ctx, "p", time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC), time.UTC); n != 3 {
		t.Fatalf("UTC count %d, want 3", n)
	}
	if n, _ := p.DailyCount(ctx, "p", time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC), time.UTC); n != 2 {
		t.Fatalf("UTC 17 Nov count %d, want 2 (address, next day)", n)
	}
}

func TestDailyCount_DSTDayIs25Hours(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	// 2026-11-01 is the end of daylight saving time in New York: 25 hours.
	late := time.Date(2026, 11, 1, 23, 30, 0, 0, ny)
	if err := p.StartRun(ctx, StageRun{RunID: "late", Project: "p", Item: "1", Stage: contract.StageReview, Started: late}); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.DailyCount(ctx, "p", late, ny); n != 1 {
		t.Fatalf("count %d, want 1", n)
	}
	if n, _ := p.DailyCount(ctx, "p", late.AddDate(0, 0, 1), ny); n != 0 {
		t.Fatalf("next day count %d, want 0", n)
	}
	if n, _ := p.DailyCount(ctx, "p", late, nil); n > 1 {
		t.Fatalf("local count %d", n)
	}
}

func TestHosts_UpsertAndList(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	if err := p.UpsertHost(ctx, Host{Name: "mac", Capabilities: []string{"agent"}, LastSeen: t0, LastCheck: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertHost(ctx, Host{Name: "mac", Capabilities: []string{"agent", "docker"}, LastSeen: t0.Add(time.Minute), LastCheck: "docker missing"}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertHost(ctx, Host{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertHost(ctx, Host{}); err == nil {
		t.Fatal("no name: want an error")
	}
	got, err := p.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Host{
		{Name: "box"},
		{Name: "mac", Capabilities: []string{"agent", "docker"}, LastSeen: t0.Add(time.Minute), LastCheck: "docker missing"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts:\n got %+v\nwant %+v", got, want)
	}
}

func TestConcurrentWriters(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	errs := make(chan error, 20)
	for i := range 20 {
		go func() {
			d := itemDecision("p", "x", 1, i+1, t0.Add(time.Duration(i)*time.Second), ItemRow{State: core.StateInReview})
			errs <- p.RecordDecision(ctx, d)
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if ds, _ := p.Decisions(ctx, "p", "x"); len(ds) != 20 {
		t.Fatalf("got %d decisions, want 20", len(ds))
	}
}
