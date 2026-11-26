package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// xProfile is testProfile with a review stage that needs agent and x.
var xProfile = strings.Replace(testProfile, "[stages.review]\n", "[stages.review]\ncapabilities = [\"x\", \"agent\"]\n", 1)

func xStageCaps(t *testing.T) map[contract.Stage][]string {
	t.Helper()
	dep, err := profile.LoadDeployment(writeDeployment(t, map[string]string{"widgets": xProfile}))
	if err != nil {
		t.Fatal(err)
	}
	_, prof, err := onlyProject(dep)
	if err != nil {
		t.Fatal(err)
	}
	return stageCaps(prof)
}

// TestFR14AC1_ProfileStageCapabilitiesMapToWorkerQueues: a stage that needs
// x is on cap:agent+x, which only a worker with agent and x polls; a
// worker with agent alone polls the other stages' queue. A second worker
// joins by its capability list alone (TECHNICAL-SPEC §6.4).
func TestFR14AC1_ProfileStageCapabilitiesMapToWorkerQueues(t *testing.T) {
	caps := xStageCaps(t)
	if q := workflows.CapabilityQueue(caps[contract.StageReview]); q != "cap:agent+x" {
		t.Fatalf("review queue %s", q)
	}
	if q := workflows.CapabilityQueue(caps[contract.StageImplement]); q != "cap:agent" {
		t.Fatalf("implement queue %s", q)
	}
	for _, c := range []struct {
		caps string
		want []string
	}{
		{"agent", []string{"core", "cap:agent"}},
		{"agent,x", []string{"core", "cap:agent", "cap:agent+x"}},
		{"x", []string{"core"}},
		{"x,agent,docker", []string{"core", "cap:agent", "cap:agent+x"}},
	} {
		if got := workflows.WorkerQueues(parseCapabilities(c.caps), caps); !reflect.DeepEqual(got, c.want) {
			t.Errorf("worker --capabilities %s polls %v, want %v", c.caps, got, c.want)
		}
	}
}

// TestFR14AC1_WorkerName: --name names the host, trimmed; without it the
// OS host name does (plan D3). An empty name is refused (work plan #162
// D1).
func TestFR14AC1_WorkerName(t *testing.T) {
	osHost := func() (string, error) { return "mac-mini.local", nil }
	noHost := func() (string, error) { return "", errors.New("no host name") }
	for _, c := range []struct {
		flag   string
		given  bool
		osHost func() (string, error)
		want   string
		err    bool
	}{
		{" h2 ", true, noHost, "h2", false},
		{"", false, osHost, "mac-mini.local", false},
		{"", true, osHost, "", true},
		{"  ", true, osHost, "", true},
		{"", false, noHost, "", true},
		{"", false, func() (string, error) { return " ", nil }, "", true},
	} {
		got, err := workerName(c.flag, c.given, c.osHost)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("workerName(%q, %v) = %q, %v; want %q, error %v", c.flag, c.given, got, err, c.want, c.err)
		}
	}
	code, _, errs := cli(t, "worker", "--capabilities", "agent", "--name", " ")
	if code != 2 || !strings.Contains(errs, "--name is empty") || !strings.Contains(errs, "usage: agent-loop worker --capabilities <list> [--name <name>]") {
		t.Fatalf("empty --name: exit %d, %q", code, errs)
	}
}

// TestFR11AC4_WorkerHeartbeatKeepsTheLastCheck: each heartbeat records
// the worker's name, capabilities and time, and keeps the host's last
// check result (TECHNICAL-SPEC §10).
func TestFR11AC4_WorkerHeartbeatKeepsTheLastCheck(t *testing.T) {
	_, _, db := testServer(t, "p")
	ctx := context.Background()
	if err := db.UpsertHost(ctx, projection.Host{Name: "h2", Capabilities: []string{"agent"}, LastSeen: testNow.Add(-time.Hour), LastCheck: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := recordHost(ctx, db, "h2", []string{"agent", "x"}, testNow); err != nil {
		t.Fatal(err)
	}
	if err := recordHost(ctx, db, "mac", []string{"agent"}, testNow.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := db.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []projection.Host{
		{Name: "h2", Capabilities: []string{"agent", "x"}, LastSeen: testNow, LastCheck: "ok"},
		{Name: "mac", Capabilities: []string{"agent"}, LastSeen: testNow.Add(-time.Second)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts:\n got %+v\nwant %+v", got, want)
	}
	if heartbeatInterval >= projection.HostHealthWindow/2 {
		t.Fatalf("heartbeat every %v can't keep a host healthy within %v", heartbeatInterval, projection.HostHealthWindow)
	}
}

// TestFR14AC2_BoardShowsHostsAndTheCapableHostWait: the board lists a
// second worker, h2 with agent,x, beside mac, with health and last seen
// (FR-14 AC1, FR-11 AC4). While h2 is down, the review that needs x waits
// in its state and the board names the capability set; when h2 is back,
// the wait is gone (FR-14 AC2).
func TestFR14AC2_BoardShowsHostsAndTheCapableHostWait(t *testing.T) {
	cfg, _, db := testServerCaps(t, map[string]map[contract.Stage][]string{"widgets": xStageCaps(t)}, "widgets")
	ctx := context.Background()
	onBoard(t, db, "widgets", "42", testNow.Add(-time.Hour), projection.ItemRow{State: core.StateInReview, RoundsAllowed: 3, NextAction: "run_stage"})
	if err := db.StartRun(ctx, projection.StageRun{RunID: "r1", Project: "widgets", Item: "42", Stage: contract.StageReview, Attempt: 1, Started: testNow.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := recordHost(ctx, db, "mac", []string{"agent"}, testNow.Add(-5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := recordHost(ctx, db, "h2", []string{"agent", "x"}, testNow.Add(-90*time.Second)); err != nil {
		t.Fatal(err)
	}
	line := func(out, prefix string) string {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == prefix {
				return strings.Join(f, " ")
			}
		}
		t.Fatalf("no %s line:\n%s", prefix, out)
		return ""
	}
	code, out, errs := cli(t, "board", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "\nHosts\n") || !strings.Contains(out, "NAME") || !strings.Contains(out, "CAPABILITIES") ||
		!strings.Contains(out, "HEALTH") || !strings.Contains(out, "LAST SEEN") {
		t.Fatalf("no hosts section:\n%s", out)
	}
	if got := line(out, "h2"); got != "h2 agent,x unhealthy 1m30s ago" {
		t.Errorf("h2 line %q", got)
	}
	if got := line(out, "mac"); got != "mac agent healthy 5s ago" {
		t.Errorf("mac line %q", got)
	}
	if got := line(out, "42"); !strings.Contains(got, "in_review") || !strings.HasSuffix(got, "run_stage capable_host agent,x") {
		t.Errorf("waiting item line %q", got)
	}

	// h2 returns.
	if err := recordHost(ctx, db, "h2", []string{"agent", "x"}, testNow); err != nil {
		t.Fatal(err)
	}
	_, out, _ = cli(t, "board", "--config", cfg, "widgets")
	if got := line(out, "h2"); got != "h2 agent,x healthy 0s ago" {
		t.Errorf("h2 back: line %q", got)
	}
	if got := line(out, "42"); strings.Contains(got, "capable_host") {
		t.Errorf("h2 back: item still waits: %q", got)
	}
}

// TestFR11AC4_BoardWithNoHosts: with no worker recorded, the hosts
// section says so (work plan #162 D5).
func TestFR11AC4_BoardWithNoHosts(t *testing.T) {
	cfg, _, _ := testServer(t, "p")
	_, out, _ := cli(t, "board", "--config", cfg)
	if !strings.HasSuffix(out, "\nHosts\n  no workers have started\n") {
		t.Fatalf("board:\n%s", out)
	}
	if slices.Contains(strings.Split(out, "\n"), "NAME") {
		t.Fatalf("header with no hosts:\n%s", out)
	}
}

// TestFR11AC4_HeartbeatOnlyWhileTemporalAnswers: a heartbeat whose health
// check fails records nothing and says why; one whose check passes records
// the host (work plan #162 D2).
func TestFR11AC4_HeartbeatOnlyWhileTemporalAnswers(t *testing.T) {
	_, _, db := testServer(t, "p")
	ctx := context.Background()
	var errOut strings.Builder
	heartbeat(ctx, func(context.Context) error { return errors.New("unavailable") }, db, "mac", []string{"agent"}, testNow, &errOut)
	if got, err := db.Hosts(ctx); err != nil || len(got) != 0 {
		t.Fatalf("hosts after a failed check: %+v, %v; want none", got, err)
	}
	if !strings.Contains(errOut.String(), "Temporal isn't answering") {
		t.Fatalf("stderr %q doesn't say why", errOut.String())
	}
	heartbeat(ctx, func(context.Context) error { return nil }, db, "mac", []string{"agent"}, testNow, &errOut)
	got, err := db.Hosts(ctx)
	if err != nil || len(got) != 1 || !got[0].LastSeen.Equal(testNow) {
		t.Fatalf("hosts after a passing check: %+v, %v; want mac seen at %v", got, err, testNow)
	}
}

// TestFR14AC2_FatalWorkerErrorStopsTheHeartbeat: when a Temporal worker
// stops with a fatal error, the loop records no more heartbeats and exits
// 1, so the host turns unhealthy and the board shows the wait; a signal
// ends it with 0 (FR-11 AC4, work plan #162 D2).
func TestFR14AC2_FatalWorkerErrorStopsTheHeartbeat(t *testing.T) {
	onFatal, fatal := fatalSink()
	tick := make(chan time.Time, 1)
	beats := 0
	tick <- testNow
	var errOut strings.Builder
	beat := func() {
		beats++
		onFatal(errors.New("namespace not found"))
		onFatal(errors.New("a second worker")) // doesn't block
	}
	if code := heartbeatLoop(context.Background(), tick, fatal, beat, &errOut); code != 1 {
		t.Fatalf("exit %d after a fatal error, want 1", code)
	}
	if beats != 1 || !strings.Contains(errOut.String(), "namespace not found") {
		t.Fatalf("beats %d, stderr %q; want one beat, then the fatal error", beats, errOut.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := heartbeatLoop(ctx, make(chan time.Time), make(chan error), func() { t.Error("beat after the signal") }, &errOut); code != 0 {
		t.Fatalf("exit %d after a signal, want 0", code)
	}
}

// TestFR14AC1_WorkerNameInUseWithOtherCapabilities: a worker can't start
// under a name that a healthy host uses with other capabilities; the same
// capabilities, or a host that is unhealthy or has another name, are fine
// (plan D3, work plan #162 D7).
func TestFR14AC1_WorkerNameInUseWithOtherCapabilities(t *testing.T) {
	_, _, db := testServer(t, "p")
	ctx := context.Background()
	if err := recordHost(ctx, db, "mac", []string{"agent", "x"}, testNow.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := recordHost(ctx, db, "old", []string{"agent", "x"}, testNow.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	err := claimName(ctx, db, "mac", []string{"agent"}, testNow)
	if err == nil || !strings.Contains(err.Error(), "--name") || !strings.Contains(err.Error(), "agent,x") {
		t.Fatalf("claimName(mac, agent) = %v, want a refusal naming agent,x and --name", err)
	}
	for _, tc := range []struct {
		name string
		caps []string
	}{
		{"mac", []string{"agent", "x"}}, // same host, same queues
		{"old", []string{"agent"}},      // unhealthy: its worker has stopped
		{"h2", []string{"agent"}},       // a new host
	} {
		if err := claimName(ctx, db, tc.name, tc.caps, testNow); err != nil {
			t.Errorf("claimName(%s, %v) = %v, want nil", tc.name, tc.caps, err)
		}
	}
}
