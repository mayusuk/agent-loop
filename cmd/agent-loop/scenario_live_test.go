package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/api"
	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/secrets"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// TestScenario11_1_Live is PRODUCT-SPEC §11.1 on a GitHub test repository
// with a running agent-loop server and the project's own handlers (the
// starter handlers, plan D3). The test builds agent-loop and runs the
// worker itself, so step 4 can kill it; it acts as the owner and as a
// second human, and reads the repository with the loop's own adapters.
// Steps whose start depends on what the agents produce (a blocking
// finding in B's round 1, a third round for C) wait for it and are
// skipped, saying so, when the agents don't produce it in time (work plan
// #25 D9). Skipped unless AGENT_LOOP_LIVE=1. docs/LIVE-SCENARIO.md has
// the setup and the variables.
func TestScenario11_1_Live(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("the live scenario runs only with AGENT_LOOP_LIVE=1")
	}
	lv := newLive(t)
	a, b := lv.newItem("A"), lv.newItem("B")
	c, d := lv.newItem("C"), lv.newItem("D")

	// The steps run in the order the items reach them.
	t.Run("Step4_WorkerKilledDuringReview", func(t *testing.T) { lv.step4(t, b) })
	t.Run("Step1_ManualCommentsHoldTheMerge", func(t *testing.T) { lv.step1(t, a) })
	t.Run("Step5_OwnerPushBeforeMerge", func(t *testing.T) { lv.step5(t, b) })
	t.Run("Step2_BaseUpdateAndFollowUp", func(t *testing.T) { lv.step2(t, b) })
	t.Run("Step3_RoundLimitEscalationAndResume", func(t *testing.T) { lv.step3(t, c) })
	t.Run("Step6_UntrustedCommitEscalates", func(t *testing.T) { lv.step6(t, d) })
	t.Run("Step7_AbortKeepsTheSlotUntilClosed", func(t *testing.T) { lv.step7(t, d) })
	t.Run("Step9_DailyLimitHoldsUntilTheDayBoundary", func(t *testing.T) { lv.step9(t) })
	t.Run("Step8_BoardAndNoCredentials", func(t *testing.T) { lv.step8(t) })
}

type live struct {
	t       *testing.T
	cfg     string
	project string
	prefix  string
	timeout time.Duration
	drv     *githubtest.Driver
	owner   ports.Actor
	human   ports.Actor
	tracker ports.WorkTracker
	host    ports.CodeHost
	board   apiv1.AgentLoopClient
	bin     string
	worker  *exec.Cmd
	logs    []string // files the scenario's processes log to
	dirs    []string // directories whose files are logs (run logs)
	tokens  []string // the owner's and the human's tokens, also checked for
	boards  []string // every board read, for step 8
	bad     []string // board rows with no state, or no next action and no wait
}

func liveEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("AGENT_LOOP_LIVE=1 needs %s (docs/LIVE-SCENARIO.md)", name)
	}
	return v
}

func newLive(t *testing.T) *live {
	t.Helper()
	ctx := context.Background()
	lv := &live{t: t, cfg: liveEnv(t, "AGENT_LOOP_LIVE_CONFIG"), timeout: time.Hour}
	if s := os.Getenv("AGENT_LOOP_LIVE_STEP_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatal(err)
		}
		lv.timeout = d
	}
	dep, err := loadDeployment(lv.cfg)
	if err != nil {
		t.Fatal(err)
	}
	name, prof, err := onlyProject(dep)
	if err != nil {
		t.Fatal(err)
	}
	lv.project, lv.prefix = name, prof.BranchPrefix
	apiBase := os.Getenv("AGENT_LOOP_GITHUB_API")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	ownerTok, humanTok := liveEnv(t, "AGENT_LOOP_GITHUB_OWNER_TOKEN"), liveEnv(t, "AGENT_LOOP_GITHUB_HUMAN_TOKEN")
	lv.tokens = []string{ownerTok, humanTok}

	// The loop's own adapters, with the deployment's app keys, for
	// reading only.
	db, err := projection.Open(ctx, dep.Projection.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	backend, err := secrets.Open(dep.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	store, err := secrets.NewGitHubApps(backend, prof.Identities, secrets.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	setup, run, err := resolveGitHub(ctx, store, prof.CodeHost.Repo)
	t.Cleanup(func() { run.Close(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	gh, err := newGitHubAdapters(name, prof, db, setup)
	if err != nil {
		t.Fatal(err)
	}
	lv.tracker, lv.host = gh.Tracker, gh.Host
	if len(prof.Identities.Owners) == 0 {
		t.Fatal("the profile names no owner account")
	}
	o := prof.Identities.Owners[0]
	lv.owner = ports.Actor{ID: o.ID, Login: o.Login, Kind: ports.ActorHuman}
	lv.human = ports.Actor{ID: liveEnv(t, "AGENT_LOOP_GITHUB_HUMAN_ID"), Login: liveEnv(t, "AGENT_LOOP_GITHUB_HUMAN_LOGIN"), Kind: ports.ActorHuman}
	lv.drv = &githubtest.Driver{BaseURL: apiBase, Repo: prof.CodeHost.Repo, Owner: lv.owner,
		Tokens: map[string]string{lv.owner.ID: ownerTok, lv.human.ID: humanTok}}

	c, closer, err := api.Dial(dep.API.Socket)
	if err != nil {
		t.Fatalf("the server's API (is agent-loop server start running?): %v", err)
	}
	t.Cleanup(func() { closer.Close() })
	lv.board = c

	root, err := workspace.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	lv.dirs = append(lv.dirs, filepath.Join(root, "results"))
	if s := os.Getenv("AGENT_LOOP_LIVE_SERVER_LOG"); s != "" {
		lv.logs = append(lv.logs, s)
	}
	lv.bin = filepath.Join(t.TempDir(), "agent-loop")
	if out, err := exec.Command("go", "build", "-o", lv.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building agent-loop: %v\n%s", err, out)
	}
	lv.startWorker()
	t.Cleanup(lv.stopWorker)
	return lv
}

func (lv *live) startWorker() {
	lv.t.Helper()
	f, err := os.CreateTemp(lv.t.TempDir(), "worker-*.log")
	if err != nil {
		lv.t.Fatal(err)
	}
	lv.logs = append(lv.logs, f.Name())
	lv.worker = exec.Command(lv.bin, "worker", "--capabilities", "agent", "--config", lv.cfg)
	lv.worker.Stdout, lv.worker.Stderr = f, f
	if err := lv.worker.Start(); err != nil {
		lv.t.Fatal(err)
	}
}

// killWorker kills the worker process outright, as a lost host.
func (lv *live) killWorker() {
	if lv.worker != nil && lv.worker.Process != nil {
		lv.worker.Process.Signal(syscall.SIGKILL)
		lv.worker.Wait()
	}
	lv.worker = nil
}

func (lv *live) stopWorker() {
	if lv.worker != nil && lv.worker.Process != nil {
		lv.worker.Process.Signal(syscall.SIGTERM)
		lv.worker.Wait()
	}
}

func (lv *live) newItem(title string) ports.ItemRef {
	lv.t.Helper()
	ref, err := lv.drv.NewItem(lv.owner, "Scenario 11.1 item "+title, "Make a small, safe change to the README for scenario item "+title+".",
		ports.MarkerReady, ports.MarkerTypeFeature)
	if err != nil {
		lv.t.Fatal(err)
	}
	return ref
}

func (lv *live) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond every 10 s until it holds or the step's time is up.
func (lv *live) waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(lv.timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Second)
	}
	return true
}

// row is the item's board row, if it is on the board.
func (lv *live) row(item ports.ItemRef) *apiv1.BoardItem {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := lv.board.Board(ctx, &apiv1.BoardRequest{Project: lv.project})
	if err != nil {
		lv.t.Logf("board: %v", err)
		return nil
	}
	lv.boards = append(lv.boards, b.String())
	for _, p := range b.Projects {
		for _, it := range p.Items {
			if it.State == "" || (it.NextAction == "" && it.WaitReason == "") {
				lv.bad = append(lv.bad, fmt.Sprintf("%s: state %q, next action %q, wait %q", it.Item, it.State, it.NextAction, it.WaitReason))
			}
		}
		for _, it := range p.Items {
			if it.Item == string(item) {
				return it
			}
		}
	}
	return nil
}

func (lv *live) inState(item ports.ItemRef, st string) bool {
	r := lv.row(item)
	return r != nil && r.State == st
}

func (lv *live) change(t *testing.T, item ports.ItemRef) *ports.Change {
	t.Helper()
	ch, err := lv.host.ChangeForItem(context.Background(), item)
	lv.must(t, err)
	return ch
}

// reviews returns the change's review records.
func (lv *live) reviews(t *testing.T, ch ports.ChangeRef) []contract.Record {
	t.Helper()
	rrs, err := lv.host.ReviewRecords(context.Background(), ch)
	lv.must(t, err)
	var out []contract.Record
	for _, rr := range rrs {
		var r contract.Record
		lv.must(t, json.Unmarshal(rr.Record.Data, &r))
		out = append(out, r)
	}
	return out
}

func kinds(rs []contract.Record) []string {
	var out []string
	for _, r := range rs {
		k := string(r.Kind)
		if r.Round != nil {
			k += "/" + strconv.Itoa(*r.Round)
		}
		out = append(out, k)
	}
	return out
}

func (lv *live) merged(item ports.ItemRef) bool {
	it, err := lv.tracker.Item(context.Background(), item)
	return err == nil && it.CloseReason == ports.CloseCompleted
}

func (lv *live) labelled(item ports.ItemRef, m ports.Marker) bool {
	it, err := lv.tracker.Item(context.Background(), item)
	return err == nil && slices.Contains(it.Markers, m)
}

// hold pauses the item as the owner, so a step's owner action lands
// before the merge (work plan #25 D9); release resumes it.
func (lv *live) hold(t *testing.T, item ports.ItemRef) {
	lv.must(t, lv.drv.SetMarker(lv.owner, item, ports.MarkerPaused, true))
	if !lv.waitFor(t, func() bool { return lv.inState(item, "paused") }) {
		t.Fatalf("item %s didn't pause", item)
	}
}

func (lv *live) release(t *testing.T, item ports.ItemRef) {
	lv.must(t, lv.drv.SetMarker(lv.owner, item, ports.MarkerPaused, false))
}

// changedFile is a file the change touches, for the owner's file-level
// review comments.
func (lv *live) changedFile(t *testing.T, ch ports.ChangeRef) string {
	t.Helper()
	req, err := http.NewRequest("GET", lv.drv.BaseURL+"/repos/"+lv.drv.Repo+"/pulls/"+string(ch)+"/files", nil)
	lv.must(t, err)
	req.Header.Set("Authorization", "Bearer "+lv.tokens[0])
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	lv.must(t, err)
	defer res.Body.Close()
	var files []struct {
		Filename string `json:"filename"`
	}
	lv.must(t, json.NewDecoder(res.Body).Decode(&files))
	if len(files) == 0 {
		t.Fatalf("change %s touches no file", ch)
	}
	return files[0].Filename
}

// Step 4: the worker is killed while B is in review and restarted; B
// continues with no duplicate review.
func (lv *live) step4(t *testing.T, b ports.ItemRef) {
	if !lv.waitFor(t, func() bool { r := lv.row(b); return r != nil && r.State == "in_review" && r.Host != "" }) {
		t.Fatal("B never had a review running")
	}
	lv.killWorker()
	time.Sleep(30 * time.Second)
	lv.startWorker()
	if !lv.waitFor(t, func() bool { r := lv.row(b); return r == nil || r.State != "in_review" }) {
		t.Fatal("B's review didn't finish after the restart")
	}
	ch := lv.change(t, b)
	seen := map[string]bool{}
	for _, k := range kinds(lv.reviews(t, ch.Ref)) {
		if seen[k] && strings.HasPrefix(k, "round/") {
			t.Fatalf("B has two reviews for %s: %v", k, kinds(lv.reviews(t, ch.Ref)))
		}
		seen[k] = true
	}
}

// Step 1: three owner comments hold A's merge; the replies and the
// manual-reply review follow; an open ruling waits for the owner, who
// resolves the thread; A merges with no follow-up for the comments.
func (lv *live) step1(t *testing.T, a ports.ItemRef) {
	if !lv.waitFor(t, func() bool { return lv.inState(a, "merging") }) {
		t.Skip("A didn't reach merging (its review left blocking findings, or it merged first)")
	}
	lv.hold(t, a)
	ch := lv.change(t, a)
	file := lv.changedFile(t, ch.Ref)
	for _, body := range []string{"Please rename this.", "Why this way?", "This isn't valid, I think."} {
		_, err := lv.drv.ReviewComment(lv.owner, ch.Ref, file, 0, body)
		lv.must(t, err)
	}
	lv.release(t, a)
	if !lv.waitFor(t, func() bool { return slices.Contains(kinds(lv.reviews(t, ch.Ref)), "manual_reply") }) {
		t.Fatal("no manual-reply review after the owner's comments")
	}
	// The owner clears whatever is still open by resolving its thread.
	mcs, err := lv.host.ManualComments(context.Background(), ch.Ref)
	lv.must(t, err)
	for _, m := range mcs {
		if !m.Cleared && m.Thread != "" {
			lv.must(t, lv.drv.UnresolveThread(lv.owner, ch.Ref, m.Thread))
			lv.must(t, lv.drv.ResolveThread(lv.owner, ch.Ref, m.Thread))
		}
	}
	if !lv.waitFor(t, func() bool { return lv.merged(a) }) {
		t.Fatal("A didn't merge")
	}
	for _, r := range lv.reviews(t, ch.Ref) {
		if r.Kind == string(contract.KindManualReply) && r.Round != nil {
			t.Fatalf("a manual-reply review counted as round %d", *r.Round)
		}
	}
}

// Step 5: an owner push to B between review and merge gets an owner-push
// review that uses no round.
func (lv *live) step5(t *testing.T, b ports.ItemRef) {
	if !lv.waitFor(t, func() bool { return lv.inState(b, "merging") || lv.merged(b) }) || lv.merged(b) {
		t.Skip("B merged, or never reached merging, before the owner could push")
	}
	lv.hold(t, b)
	ch := lv.change(t, b)
	before := lv.row(b).Round
	head, err := lv.drv.Push(lv.owner, ch.Branch)
	lv.must(t, err)
	lv.release(t, b)
	if !lv.waitFor(t, func() bool {
		for _, r := range lv.reviews(t, ch.Ref) {
			if r.Kind == string(contract.KindOwnerPush) && r.Commit == string(head) {
				return true
			}
		}
		return false
	}) {
		t.Fatal("no owner-push review of the owner's head")
	}
	if r := lv.row(b); r != nil && r.Round != before {
		t.Fatalf("the owner-push review used a round: %d, was %d", r.Round, before)
	}
}

// Step 2: B, after A merged, gets a base update with no agent run and a
// base-update review, merges, and a not-ready follow-up lists the findings
// it left open.
func (lv *live) step2(t *testing.T, b ports.ItemRef) {
	ch := lv.change(t, b)
	if ch == nil {
		t.Skip("B has no open change")
	}
	if !lv.waitFor(t, func() bool { return lv.merged(b) }) {
		t.Fatal("B didn't merge")
	}
	rs := lv.reviews(t, ch.Ref)
	if !slices.Contains(kinds(rs), "base_update") {
		t.Skipf("B needed no base update (reviews %v)", kinds(rs))
	}
	recs, err := lv.tracker.FindRecords(context.Background(), b, ports.RecordFollowUp)
	lv.must(t, err)
	last := rs[len(rs)-1]
	open := 0
	for _, f := range last.Findings {
		if f.Status != contract.StatusAddressed && f.Status != contract.StatusWithdrawn {
			open++
		}
	}
	if open == 0 {
		if len(recs) != 0 {
			t.Fatal("a follow-up was filed with no finding left open")
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%d follow-up records for %d open findings, want 1", len(recs), open)
	}
	var rec contract.Record
	lv.must(t, json.Unmarshal(recs[0].Data, &rec))
	if lv.labelled(ports.ItemRef(rec.Item), ports.MarkerReady) {
		t.Fatalf("follow-up %s is ready", rec.Item)
	}
}

// Step 3: C escalated at the round limit, resumed by the owner after a
// push, and merged. Skipped when the agents don't take C to the limit.
func (lv *live) step3(t *testing.T, c ports.ItemRef) {
	if !lv.waitFor(t, func() bool { return lv.inState(c, "escalated") || lv.merged(c) }) || lv.merged(c) {
		t.Skip("the agents didn't take C to an escalation")
	}
	if !lv.labelled(c, ports.MarkerNeedsOwner) {
		t.Fatal("escalated C has no needs-owner label")
	}
	ch := lv.change(t, c)
	_, err := lv.drv.Push(lv.owner, ch.Branch)
	lv.must(t, err)
	lv.must(t, lv.drv.SetMarker(lv.owner, c, ports.MarkerNeedsOwner, false))
	if !lv.waitFor(t, func() bool { return lv.merged(c) || lv.inState(c, "escalated") }) {
		t.Fatal("C neither merged nor escalated again after the resume")
	}
}

// Step 6: on GitHub the push rule refuses an outside push, so the owner
// disables and re-enables the loop-branch ruleset, which makes D's commits
// untrusted (D40). A non-owner's label change does nothing. The owner's
// resume approves the commits and D is reviewed again; a second ruleset
// change escalates D again.
func (lv *live) step6(t *testing.T, d ports.ItemRef) {
	if !lv.waitFor(t, func() bool { return lv.inState(d, "merging") }) {
		t.Skip("D didn't reach merging")
	}
	lv.hold(t, d)
	if _, err := lv.drv.Push(lv.human, lv.change(t, d).Branch); err == nil {
		t.Fatal("the push rule let a non-owner push to D's branch")
	}
	toggle := func() {
		lv.must(t, lv.drv.SetRuleset(github.RulesetNameLoop, "disable"))
		lv.must(t, lv.drv.SetRuleset(github.RulesetNameLoop, "enable"))
	}
	toggle()
	lv.release(t, d)
	if !lv.waitFor(t, func() bool { return lv.inState(d, "escalated") }) {
		t.Fatal("D wasn't escalated after the ruleset change")
	}
	lv.must(t, lv.drv.SetMarker(lv.human, d, ports.MarkerNeedsOwner, false))
	time.Sleep(2 * time.Minute)
	if !lv.inState(d, "escalated") {
		t.Fatal("a non-owner's label change resumed D")
	}
	ch := lv.change(t, d)
	n := len(lv.reviews(t, ch.Ref))
	lv.must(t, lv.drv.SetMarker(lv.owner, d, ports.MarkerNeedsOwner, true))
	lv.must(t, lv.drv.SetMarker(lv.owner, d, ports.MarkerNeedsOwner, false))
	if !lv.waitFor(t, func() bool { return len(lv.reviews(t, ch.Ref)) > n }) {
		t.Fatal("D wasn't reviewed again after the owner's resume")
	}
	lv.hold(t, d)
	toggle()
	lv.release(t, d)
	if !lv.waitFor(t, func() bool { return lv.inState(d, "escalated") }) {
		t.Fatal("the second ruleset change didn't escalate D")
	}
}

// Step 7: the owner aborts D; D and its change are aborted and the change
// stays open; closing D removes aborted from the change.
func (lv *live) step7(t *testing.T, d ports.ItemRef) {
	ch := lv.change(t, d)
	if ch == nil {
		t.Skip("D has no open change")
	}
	lv.must(t, lv.drv.SetMarker(lv.owner, d, ports.MarkerAborted, true))
	if !lv.waitFor(t, func() bool { return lv.inState(d, "aborted") }) {
		t.Fatal("D isn't aborted")
	}
	got, err := lv.host.Change(context.Background(), ch.Ref)
	lv.must(t, err)
	if got.State != ports.ChangeStateOpen || !slices.Contains(got.Markers, ports.MarkerAborted) {
		t.Fatalf("D's change is %s with %v, want open and aborted", got.State, got.Markers)
	}
	lv.must(t, lv.drv.CloseItem(lv.owner, d, ports.CloseNotDone))
	if !lv.waitFor(t, func() bool {
		got, err := lv.host.Change(context.Background(), ch.Ref)
		return err == nil && !slices.Contains(got.Markers, ports.MarkerAborted)
	}) {
		t.Fatal("closing D left aborted on its change")
	}
}

// Step 9 needs a profile with a small daily limit; it is checked from the
// board's project line when AGENT_LOOP_LIVE_DAILY_LIMIT names the
// profile's limits.daily_agent_runs.
func (lv *live) step9(t *testing.T) {
	limit, err := strconv.Atoi(os.Getenv("AGENT_LOOP_LIVE_DAILY_LIMIT"))
	if err != nil || limit <= 0 {
		t.Skip("set AGENT_LOOP_LIVE_DAILY_LIMIT to the profile's small daily limit to run step 9")
	}
	ctx := context.Background()
	reached := lv.waitFor(t, func() bool {
		b, err := lv.board.Board(ctx, &apiv1.BoardRequest{Project: lv.project})
		return err == nil && len(b.Projects) == 1 && b.Projects[0].DailyLimitReached
	})
	if !reached {
		t.Skipf("the project didn't reach its daily limit of %d", limit)
	}
	b, err := lv.board.Board(ctx, &apiv1.BoardRequest{Project: lv.project})
	lv.must(t, err)
	if n := b.Projects[0].DailyRuns; n > int64(limit) {
		t.Fatalf("%d agent runs counted today, over the limit of %d", n, limit)
	}
	// The waiting items continue after 00:00 on their own; the run is
	// left for the owner to watch (docs/LIVE-SCENARIO.md).
}

// Step 8: every board read showed a state and a next action or wait for
// each item, and no credential is in any log.
func (lv *live) step8(t *testing.T) {
	if len(lv.boards) == 0 {
		t.Fatal("the board was never read")
	}
	for _, b := range lv.bad {
		t.Errorf("a board read showed %s", b)
	}
	shape := regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	files := slices.Clone(lv.logs)
	for _, dir := range lv.dirs {
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() && fi.Name() == "out" {
				return filepath.SkipDir // the handler's own files (work plan #25 D5)
			}
			if err == nil && !fi.IsDir() {
				files = append(files, p)
			}
			return nil
		})
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if m := shape.Find(data); m != nil {
			t.Errorf("%s has a credential-shaped string", f)
		}
		for _, tok := range lv.tokens {
			if strings.Contains(string(data), tok) {
				t.Errorf("%s has a test account's token", f)
			}
		}
	}
	t.Logf("checked %d log files and %d board reads", len(files), len(lv.boards))
}
