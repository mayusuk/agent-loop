package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/api"
	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/secrets"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// The live PRODUCT-SPEC §11.1 scenario on a GitHub test repository, with a
// running agent-loop server and a worker this test runs (so step 4 can
// kill it). Every step runs, as in the fake run (§11: "both runs are the
// same", work plan #25 D9): the profile's handlers are scripthandler,
// which hands every run to this test, so each review finds what the step
// needs; the test reports the profile's gate itself, so it holds a merge
// until the owner's action is in; and it acts as the owner and as a second
// human. Each step runs its own items, one after the other, as each fake
// step is its own run (D1). Step 9 needs a profile with a small daily limit,
// so it is its own test. Skipped unless AGENT_LOOP_LIVE=1;
// docs/LIVE-SCENARIO.md has the setup.

func TestScenario11_1_Live(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("the live scenario runs only with AGENT_LOOP_LIVE=1")
	}
	lv := newLive(t, false)
	t.Run("Step1_ManualCommentsHoldTheMerge", lv.step1)
	t.Run("Step2_BaseUpdateAndFollowUp", lv.step2)
	t.Run("Step3_RoundLimitEscalationAndResume", lv.step3)
	t.Run("Step4_WorkerKilledDuringReview", lv.step4)
	t.Run("Step5_OwnerPushBeforeMerge", lv.step5)
	t.Run("Step6_UntrustedCommitEscalates", lv.step6)
	t.Run("Step7_AbortKeepsTheSlotUntilClosed", lv.step7)
	t.Run("Step8_BoardAndNoCredentials", lv.step8)
}

func TestScenario11_1_Live_Step9(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("the live scenario runs only with AGENT_LOOP_LIVE=1")
	}
	lv := newLive(t, true)
	t.Run("Step9_DailyLimitHoldsUntilTheDayBoundary", lv.step9)
	t.Run("NoCredentials", func(t *testing.T) { lv.noCredentials(t) })
}

// step9Limit is the daily limit step 9 needs, as in the fake run: both
// implements and both round reviews, so Y's base-update review waits.
const step9Limit = 4

// liveHandler is one scripted handler run: whether scripthandler commits
// and pushes first, and the out/ files and the result it writes. kill
// kills the worker while the run is in the handler instead (step 4).
type liveHandler struct {
	commit bool
	kill   bool
	finish func(in contract.Input, head string) (files map[string]string, outcome string, body any)
}

// The scripthandler exchange (internal/scripthandler).
type (
	shHello struct {
		Input json.RawMessage `json:"input"`
	}
	shPlan struct {
		Commit bool   `json:"commit"`
		Error  string `json:"error,omitempty"`
	}
	shPushed struct {
		Head    string `json:"head"`
		GHToken string `json:"gh_token,omitempty"`
	}
	shFinish struct {
		Files  map[string]string `json:"files,omitempty"`
		Result json.RawMessage   `json:"result,omitempty"`
		Error  string            `json:"error,omitempty"`
	}
)

// leakText is what a careless handler echoes: scripthandler fills in the
// run's GH_TOKEN, which the worker must mask (PRODUCT-SPEC §8 item 7).
const leakText = "token={{GH_TOKEN}}"

type live struct {
	t       *testing.T
	cfg     string
	project string
	prof    *profile.Profile
	gate    string
	timeout time.Duration
	quiet   time.Duration // how long "has no effect" is watched
	drv     *githubtest.Driver
	owner   ports.Actor
	human   ports.Actor
	coder   ports.Actor // the coder app, for the handler's own thread resolution
	store   *secrets.GitHubApps
	tracker ports.WorkTracker
	host    ports.CodeHost
	db      *projection.DB
	board   apiv1.AgentLoopClient
	bin     string
	logs    []string // files the scenario's processes log to
	dirs    []string // directories whose files are logs (run logs)
	boards  []string // every board read, for step 8
	bad     []string // board rows with no state, or no next action and no wait
	rows    map[ports.ItemRef]*apiv1.BoardItem

	mu       sync.Mutex
	worker   *exec.Cmd
	scripts  map[ports.ItemRef]map[contract.Stage][]liveHandler
	ins      map[ports.ItemRef][]contract.Input // every run handed to a handler
	at       map[string]time.Time               // when each run reached its handler
	held     map[ports.ItemRef]bool
	items    []ports.ItemRef
	tokens   []string // the test accounts' and every run's GH_TOKEN
	problems []string // errors met off the test goroutine
}

func liveEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("AGENT_LOOP_LIVE=1 needs %s (docs/LIVE-SCENARIO.md)", name)
	}
	return v
}

// checkProfile fails unless the profile is the one the scenario needs
// (docs/LIVE-SCENARIO.md): scripthandler on every stage, one gate, and the
// default WIP, rounds and threshold the steps count with.
func checkProfile(t *testing.T, prof *profile.Profile, handler, socket string, step9 bool) {
	t.Helper()
	for _, st := range []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress} {
		sc := prof.Stages[string(st)]
		if !slices.Equal(sc.Command, []string{handler, socket}) || sc.Runner != profile.RunnerLocalUnsandboxed {
			t.Fatalf("stage %s runs %v with runner %q; the scenario needs command = [%q, %q] and runner = %q",
				st, sc.Command, sc.Runner, handler, socket, profile.RunnerLocalUnsandboxed)
		}
	}
	if len(prof.Gates) != 1 {
		t.Fatalf("the profile has gates %v; the scenario reports one gate itself", prof.Gates)
	}
	if prof.WIP != 2 || prof.MaxRounds != 3 || prof.ResumeRounds != 1 || prof.BlockingThreshold != 4 {
		t.Fatalf("wip %d, max_rounds %d, resume_rounds %d, blocking_threshold %d; the scenario needs the defaults 2, 3, 1, 4",
			prof.WIP, prof.MaxRounds, prof.ResumeRounds, prof.BlockingThreshold)
	}
	switch n := prof.Limits.DailyAgentRuns; {
	case step9 && n != step9Limit:
		t.Fatalf("step 9 needs limits.daily_agent_runs = %d, not %d", step9Limit, n)
	case !step9 && n != 0 && n < 100:
		t.Fatalf("steps 1 to 8 need limits.daily_agent_runs of at least 100 (or none), not %d", n)
	}
}

func newLive(t *testing.T, step9 bool) *live {
	t.Helper()
	ctx := context.Background()
	lv := &live{t: t, cfg: liveEnv(t, "AGENT_LOOP_LIVE_CONFIG"), timeout: time.Hour,
		rows: map[ports.ItemRef]*apiv1.BoardItem{}, scripts: map[ports.ItemRef]map[contract.Stage][]liveHandler{},
		ins: map[ports.ItemRef][]contract.Input{}, at: map[string]time.Time{}, held: map[ports.ItemRef]bool{}}
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
	handler, socket := liveEnv(t, "AGENT_LOOP_LIVE_HANDLER"), liveEnv(t, "AGENT_LOOP_LIVE_HANDLER_SOCKET")
	checkProfile(t, prof, handler, socket, step9)
	lv.project, lv.prof, lv.gate = name, prof, prof.Gates[0]
	lv.quiet = 2*prof.Intervals.Poll + 30*time.Second
	apiBase := os.Getenv("AGENT_LOOP_GITHUB_API")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	ownerTok, humanTok := liveEnv(t, "AGENT_LOOP_GITHUB_OWNER_TOKEN"), liveEnv(t, "AGENT_LOOP_GITHUB_HUMAN_TOKEN")
	lv.tokens = []string{ownerTok, humanTok}

	// The loop's own adapters, with the deployment's app keys, for
	// reading; and the coder app, for what a handler does on its own.
	db, err := projection.Open(ctx, dep.Projection.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	lv.db = db
	backend, err := secrets.Open(dep.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	store, err := secrets.NewGitHubApps(backend, prof.Identities, secrets.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	lv.store = store
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
	lv.coder = ports.Actor{ID: "scenario-coder", Login: "coder", Kind: ports.ActorBot}
	lv.drv = &githubtest.Driver{BaseURL: apiBase, Repo: prof.CodeHost.Repo, Owner: lv.owner, Base: prof.BaseBranch,
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
	if out, err := exec.Command("go", "build", "-o", handler, "github.com/mayusuk/agent-loop/internal/scripthandler").CombinedOutput(); err != nil {
		t.Fatalf("building scripthandler: %v\n%s", err, out)
	}
	lv.serveHandlers(socket)
	lv.bin = filepath.Join(t.TempDir(), "agent-loop")
	if out, err := exec.Command("go", "build", "-o", lv.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building agent-loop: %v\n%s", err, out)
	}
	if err := lv.startWorker(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lv.stopWorker)
	lv.runGates()
	t.Cleanup(func() {
		lv.mu.Lock()
		defer lv.mu.Unlock()
		for _, p := range lv.problems {
			t.Error(p)
		}
	})
	return lv
}

func (lv *live) problem(format string, args ...any) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	lv.problems = append(lv.problems, fmt.Sprintf(format, args...))
}

// serveHandlers answers scripthandler on socket for the whole test.
func (lv *live) serveHandlers(socket string) {
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		lv.t.Fatal(err)
	}
	lv.t.Cleanup(func() { ln.Close(); os.Remove(socket) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go lv.handle(c)
		}
	}()
}

func (lv *live) handle(c net.Conn) {
	defer c.Close()
	enc, dec := json.NewEncoder(c), json.NewDecoder(bufio.NewReader(c))
	var h shHello
	if err := dec.Decode(&h); err != nil {
		lv.problem("scripthandler: %v", err)
		return
	}
	in, err := contract.ParseInput(h.Input)
	if err != nil {
		lv.problem("scripthandler input: %v", err)
		return
	}
	item := ports.ItemRef(in.Item.Ref)
	lv.mu.Lock()
	q := lv.scripts[item][in.Stage]
	var hd *liveHandler
	if len(q) > 0 {
		hd, lv.scripts[item][in.Stage] = &q[0], q[1:]
	}
	lv.ins[item] = append(lv.ins[item], in)
	lv.at[in.RunID] = time.Now()
	lv.mu.Unlock()
	if hd == nil {
		lv.problem("item %s: unscripted %s run %s", item, in.Stage, in.RunID)
		enc.Encode(shPlan{Error: "unscripted run"})
		return
	}
	if hd.kill {
		// The worker host is lost with this run in its handler (step 4):
		// the run never reports, and the handler is left to fail.
		lv.killWorker()
		time.Sleep(30 * time.Second)
		if err := lv.startWorker(); err != nil {
			lv.problem("restarting the worker: %v", err)
		}
		return
	}
	if err := enc.Encode(shPlan{Commit: hd.commit}); err != nil {
		lv.problem("scripthandler: %v", err)
		return
	}
	var p shPushed
	if err := dec.Decode(&p); err != nil {
		lv.problem("scripthandler for %s %s: %v", item, in.Stage, err)
		return
	}
	if p.GHToken != "" {
		lv.mu.Lock()
		lv.tokens = append(lv.tokens, p.GHToken)
		lv.mu.Unlock()
	}
	files, outcome, body := hd.finish(in, p.Head)
	res, _ := json.Marshal(map[string]any{"contract_version": "1.0", "run_id": in.RunID, "result": outcome, outcome: body})
	enc.Encode(shFinish{Files: files, Result: res})
}

// on queues handler runs for the item's runs of a stage, in order.
func (lv *live) on(item ports.ItemRef, st contract.Stage, hds ...liveHandler) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	if lv.scripts[item] == nil {
		lv.scripts[item] = map[contract.Stage][]liveHandler{}
	}
	lv.scripts[item][st] = append(lv.scripts[item][st], hds...)
}

// stagesOf returns the stages handed to a handler for the item, in order.
func (lv *live) stagesOf(item ports.ItemRef) []contract.Stage {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	var out []contract.Stage
	for _, in := range lv.ins[item] {
		out = append(out, in.Stage)
	}
	return out
}

func (lv *live) impl() liveHandler {
	return liveHandler{commit: true, finish: func(in contract.Input, head string) (map[string]string, string, any) {
		return map[string]string{"change.md": "The change. " + leakText}, "done",
			map[string]any{"head": head, "change": map[string]any{"title": "Scenario item " + in.Item.Ref, "body_file": "change.md"}}
	}}
}

// review is a scripted review, as in the fake run: rulings for findings
// (open when not named), a ruling per manual comment by its position in
// rule_manual (addressed when not named), and new findings by score.
// before runs first, with the handler live.
type review struct {
	findings map[string]contract.FindingStatus
	manual   map[int]contract.ManualRulingValue
	add      []int
	before   func(in contract.Input)
}

func (lv *live) rev(r review) liveHandler {
	return liveHandler{finish: func(in contract.Input, _ string) (map[string]string, string, any) {
		if r.before != nil {
			r.before(in)
		}
		files := map[string]string{"s.md": "Summary. " + leakText}
		rs, ms, fs := []map[string]any{}, []map[string]any{}, []map[string]any{}
		for _, id := range in.Review.RuleFindings {
			st := r.findings[id]
			if st == "" {
				st = contract.StatusOpen
			}
			rs = append(rs, map[string]any{"finding": id, "status": st})
		}
		for i, id := range in.Review.RuleManual {
			v := r.manual[i]
			if v == "" {
				v = contract.RulingAddressed
			}
			ms = append(ms, map[string]any{"comment": id, "ruling": v})
		}
		for i, score := range r.add {
			name := fmt.Sprintf("n%d.md", i)
			files[name] = "finding body: secret detail"
			fs = append(fs, map[string]any{"score": score, "path": itemFile(in.Item.Ref), "line": 1, "title": "Scenario finding", "body_file": name})
		}
		return files, "done", map[string]any{"commit": in.Review.Commit, "kind": in.Review.Kind, "summary_file": "s.md",
			"rulings": rs, "manual_rulings": ms, "new_findings": fs}
	}}
}

// clean rules everything addressed and adds nothing.
func (lv *live) clean() liveHandler {
	return liveHandler{finish: func(in contract.Input, head string) (map[string]string, string, any) {
		all := map[string]contract.FindingStatus{}
		for _, id := range in.Review.RuleFindings {
			all[id] = contract.StatusAddressed
		}
		return lv.rev(review{findings: all}).finish(in, head)
	}}
}

// address is a scripted address run: push moves the head; finding and
// manual give each reply's kind (explained when nil); during runs with
// the handler live.
type address struct {
	push    bool
	finding func(id string) contract.ReplyKind
	manual  func(i int, id string) contract.ReplyKind
	during  func(in contract.Input)
}

func (lv *live) addr(a address) liveHandler {
	return liveHandler{commit: a.push, finish: func(in contract.Input, head string) (map[string]string, string, any) {
		files := map[string]string{}
		rs, ms := []map[string]any{}, []map[string]any{}
		for _, id := range in.Address.AnswerFindings {
			k := contract.ReplyExplained
			if a.finding != nil {
				k = a.finding(id)
			}
			files["r"+id+".md"] = "Reply to " + id + ". " + leakText
			rs = append(rs, map[string]any{"finding": id, "kind": k, "body_file": "r" + id + ".md"})
		}
		for i, id := range in.Address.AnswerManual {
			k := contract.ReplyExplained
			if a.manual != nil {
				k = a.manual(i, id)
			}
			name := fmt.Sprintf("m%d.md", i)
			files[name] = "Manual reply."
			ms = append(ms, map[string]any{"comment": id, "kind": k, "body_file": name})
		}
		if a.during != nil {
			a.during(in)
		}
		return files, "done", map[string]any{"head": head, "replies": rs, "manual_replies": ms}
	}}
}

// itemFile is the file scripthandler's commits change for the item.
func itemFile(item string) string { return "scenario/item-" + item + ".txt" }

// runGates reports the profile's gate on every scenario change's head,
// passing unless the item is held, as the fake run's gates do.
func (lv *live) runGates() {
	stop := make(chan struct{})
	done := make(chan struct{})
	lv.t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		set := map[string]bool{}
		for {
			lv.mu.Lock()
			items := slices.Clone(lv.items)
			lv.mu.Unlock()
			for _, it := range items {
				ch, err := lv.host.ChangeForItem(context.Background(), it)
				if err != nil || ch == nil || ch.State != ports.ChangeStateOpen {
					continue
				}
				st := ports.GatePass
				if lv.isHeld(it) {
					st = ports.GatePending
				}
				if k := string(ch.Head) + ":" + string(st); !set[k] {
					if err := lv.drv.SetGate(ch.Head, lv.gate, st); err == nil {
						set[k] = true
					}
				}
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

func (lv *live) isHeld(item ports.ItemRef) bool {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	return lv.held[item]
}

// hold keeps the item's gate pending until release.
func (lv *live) hold(item ports.ItemRef) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	lv.held[item] = true
}

func (lv *live) release(item ports.ItemRef) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	delete(lv.held, item)
}

func (lv *live) startWorker() error {
	f, err := os.CreateTemp(lv.t.TempDir(), "worker-*.log")
	if err != nil {
		return err
	}
	w := exec.Command(lv.bin, "worker", "--capabilities", "agent", "--config", lv.cfg)
	w.Stdout, w.Stderr = f, f
	if err := w.Start(); err != nil {
		return err
	}
	lv.mu.Lock()
	defer lv.mu.Unlock()
	lv.logs = append(lv.logs, f.Name())
	lv.worker = w
	return nil
}

// killWorker kills the worker process outright, as a lost host.
func (lv *live) killWorker() {
	lv.mu.Lock()
	w := lv.worker
	lv.worker = nil
	lv.mu.Unlock()
	if w != nil && w.Process != nil {
		w.Process.Signal(syscall.SIGKILL)
		w.Wait()
	}
}

func (lv *live) stopWorker() {
	lv.mu.Lock()
	w := lv.worker
	lv.mu.Unlock()
	if w != nil && w.Process != nil {
		w.Process.Signal(syscall.SIGTERM)
		w.Wait()
	}
}

// newItem creates a ready feature as the owner. The step's cleanup
// closes it if the step left it open, so the next step has both slots.
func (lv *live) newItem(t *testing.T, title string) ports.ItemRef {
	t.Helper()
	ref, err := lv.drv.NewItem(lv.owner, "Scenario 11.1 item "+title, "Scenario item "+title+": a scripted change.",
		ports.MarkerReady, ports.MarkerTypeFeature)
	if err != nil {
		t.Fatal(err)
	}
	lv.mu.Lock()
	lv.items = append(lv.items, ref)
	lv.mu.Unlock()
	t.Cleanup(func() {
		if it, err := lv.tracker.Item(context.Background(), ref); err == nil && it.CloseReason == "" {
			lv.drv.CloseItem(lv.owner, ref, ports.CloseNotDone)
		}
		lv.release(ref)
	})
	return ref
}

func (lv *live) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// await polls cond every 10 s until it holds, and fails the step if the
// step's time runs out first.
func (lv *live) await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(lv.timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Second)
	}
}

// readBoard reads the project's board, checking every row has a state
// and a next action or a wait (step 8).
func (lv *live) readBoard() *apiv1.BoardReply {
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
			lv.rows[ports.ItemRef(it.Item)] = it
		}
	}
	return b
}

// row is the item's board row, if it is on the board.
func (lv *live) row(item ports.ItemRef) *apiv1.BoardItem {
	b := lv.readBoard()
	if b == nil {
		return nil
	}
	for _, p := range b.Projects {
		for _, it := range p.Items {
			if it.Item == string(item) {
				return it
			}
		}
	}
	return nil
}

func (lv *live) inState(item ports.ItemRef, st core.State) bool {
	r := lv.row(item)
	return r != nil && r.State == string(st)
}

func (lv *live) change(t *testing.T, item ports.ItemRef) ports.Change {
	t.Helper()
	ch, err := lv.host.ChangeForItem(context.Background(), item)
	lv.must(t, err)
	if ch == nil {
		t.Fatalf("item %s has no open change", item)
	}
	return *ch
}

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
		k := r.Kind
		if r.Round != nil {
			k += "/" + strconv.Itoa(*r.Round)
		}
		out = append(out, k)
	}
	return out
}

func (lv *live) reviewKinds(t *testing.T, ch ports.ChangeRef) []string {
	t.Helper()
	return kinds(lv.reviews(t, ch))
}

func (lv *live) records(t *testing.T, item ports.ItemRef, typ ports.RecordType) []contract.Record {
	t.Helper()
	recs, err := lv.tracker.FindRecords(context.Background(), item, typ)
	lv.must(t, err)
	var out []contract.Record
	for _, r := range recs {
		var c contract.Record
		lv.must(t, json.Unmarshal(r.Data, &c))
		out = append(out, c)
	}
	return out
}

func (lv *live) escalations(t *testing.T, item ports.ItemRef) int {
	t.Helper()
	n := 0
	for _, r := range lv.records(t, item, ports.RecordStatus) {
		if r.Event == contract.EventEscalate {
			n++
		}
	}
	return n
}

func (lv *live) merged(item ports.ItemRef) bool {
	it, err := lv.tracker.Item(context.Background(), item)
	return err == nil && it.CloseReason == ports.CloseCompleted
}

func (lv *live) labelled(item ports.ItemRef, m ports.Marker) bool {
	it, err := lv.tracker.Item(context.Background(), item)
	return err == nil && slices.Contains(it.Markers, m)
}

func (lv *live) changeMarked(t *testing.T, ref ports.ChangeRef, m ports.Marker) bool {
	t.Helper()
	ch, err := lv.host.Change(context.Background(), ref)
	lv.must(t, err)
	return slices.Contains(ch.Markers, m)
}

// get reads a repository path as the owner.
func (lv *live) get(t *testing.T, path string, out any) {
	t.Helper()
	lv.must(t, lv.drv.Call(lv.owner, http.MethodGet, path, nil, out))
}

// asCoder runs f with the coder app's token in the driver, as a handler
// acting on its own would (step 1's thread resolution).
func (lv *live) asCoder(f func() error) error {
	ctx := context.Background()
	tok, err := lv.store.Token(ctx, ports.IdentityCoder, ports.Scope{Repos: []string{lv.prof.CodeHost.Repo},
		Permissions: map[string]string{"pull_requests": "write", "metadata": "read"}})
	if err != nil {
		return err
	}
	defer lv.store.Revoke(ctx, tok)
	lv.mu.Lock()
	lv.drv.Tokens[lv.coder.ID] = tok.Secret()
	lv.tokens = append(lv.tokens, tok.Secret())
	lv.mu.Unlock()
	return f()
}

// changeRefs returns the changes the item's handler runs were given.
func (lv *live) changeRefs(item ports.ItemRef) []ports.ChangeRef {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	var out []ports.ChangeRef
	for _, in := range lv.ins[item] {
		if in.Change != nil && in.Change.Ref != "" && !slices.Contains(out, ports.ChangeRef(in.Change.Ref)) {
			out = append(out, ports.ChangeRef(in.Change.Ref))
		}
	}
	return out
}

// manualIDs maps the change's manual comment threads to their IDs.
func (lv *live) manualIDs(t *testing.T, ref ports.ChangeRef) map[ports.ThreadRef]string {
	t.Helper()
	mcs, err := lv.host.ManualComments(context.Background(), ref)
	lv.must(t, err)
	ids := map[ports.ThreadRef]string{}
	for _, m := range mcs {
		ids[m.Thread] = m.ID
	}
	return ids
}

// Step 1: A's clean round-1 review, then three owner comments hold the
// merge. An address run fixes the first (moving the head), explains the
// second and calls the third not valid, and the coder's resolution of the
// third thread is ignored. The manual-reply review, which is no round,
// rules the third open; the owner resolves it before the next merge
// check, and A merges with no follow-up.
func (lv *live) step1(t *testing.T) {
	a := lv.newItem(t, "A")
	lv.hold(a)
	var threads []ports.ThreadRef
	var manualIDs, ruled []string
	lv.on(a, contract.StageImplement, lv.impl())
	lv.on(a, contract.StageReview, lv.clean())
	lv.on(a, contract.StageAddress, lv.addr(address{
		push: true,
		manual: func(i int, id string) contract.ReplyKind {
			manualIDs = append(manualIDs, id)
			return []contract.ReplyKind{contract.ReplyFixed, contract.ReplyExplained, contract.ReplyNotValid}[i%3]
		},
		during: func(in contract.Input) {
			// The handler tries to clear the third comment itself.
			if err := lv.asCoder(func() error { return lv.drv.ResolveThread(lv.coder, ports.ChangeRef(in.Change.Ref), threads[2]) }); err != nil {
				lv.problem("step 1: the coder's resolution: %v", err)
			}
		},
	}))
	lv.on(a, contract.StageReview, lv.rev(review{
		manual: map[int]contract.ManualRulingValue{2: contract.RulingOpen},
		before: func(in contract.Input) {
			ruled = in.Review.RuleManual
			if in.Review.Kind != contract.KindManualReply {
				lv.problem("step 1: A's second review is %s, want manual_reply", in.Review.Kind)
			}
			// The owner clears the third comment by resolving its thread,
			// before the next merge check.
			ref := ports.ChangeRef(in.Change.Ref)
			if err := lv.drv.UnresolveThread(lv.owner, ref, threads[2]); err != nil {
				lv.problem("step 1: unresolve: %v", err)
			}
			if err := lv.drv.ResolveThread(lv.owner, ref, threads[2]); err != nil {
				lv.problem("step 1: resolve: %v", err)
			}
		},
	}))
	lv.await(t, "A merging on its held gate", func() bool { return lv.inState(a, core.StateMerging) })
	ch := lv.change(t, a)
	for _, body := range []string{"Please rename this.", "Why this way?", "Split this file."} {
		tr, err := lv.drv.ReviewComment(lv.owner, ch.Ref, itemFile(string(a)), 1, body)
		lv.must(t, err)
		threads = append(threads, tr)
	}
	lv.release(a)
	lv.await(t, "A merged", func() bool { return lv.merged(a) })

	if len(manualIDs) != 3 || !slices.Equal(ruled, manualIDs) {
		t.Fatalf("answered %v, manual-reply review ruled %v: the coder's resolution must not clear the third", manualIDs, ruled)
	}
	if got := lv.reviewKinds(t, ch.Ref); !slices.Equal(got, []string{"round/1", "manual_reply"}) {
		t.Fatalf("A's reviews %v: the manual-reply review must not count as a round", got)
	}
	if got := lv.stagesOf(a); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress, contract.StageReview}) {
		t.Fatalf("A's stages %v", got)
	}
	if fu := lv.records(t, a, ports.RecordFollowUp); len(fu) != 0 {
		t.Fatalf("follow-ups filed for A: %v", fu)
	}
}

// Step 2: B's round-1 score-4 finding is addressed and round 2 leaves a
// score-2 finding. A merged first, so B's head lacks the base tip: the
// worker merges the base with no agent run (D41), a base-update review
// (no round) follows, and B merges. A not-ready follow-up lists the
// score-2 finding by ID, score, status and link only.
func (lv *live) step2(t *testing.T) {
	a, b := lv.newItem(t, "A2"), lv.newItem(t, "B")
	lv.hold(a)
	lv.hold(b)
	lv.on(a, contract.StageImplement, lv.impl())
	lv.on(a, contract.StageReview, lv.clean())
	lv.on(b, contract.StageImplement, lv.impl())
	lv.on(b, contract.StageReview, lv.rev(review{add: []int{4}, before: func(contract.Input) { lv.release(a) }}))
	lv.on(b, contract.StageAddress, lv.addr(address{push: true, finding: func(string) contract.ReplyKind { return contract.ReplyFixed }}))
	lv.on(b, contract.StageReview,
		lv.rev(review{findings: map[string]contract.FindingStatus{"F1": contract.StatusAddressed}, add: []int{2}}),
		lv.rev(review{before: func(in contract.Input) {
			if in.Review.Kind != contract.KindBaseUpdate {
				lv.problem("step 2: B's third review is %s, want base_update", in.Review.Kind)
			}
		}}))
	lv.await(t, "A merged", func() bool { return lv.merged(a) })
	ch := lv.change(t, b)
	lv.release(b)
	lv.await(t, "B merged", func() bool { return lv.merged(b) })

	if got := lv.reviewKinds(t, ch.Ref); !slices.Equal(got, []string{"round/1", "round/2", "base_update"}) {
		t.Fatalf("B's reviews %v", got)
	}
	if got := lv.stagesOf(b); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress, contract.StageReview, contract.StageReview}) {
		t.Fatalf("B's stages %v: the base update must run no handler", got)
	}
	rec := lv.reviews(t, ch.Ref)
	var commit struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	lv.get(t, "/commits/"+rec[2].Commit, &commit)
	if len(commit.Parents) != 2 || commit.Parents[0].SHA != rec[1].Commit {
		t.Fatalf("base-update head %s has parents %v, want %s and the base tip", rec[2].Commit, commit.Parents, rec[1].Commit)
	}
	fus := lv.records(t, b, ports.RecordFollowUp)
	if len(fus) != 1 {
		t.Fatalf("%d follow-up records, want 1", len(fus))
	}
	fu := ports.ItemRef(fus[0].Item)
	lv.mu.Lock()
	lv.items = append(lv.items, fu)
	lv.mu.Unlock()
	t.Cleanup(func() { lv.drv.CloseItem(lv.owner, fu, ports.CloseNotDone) })
	if lv.labelled(fu, ports.MarkerReady) || !lv.labelled(fu, ports.MarkerFollowUp) {
		t.Fatalf("follow-up %s: want follow-up and not ready", fu)
	}
	var issue struct {
		Body string `json:"body"`
	}
	lv.get(t, "/issues/"+string(fu), &issue)
	if !strings.Contains(issue.Body, "| F2 | 2 | open |") || strings.Contains(issue.Body, "F1") || strings.Contains(issue.Body, "finding body") {
		t.Fatalf("follow-up body must list F2 by ID, score, status and link only:\n%s", issue.Body)
	}
}

// Step 3: a manual comment during C's round 3, which still has a score-5
// finding, escalates C (needs-owner, a notification, the comment listed)
// and C keeps its WIP slot. The owner pushes a fix and resumes; an address
// run answers both and a clean fourth review uses the one extra round. A
// later manual comment doesn't escalate: it is answered, ruled open,
// answered again and ruled addressed, and C merges. Neither manual-reply
// review counts as a round.
func (lv *live) step3(t *testing.T) {
	c := lv.newItem(t, "C")
	lv.hold(c)
	var first, second ports.ThreadRef
	lv.on(c, contract.StageImplement, lv.impl())
	lv.on(c, contract.StageReview,
		lv.rev(review{add: []int{5}}),
		lv.rev(review{}),
		lv.rev(review{before: func(in contract.Input) {
			var err error
			if first, err = lv.drv.ReviewComment(lv.owner, ports.ChangeRef(in.Change.Ref), itemFile(string(c)), 1, "This needs a test."); err != nil {
				lv.problem("step 3: comment: %v", err)
			}
		}}),
		lv.rev(review{findings: map[string]contract.FindingStatus{"F1": contract.StatusAddressed}}),
		lv.rev(review{manual: map[int]contract.ManualRulingValue{0: contract.RulingOpen}}),
		lv.rev(review{}),
	)
	var answered [][]string
	record := func(in contract.Input) {
		answered = append(answered, append(slices.Clone(in.Address.AnswerFindings), in.Address.AnswerManual...))
	}
	lv.on(c, contract.StageAddress,
		lv.addr(address{}), lv.addr(address{}),
		lv.addr(address{during: record}),
		lv.addr(address{during: record}),
		lv.addr(address{during: record}),
	)
	lv.await(t, "C escalated at the round limit", func() bool { return lv.inState(c, core.StateEscalated) })
	if !lv.labelled(c, ports.MarkerNeedsOwner) {
		t.Fatal("escalated C has no needs-owner label")
	}
	if !lv.labelled(c, ports.MarkerInProgress) || lv.row(c) == nil {
		t.Fatal("escalated C doesn't hold its WIP slot")
	}
	ch := lv.change(t, c)
	_, err := lv.drv.Push(lv.owner, ch.Branch)
	lv.must(t, err)
	lv.must(t, lv.drv.SetMarker(lv.owner, c, ports.MarkerNeedsOwner, false))
	lv.await(t, "C's fourth review and merging", func() bool {
		return slices.Contains(lv.reviewKinds(t, ch.Ref), "round/4") && lv.inState(c, core.StateMerging)
	})
	if r := lv.rows[c]; r.Round != 4 || r.RoundsAllowed != 4 {
		t.Fatalf("C used %d of %d rounds, want 4 of 4", r.Round, r.RoundsAllowed)
	}
	second, err = lv.drv.ReviewComment(lv.owner, ch.Ref, itemFile(string(c)), 1, "One more thing.")
	lv.must(t, err)
	lv.release(c)
	lv.await(t, "C merged", func() bool { return lv.merged(c) })

	if n := lv.escalations(t, c); n != 1 {
		t.Fatalf("C escalated %d times, want once", n)
	}
	if got := lv.reviewKinds(t, ch.Ref); !slices.Equal(got, []string{"round/1", "round/2", "round/3", "round/4", "manual_reply", "manual_reply"}) {
		t.Fatalf("C's reviews %v", got)
	}
	ids := lv.manualIDs(t, ch.Ref)
	var comments []struct {
		Body string `json:"body"`
	}
	lv.get(t, "/issues/"+string(c)+"/comments?per_page=100", &comments)
	listed := false
	for _, cm := range comments {
		listed = listed || strings.Contains(cm.Body, ids[first])
	}
	if ids[first] == "" || !listed {
		t.Errorf("the escalation comment doesn't list %q", ids[first])
	}
	want := [][]string{{"F1", ids[first]}, {ids[second]}, {ids[second]}}
	if fmt.Sprint(answered) != fmt.Sprint(want) {
		t.Fatalf("address runs answered %v, want %v", answered, want)
	}
	// The desktop notification is the decision's notify action; the
	// deployment's notifiers deliver it.
	ds, err := lv.db.Decisions(context.Background(), lv.project, string(c))
	lv.must(t, err)
	notified := false
	for _, d := range ds {
		notified = notified || (d.Record.To == core.StateEscalated && strings.HasPrefix(d.Record.Rule, "FR-6 AC3") && slices.Contains(d.Record.Actions, "notify"))
	}
	if !notified {
		t.Fatal("C's round-limit escalation didn't notify")
	}
}

// Step 4: the worker host is killed while B is in review, with the review
// in its handler, and restarted. B continues with no duplicate review: the
// rerun posts the one round-1 review, and B merges (NFR-1 AC1).
func (lv *live) step4(t *testing.T) {
	b := lv.newItem(t, "B4")
	lv.on(b, contract.StageImplement, lv.impl())
	lv.on(b, contract.StageReview, liveHandler{kill: true}, lv.clean())
	lv.await(t, "B4 merged", func() bool { return lv.merged(b) })

	if got := lv.stagesOf(b); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageReview}) {
		t.Fatalf("B4's handler runs %v: want the killed review and its rerun", got)
	}
	refs := lv.changeRefs(b)
	if len(refs) != 1 {
		t.Fatalf("B4's runs name changes %v, want one", refs)
	}
	if got := lv.reviewKinds(t, refs[0]); !slices.Equal(got, []string{"round/1"}) {
		t.Fatalf("B4's reviews %v, want one", got)
	}
	runs, err := lv.db.Runs(context.Background(), lv.project, string(b))
	lv.must(t, err)
	lost := false
	for _, r := range runs {
		lost = lost || (r.Stage == contract.StageReview && r.Result == contract.ReasonWorkerLost)
	}
	if !lost {
		t.Fatalf("B4's runs %v: the killed run isn't recorded as worker_lost", runs)
	}
}

// Step 5: the owner pushes to B's branch between review and merge. The
// merge stops and an owner-push review follows, which doesn't use the
// round allowance.
func (lv *live) step5(t *testing.T) {
	b := lv.newItem(t, "B5")
	lv.hold(b)
	lv.on(b, contract.StageImplement, lv.impl())
	lv.on(b, contract.StageReview, lv.clean(), lv.clean())
	lv.await(t, "B5 merging on its held gate", func() bool { return lv.inState(b, core.StateMerging) })
	ch := lv.change(t, b)
	pushed, err := lv.drv.Push(lv.owner, ch.Branch)
	lv.must(t, err)
	lv.release(b)
	lv.await(t, "B5 merged", func() bool { return lv.merged(b) })

	recs := lv.reviews(t, ch.Ref)
	if got := kinds(recs); !slices.Equal(got, []string{"round/1", "owner_push"}) {
		t.Fatalf("B5's reviews %v", got)
	}
	if recs[1].Commit != string(pushed) {
		t.Fatalf("owner-push review of %s, want the owner's head %s", recs[1].Commit, pushed)
	}
	if r := lv.rows[b]; r == nil || r.Round != 1 {
		t.Fatalf("B5's last board row %v: want round 1", r)
	}
}

// Step 6: on GitHub the push rule refuses an outside push, so the owner
// disables and re-enables the loop-branch ruleset, which makes D's commits
// untrusted (D40). A label change by a human who isn't an owner does
// nothing. D is escalated, and not addressed or merged. The owner's resume
// approves those commits and D is reviewed again; a second ruleset change
// escalates D again.
func (lv *live) step6(t *testing.T) {
	d := lv.newItem(t, "D")
	lv.hold(d)
	lv.on(d, contract.StageImplement, lv.impl())
	lv.on(d, contract.StageReview, lv.clean(), lv.clean(), lv.clean())
	lv.await(t, "D merging on its held gate", func() bool { return lv.inState(d, core.StateMerging) })
	ch := lv.change(t, d)
	first := ch.Head
	if _, err := lv.drv.Push(lv.human, ch.Branch); !errors.Is(err, ports.ErrPermission) {
		t.Fatalf("a non-owner's push to D's branch: %v, want refused by the push rule", err)
	}
	toggle := func() {
		lv.must(t, lv.drv.SetRuleset(github.RulesetNameLoop, "disable"))
		lv.must(t, lv.drv.SetRuleset(github.RulesetNameLoop, "enable"))
	}
	toggle()
	lv.await(t, "D escalated after the ruleset change", func() bool { return lv.inState(d, core.StateEscalated) })
	if !lv.labelled(d, ports.MarkerNeedsOwner) {
		t.Fatal("the escalation set no needs-owner label")
	}
	// A human who isn't an owner removes needs-owner: no effect, and the
	// loop doesn't put the label back (FR-13 AC7, work plan D6).
	runs := len(lv.stagesOf(d))
	lv.must(t, lv.drv.SetMarker(lv.human, d, ports.MarkerNeedsOwner, false))
	time.Sleep(lv.quiet)
	if !lv.inState(d, core.StateEscalated) || len(lv.stagesOf(d)) != runs {
		t.Fatalf("a non-owner's label change had an effect: row %v, stages %v", lv.row(d), lv.stagesOf(d))
	}
	reviews := len(lv.reviews(t, ch.Ref))
	lv.must(t, lv.drv.SetMarker(lv.owner, d, ports.MarkerNeedsOwner, true))
	lv.must(t, lv.drv.SetMarker(lv.owner, d, ports.MarkerNeedsOwner, false))
	lv.await(t, "D reviewed again after the owner's resume, and merging", func() bool {
		return len(lv.reviews(t, ch.Ref)) > reviews && lv.inState(d, core.StateMerging)
	})
	toggle()
	lv.await(t, "D escalated after the second ruleset change", func() bool { return lv.inState(d, core.StateEscalated) })
	if !lv.labelled(d, ports.MarkerNeedsOwner) {
		t.Fatal("the second escalation set no needs-owner label")
	}

	if slices.Contains(lv.stagesOf(d), contract.StageAddress) || lv.merged(d) {
		t.Fatalf("an untrusted D was addressed or merged: stages %v", lv.stagesOf(d))
	}
	if n := lv.escalations(t, d); n != 2 {
		t.Fatalf("D escalated %d times, want 2", n)
	}
	reviewed := false
	for _, r := range lv.reviews(t, ch.Ref)[reviews:] {
		reviewed = reviewed || r.Commit == string(first)
	}
	if !reviewed {
		t.Fatalf("D wasn't reviewed again on the approved head %s: %v", first, lv.reviewKinds(t, ch.Ref))
	}
	approved := false
	for _, r := range lv.records(t, d, ports.RecordStatus) {
		approved = approved || slices.Contains(r.Commits, string(first))
	}
	if !approved {
		t.Fatalf("no record approves %s", first)
	}
	lv.must(t, lv.drv.CloseItem(lv.owner, d, ports.CloseNotDone))
	lv.await(t, "D off the board", func() bool { return lv.row(d) == nil })
}

// Step 7: the owner aborts D. D and its change get aborted, the change
// stays open, and D keeps its WIP slot (so a third ready item waits) until
// the owner closes D, which removes aborted from the change.
func (lv *live) step7(t *testing.T) {
	d, e := lv.newItem(t, "D7"), lv.newItem(t, "E")
	lv.hold(d)
	lv.hold(e)
	for _, it := range []ports.ItemRef{d, e} {
		lv.on(it, contract.StageImplement, lv.impl())
		lv.on(it, contract.StageReview, lv.clean())
	}
	lv.await(t, "D7 and E merging on their held gates", func() bool {
		return lv.inState(d, core.StateMerging) && lv.inState(e, core.StateMerging)
	})
	ref := lv.change(t, d).Ref
	lv.must(t, lv.drv.SetMarker(lv.owner, d, ports.MarkerAborted, true))
	f := lv.newItem(t, "F")
	lv.on(f, contract.StageImplement, lv.impl())
	lv.on(f, contract.StageReview, lv.clean())
	lv.await(t, "D7 aborted", func() bool { return lv.inState(d, core.StateAborted) })
	time.Sleep(lv.quiet)
	if !lv.changeMarked(t, ref, ports.MarkerAborted) {
		t.Fatal("D7's change isn't marked aborted")
	}
	if got, err := lv.host.Change(context.Background(), ref); err != nil || got.State != ports.ChangeStateOpen {
		t.Fatalf("D7's change is %v (%v), want open", got.State, err)
	}
	if lv.row(d) == nil || !lv.labelled(d, ports.MarkerInProgress) {
		t.Fatal("aborted D7 doesn't hold its WIP slot")
	}
	if lv.labelled(f, ports.MarkerInProgress) {
		t.Fatal("F was claimed while aborted D7 held a slot")
	}
	lv.must(t, lv.drv.CloseItem(lv.owner, d, ports.CloseNotDone))
	lv.await(t, "aborted removed from D7's change", func() bool { return !lv.changeMarked(t, ref, ports.MarkerAborted) })
	if got, err := lv.host.Change(context.Background(), ref); err != nil || got.State != ports.ChangeStateOpen {
		t.Fatalf("D7's change is %v (%v); closing the item must not close it", got.State, err)
	}
	// F merges first, so E's head lacks the base tip: a base-update
	// review follows.
	lv.on(e, contract.StageReview, lv.clean())
	lv.await(t, "F merged", func() bool { return lv.merged(f) })
	lv.release(e)
	lv.await(t, "E merged", func() bool { return lv.merged(e) })
	lv.drv.CloseChange(lv.owner, ref)
}

// boardShows checks that the board holds exactly the items named, each in
// its state with a next action or a wait (FR-11 AC1).
func (lv *live) boardShows(t *testing.T, when string, want map[ports.ItemRef]core.State) {
	t.Helper()
	b := lv.readBoard()
	if b == nil || len(b.Projects) != 1 {
		t.Fatalf("%s: no board", when)
	}
	rows := map[string]*apiv1.BoardItem{}
	for _, it := range b.Projects[0].Items {
		rows[it.Item] = it
	}
	if len(rows) != len(want) {
		t.Errorf("%s: board %v, want %v", when, rows, want)
	}
	for item, st := range want {
		r, ok := rows[string(item)]
		switch {
		case !ok:
			t.Errorf("%s: %s isn't on the board", when, item)
		case r.State != string(st):
			t.Errorf("%s: the board shows %s %s, want %s", when, item, r.State, st)
		case r.NextAction == "" && r.WaitReason == "":
			t.Errorf("%s: the board shows no next action or wait for %s (%s)", when, item, r.State)
		}
	}
}

// Step 8: throughout, the board shows each item's state and next action,
// and no credential appears in any log. Every board read of steps 1 to 7
// was checked; here the board is also read at each turn of a run with an
// item that merges and one that escalates, and then every log and every
// text the worker posted is checked.
func (lv *live) step8(t *testing.T) {
	a, b := lv.newItem(t, "A8"), lv.newItem(t, "B8")
	lv.hold(a)
	lv.on(a, contract.StageImplement, lv.impl())
	lv.on(a, contract.StageReview, lv.clean())
	lv.on(b, contract.StageImplement, liveHandler{finish: func(in contract.Input, _ string) (map[string]string, string, any) {
		return map[string]string{"q.md": "Which API? " + leakText}, "needs_owner", map[string]any{"question_file": "q.md"}
	}})
	lv.await(t, "A8 merging and B8 escalated", func() bool {
		return lv.inState(a, core.StateMerging) && lv.inState(b, core.StateEscalated)
	})
	lv.boardShows(t, "A8 waiting on its gate, B8 escalated", map[ports.ItemRef]core.State{a: core.StateMerging, b: core.StateEscalated})
	lv.release(a)
	lv.await(t, "A8 merged", func() bool { return lv.merged(a) && lv.row(a) == nil })
	lv.boardShows(t, "after A8 merged", map[ports.ItemRef]core.State{b: core.StateEscalated})
	lv.must(t, lv.drv.CloseItem(lv.owner, b, ports.CloseNotDone))
	lv.await(t, "B8 off the board", func() bool { return lv.row(b) == nil })
	lv.boardShows(t, "after B8 closed", map[ports.ItemRef]core.State{})

	var comments []struct {
		Body string `json:"body"`
	}
	lv.get(t, "/issues/"+string(b)+"/comments?per_page=100", &comments)
	asked := false
	for _, c := range comments {
		asked = asked || strings.Contains(c.Body, "Which API?")
	}
	if !asked {
		t.Fatal("the escalation didn't post the handler's question, so the check saw no handler text")
	}
	for _, r := range lv.bad {
		t.Errorf("a board read showed %s", r)
	}
	lv.noCredentials(t)
}

// postedTexts returns every text the worker posted on the scenario's
// items and changes: item comments, change bodies, reviews, review
// comments and replies.
func (lv *live) postedTexts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	type body struct {
		Body string `json:"body"`
	}
	lv.mu.Lock()
	items := slices.Clone(lv.items)
	lv.mu.Unlock()
	for _, it := range items {
		var issue body
		lv.get(t, "/issues/"+string(it), &issue)
		out["item "+string(it)] = issue.Body
		var cs []body
		lv.get(t, "/issues/"+string(it)+"/comments?per_page=100", &cs)
		for i, c := range cs {
			out[fmt.Sprintf("item %s comment %d", it, i)] = c.Body
		}
		for _, ref := range lv.changeRefs(it) {
			var pr body
			lv.get(t, "/pulls/"+string(ref), &pr)
			out["change "+string(ref)] = pr.Body
			for _, path := range []string{"/reviews", "/comments"} {
				var rs []body
				lv.get(t, "/pulls/"+string(ref)+path+"?per_page=100", &rs)
				for i, x := range rs {
					out[fmt.Sprintf("change %s%s %d", ref, path, i)] = x.Body
				}
			}
		}
	}
	return out
}

// noCredentials checks every log the scenario's processes wrote, every run
// log, and every text the worker posted, for a token of the test accounts,
// any token a run was given, and anything shaped like a token or a key
// (PRODUCT-SPEC §8 item 7, work plan #25 D5).
func (lv *live) noCredentials(t *testing.T) {
	t.Helper()
	shape := regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	lv.mu.Lock()
	files, tokens := slices.Clone(lv.logs), slices.Clone(lv.tokens)
	lv.mu.Unlock()
	if len(tokens) <= 2 {
		t.Fatal("no run's GH_TOKEN was seen, so the check proves nothing")
	}
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
	sources := lv.postedTexts(t)
	for _, f := range files {
		if data, err := os.ReadFile(f); err == nil {
			sources[f] = string(data)
		}
	}
	for name, text := range sources {
		if shape.MatchString(text) {
			t.Errorf("%s has a credential-shaped string", name)
		}
		for _, tok := range tokens {
			if tok != "" && strings.Contains(text, tok) {
				t.Errorf("%s has a token", name)
			}
		}
	}
	t.Logf("checked %d logs and posted texts, and %d board reads", len(sources), len(lv.boards))
}

// Step 9: the project reaches its daily limit (4 agent runs: both
// implements and both round reviews, so Y's base-update review waits). No
// new agent stage starts and nothing new is picked; the stage already
// running finishes; after 00:00 in the profile's time zone the waiting
// item and the unpicked one continue on their own (FR-16 AC3, AC4).
func (lv *live) step9(t *testing.T) {
	loc := lv.prof.Location
	if loc == nil {
		loc = time.Local
	}
	start := time.Now()
	midnight := core.NextDayBoundary(start, loc)
	if until := midnight.Sub(start); until < 20*time.Minute || until > lv.timeout {
		t.Fatalf("00:00 in %s is %s away; step 9 needs it between 20m and the step timeout %s (docs/LIVE-SCENARIO.md)", loc, until.Round(time.Minute), lv.timeout)
	}
	x, y := lv.newItem(t, "X"), lv.newItem(t, "Y")
	for _, it := range []ports.ItemRef{x, y} {
		lv.on(it, contract.StageImplement, lv.impl())
		lv.on(it, contract.StageReview, lv.clean(), lv.clean())
	}
	lv.await(t, "X and Y claimed", func() bool {
		return lv.labelled(x, ports.MarkerInProgress) && lv.labelled(y, ports.MarkerInProgress)
	})
	z := lv.newItem(t, "Z")
	lv.on(z, contract.StageImplement, lv.impl())
	lv.on(z, contract.StageReview, lv.clean())
	project := func() *apiv1.ProjectBoard {
		b := lv.readBoard()
		if b == nil || len(b.Projects) != 1 {
			return nil
		}
		return b.Projects[0]
	}
	lv.await(t, "the daily limit reached", func() bool { p := project(); return p != nil && p.DailyLimitReached })
	waiting := func() int {
		n := 0
		for _, it := range []ports.ItemRef{x, y} {
			if lv.inState(it, core.StateWaitingLimit) {
				n++
			}
		}
		return n
	}
	lv.await(t, "one of X and Y waiting for the limit", func() bool { return waiting() == 1 || time.Now().After(midnight.Add(-5*time.Minute)) })
	if time.Now().After(midnight) {
		t.Fatal("the run reached 00:00 before the limit could be checked; start step 9 earlier")
	}
	if n := waiting(); n != 1 {
		t.Fatalf("%d items wait for the limit, want 1", n)
	}
	if p := project(); p == nil || p.DailyRuns != step9Limit {
		t.Fatalf("agent runs counted today: %v, want %d", p, step9Limit)
	}
	if lv.labelled(z, ports.MarkerInProgress) {
		t.Fatal("Z was picked at the daily limit")
	}
	lv.await(t, "X, Y and Z merged after 00:00", func() bool { return lv.merged(x) && lv.merged(y) && lv.merged(z) })

	var before, after []string
	lv.mu.Lock()
	for _, it := range []ports.ItemRef{x, y, z} {
		for _, in := range lv.ins[it] {
			run := string(it) + ":" + string(in.Stage)
			if lv.at[in.RunID].Before(midnight) {
				before = append(before, run)
			} else {
				after = append(after, run)
			}
		}
	}
	lv.mu.Unlock()
	if len(before) != step9Limit {
		t.Fatalf("agent runs before 00:00: %v, want %d", before, step9Limit)
	}
	if !slices.Contains(after, string(z)+":implement") {
		t.Fatalf("Z wasn't implemented after 00:00: %v", after)
	}
}
