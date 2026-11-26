package api

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/projection"
)

var hostsNow = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func hostsService(t *testing.T) (*Service, *projection.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := projection.Open(ctx, filepath.Join(shortDir(t), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, it := range []string{"42", "7"} {
		err := db.RecordDecision(ctx, projection.Decision{
			WorkflowID: "item/p/" + it, Claim: 1,
			Record: core.DecisionRecord{Seq: 1, At: hostsNow.Add(-time.Hour), Event: "started", To: core.StateInReview},
			Item:   projection.ItemRow{Project: "p", Item: it, State: core.StateInReview, RoundsAllowed: 3, NextAction: "run_stage"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{
		Workflows: &stubWorkflows{}, Projection: db, Projects: []string{"p"},
		StageCaps: map[string]map[contract.Stage][]string{"p": {contract.StageReview: {"x", "agent"}}},
		Now:       func() time.Time { return hostsNow },
	}
	return s, db
}

func upsertHost(t *testing.T, db *projection.DB, h projection.Host) {
	t.Helper()
	if err := db.UpsertHost(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

func boardRow(t *testing.T, s *Service, item string) *apiv1.BoardItem {
	t.Helper()
	b, err := s.Board(context.Background(), &apiv1.BoardRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range b.GetProjects()[0].GetItems() {
		if it.GetItem() == item {
			return it
		}
	}
	t.Fatalf("item %s isn't on the board", item)
	return nil
}

// TestFR11AC4_BoardListsHostsWithHealth: the board lists every host by
// name with its capabilities, its health and the time since its last
// heartbeat. A host that hasn't been seen for over a minute is unhealthy
// (work plan #162 D2, D5).
func TestFR11AC4_BoardListsHostsWithHealth(t *testing.T) {
	s, db := hostsService(t)
	upsertHost(t, db, projection.Host{Name: "mac", Capabilities: []string{"agent"}, LastSeen: hostsNow.Add(-10 * time.Second)})
	upsertHost(t, db, projection.Host{Name: "h2", Capabilities: []string{"agent", "x"}, LastSeen: hostsNow.Add(-61 * time.Second)})
	upsertHost(t, db, projection.Host{Name: "new"})
	b, err := s.Board(context.Background(), &apiv1.BoardRequest{Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name    string
		caps    []string
		healthy bool
		ago     int64
	}
	var got []row
	for _, h := range b.GetHosts() {
		got = append(got, row{h.GetName(), h.GetCapabilities(), h.GetHealthy(), h.GetLastSeenSecondsAgo()})
	}
	want := []row{
		{"h2", []string{"agent", "x"}, false, 61},
		{"mac", []string{"agent"}, true, 10},
		{"new", nil, false, -1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts:\n got %+v\nwant %+v", got, want)
	}
}

// TestFR14AC2_BoardNamesTheCapabilityAStageWaitsFor: a stage that no
// worker has started waits for a capable host while no healthy host has
// every capability it needs, and the board names the set. It stops
// waiting once such a host is healthy, and a stage that runs, or needs
// only capabilities a healthy host has, never shows the wait (work plan
// #162 D3, D4).
func TestFR14AC2_BoardNamesTheCapabilityAStageWaitsFor(t *testing.T) {
	s, db := hostsService(t)
	ctx := context.Background()
	upsertHost(t, db, projection.Host{Name: "mac", Capabilities: []string{"agent"}, LastSeen: hostsNow})
	start := func(id, item string, st contract.Stage) {
		t.Helper()
		if err := db.StartRun(ctx, projection.StageRun{RunID: id, Project: "p", Item: item, Stage: st, Attempt: 1, Started: hostsNow.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	start("r42", "42", contract.StageReview)
	start("r7", "7", contract.StageImplement) // needs agent, which mac has

	if got := boardRow(t, s, "42").GetWaitReason(); got != "capable_host agent,x" {
		t.Errorf("review needs agent,x and only mac (agent) is healthy: wait %q", got)
	}
	if got := boardRow(t, s, "42").GetState(); got != string(core.StateInReview) {
		t.Errorf("waiting stage's state %q, want it kept", got)
	}
	if got := boardRow(t, s, "7").GetWaitReason(); got != "" {
		t.Errorf("implement needs agent, mac is healthy: wait %q", got)
	}

	// h2 has x but is down: still waiting.
	upsertHost(t, db, projection.Host{Name: "h2", Capabilities: []string{"agent", "x"}, LastSeen: hostsNow.Add(-2 * time.Minute)})
	if got := boardRow(t, s, "42").GetWaitReason(); got != "capable_host agent,x" {
		t.Errorf("h2 is unhealthy: wait %q", got)
	}
	// h2 comes back: the stage is about to start.
	upsertHost(t, db, projection.Host{Name: "h2", Capabilities: []string{"agent", "x"}, LastSeen: hostsNow})
	if got := boardRow(t, s, "42").GetWaitReason(); got != "" {
		t.Errorf("h2 is healthy: wait %q", got)
	}
	// Once h2 starts it, the run has a host, so it no longer waits even if
	// h2's heartbeat is late.
	if err := db.SetRunHost(ctx, "r42", "h2"); err != nil {
		t.Fatal(err)
	}
	upsertHost(t, db, projection.Host{Name: "h2", Capabilities: []string{"agent", "x"}, LastSeen: hostsNow.Add(-2 * time.Minute)})
	if r := boardRow(t, s, "42"); r.GetWaitReason() != "" || r.GetHost() != "h2" {
		t.Errorf("running on h2: wait %q, host %q", r.GetWaitReason(), r.GetHost())
	}
}

// TestFR14AC2_CapableHostWaitText: the wait names the sorted capability
// set, agent when the stage names none (work plan #162 D4).
func TestFR14AC2_CapableHostWaitText(t *testing.T) {
	for _, c := range []struct {
		caps []string
		want string
	}{
		{nil, "capable_host agent"},
		{[]string{"x", "agent"}, "capable_host agent,x"},
		{[]string{"docker", "agent", "docker"}, "capable_host agent,docker"},
	} {
		if got := CapableHostWait(c.caps); got != c.want {
			t.Errorf("CapableHostWait(%v) = %q, want %q", c.caps, got, c.want)
		}
	}
}
