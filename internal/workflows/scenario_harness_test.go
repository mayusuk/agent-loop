package workflows

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// The PRODUCT-SPEC §11.1 scenario harness (work plan #25): the workflows
// in Temporal's test environment, the fake adapter as tracker and code
// host, git and workspaces backed by the fake's branches, handlers scripted
// per item and stage, the real SQLite projection for the board, and every
// log and posted text captured for the credential check (step 8).

// syncBuf is a log sink safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// scenGit is GitOps over the fake's commit graph. A merge without
// conflicts is pushed by CommitMerge itself (as the coder, during the
// worker's coder run), so Push only checks the branch is at it.
type scenGit struct{ f *fake.Fake }

func (g scenGit) RemoteHead(_ context.Context, _ ports.RepoRef, branch string) (ports.Commit, error) {
	return g.f.BranchHead(branch)
}

func (g scenGit) FetchedHead(_ context.Context, _ ports.Dir, branch string) (ports.Commit, error) {
	return g.f.BranchHead(branch)
}

func (g scenGit) MergeTree(context.Context, ports.Dir, ports.Commit, ports.Commit) (string, []string, error) {
	return "tree", nil, nil
}

func (g scenGit) CommitMerge(_ context.Context, d ports.Dir, _ string, parents []ports.Commit, _ string, _ workspace.Signature) (ports.Commit, error) {
	if head, err := g.f.BranchHead(d.Branch); err != nil || head != parents[0] {
		return "", fmt.Errorf("scenario git: %s moved: %w", d.Branch, ports.ErrConflict)
	}
	return g.f.MergeBaseInto(fake.Coder, d.Branch)
}

func (g scenGit) Push(_ context.Context, _ ports.Dir, _ ports.RepoRef, c ports.Commit, branch string, _ ports.Token) error {
	if head, err := g.f.BranchHead(branch); err != nil || head != c {
		return fmt.Errorf("scenario git: push %s: %w", branch, ports.ErrConflict)
	}
	return nil
}

func (g scenGit) Parents(_ context.Context, _ ports.Dir, c ports.Commit) ([]ports.Commit, error) {
	return g.f.Parents(c)
}

func (g scenGit) IsAncestor(_ context.Context, _ ports.Dir, a, b ports.Commit) (bool, error) {
	return g.f.IsAncestor(a, b), nil
}

func (g scenGit) FileLines(context.Context, ports.Dir, ports.Commit, string) (int, error) {
	return 100, nil
}

func (g scenGit) ResolvedPaths(context.Context, ports.Dir, ports.Commit) ([]string, error) {
	return nil, nil
}

// scenWS prepares directories at the fake's branch heads.
type scenWS struct {
	t *testing.T
	f *fake.Fake
}

func (w scenWS) Prepare(_ context.Context, repo ports.RepoRef, b ports.Branch) (ports.Dir, error) {
	d := ports.Dir{Path: w.t.TempDir()}
	if b.Detach != "" {
		d.Head = b.Detach
		return d, nil
	}
	d.Branch = b.Name
	head, err := w.f.BranchHead(b.Name)
	if err != nil {
		head, err = w.f.BranchHead(repo.BaseBranch)
	}
	d.Head = head
	return d, err
}

func (scenWS) Release(context.Context, ports.Dir) error { return nil }

// posted records every text the worker posts.
type posted struct {
	mu    sync.Mutex
	texts []string
	items []ports.NewItem
	refs  []ports.ItemRef
}

func (p *posted) add(ts ...ports.UntrustedText) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range ts {
		p.texts = append(p.texts, untrusted.Read(t))
	}
}

func (p *posted) all() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.texts, "\n")
}

type tapTracker struct {
	ports.WorkTracker
	p *posted
}

func (x tapTracker) Comment(ctx context.Context, ref ports.ItemRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) (ports.CommentRef, error) {
	x.p.add(body)
	return x.WorkTracker.Comment(ctx, ref, rec, body, key)
}

func (x tapTracker) CreateItem(ctx context.Context, n ports.NewItem, key ports.IdemKey) (ports.ItemRef, error) {
	x.p.add(n.Title, n.Body)
	ref, err := x.WorkTracker.CreateItem(ctx, n, key)
	if err == nil {
		x.p.mu.Lock()
		if !slices.Contains(x.p.refs, ref) {
			x.p.items, x.p.refs = append(x.p.items, n), append(x.p.refs, ref)
		}
		x.p.mu.Unlock()
	}
	return ref, err
}

type tapHost struct {
	ports.CodeHost
	p *posted
}

func (x tapHost) OpenChange(ctx context.Context, n ports.NewChange, as ports.Identity, key ports.IdemKey) (ports.ChangeRef, error) {
	x.p.add(n.Title, n.Body)
	return x.CodeHost.OpenChange(ctx, n, as, key)
}

func (x tapHost) PostReview(ctx context.Context, ref ports.ChangeRef, rp ports.ReviewPost, key ports.IdemKey) (ports.ReviewRef, error) {
	x.p.add(rp.Summary)
	for _, f := range rp.Findings {
		x.p.add(f.Body)
	}
	return x.CodeHost.PostReview(ctx, ref, rp, key)
}

func (x tapHost) PostReply(ctx context.Context, ref ports.ChangeRef, to ports.ThreadRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) error {
	x.p.add(body)
	return x.CodeHost.PostReply(ctx, ref, to, rec, body, key)
}

// teeProjection writes to the recorder and to the real projection.
type teeProjection struct {
	rec *recorder
	db  *projection.DB
}

func (p teeProjection) RecordDecision(ctx context.Context, d projection.Decision) error {
	p.rec.RecordDecision(ctx, d)
	return p.db.RecordDecision(ctx, d)
}

func (p teeProjection) StartRun(ctx context.Context, s projection.StageRun) error {
	p.rec.StartRun(ctx, s)
	return p.db.StartRun(ctx, s)
}

func (p teeProjection) SetRunHost(ctx context.Context, id, host string) error {
	p.rec.SetRunHost(ctx, id, host)
	return p.db.SetRunHost(ctx, id, host)
}

func (p teeProjection) FinishRun(ctx context.Context, id string, ended time.Time, result, log string) error {
	p.rec.FinishRun(ctx, id, ended, result, log)
	return p.db.FinishRun(ctx, id, ended, result, log)
}

func (p teeProjection) RecordRelease(ctx context.Context, r projection.Release) error {
	p.rec.RecordRelease(ctx, r)
	return p.db.RecordRelease(ctx, r)
}

func (p teeProjection) DailyCount(ctx context.Context, project string, day time.Time, loc *time.Location) (int, error) {
	return p.db.DailyCount(ctx, project, day, loc)
}

// scen is one §11.1 run: a project with WIP 2 and the items a step names.
type scen struct {
	*wfHarness
	logs   *syncBuf
	dbDir  string
	db     *projection.DB
	posted *posted

	smu     sync.Mutex
	scripts map[ports.ItemRef]map[contract.Stage][]handler
	held    map[ports.ItemRef]bool // items whose gate stays pending
	// runHook, when set, wraps every RunStage activity (the worker kill of
	// step 4).
	runHook func(ctx context.Context, req StageRequest) (StageReport, error)
}

func newScenario(t *testing.T) *scen {
	t.Helper()
	logs := &syncBuf{}
	w := newWFLogging(t, logs)
	s := &scen{wfHarness: w, logs: logs, posted: &posted{}, scripts: map[ports.ItemRef]map[contract.Stage][]handler{}, held: map[ports.ItemRef]bool{}}
	s.dbDir = t.TempDir()
	db, err := projection.Open(context.Background(), filepath.Join(s.dbDir, "agent-loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s.db = db
	a := w.a
	a.Projection = teeProjection{rec: w.rec, db: db}
	a.Tracker = tapTracker{WorkTracker: w.f.Tracker(), p: s.posted}
	a.Host = tapHost{CodeHost: w.f.Host(), p: s.posted}
	a.Git = scenGit{f: w.f}
	a.Workspace = scenWS{t: t, f: w.f}
	a.FollowUpType = ports.MarkerTypeFeature
	w.cfg.Pick.WIP = 2
	w.cfg.Item.Project = "p"
	w.cfg.SupervisorInterval = 3 * time.Hour
	w.env.SetTestTimeout(2 * time.Minute)
	t.Cleanup(func() {
		if t.Failed() {
			dumpDecisions(t, w.rec)
		}
	})
	for _, st := range []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress} {
		w.handle(st, s.dispatch)
	}
	// Gates pass on every head at the merge check, unless the item is
	// held.
	w.env.OnActivity("CheckMerge", mock.Anything, mock.Anything).Return(func(ctx context.Context, req MergeCheckRequest) (core.MergeChecked, error) {
		if ch, err := w.f.Host().Change(ctx, ports.ChangeRef(req.Action.Change)); err == nil && !s.isHeld(ch.Item) {
			w.f.SetGate(ch.Head, "ci", ports.GatePass)
		}
		return a.CheckMerge(ctx, req)
	})
	w.env.OnActivity("RunStage", mock.Anything, mock.Anything).Return(func(ctx context.Context, req StageRequest) (StageReport, error) {
		if s.runHook != nil {
			return s.runHook(ctx, req)
		}
		return a.RunStage(ctx, req)
	})
	return s
}

func (s *scen) isHeld(item ports.ItemRef) bool {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.held[item]
}

// hold keeps the item's gate pending until release.
func (s *scen) hold(item ports.ItemRef) {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.held[item] = true
}

func (s *scen) release(item ports.ItemRef) {
	s.smu.Lock()
	defer s.smu.Unlock()
	delete(s.held, item)
}

// newItem creates a ready feature as the owner.
func (s *scen) newItem(title string) ports.ItemRef {
	s.t.Helper()
	ref, err := s.f.NewItem(fake.Owner, title, "body of "+title, ports.MarkerReady, ports.MarkerTypeFeature)
	if err != nil {
		s.t.Fatal(err)
	}
	return ref
}

// on queues handlers for the item's runs of a stage, in order.
func (s *scen) on(item ports.ItemRef, st contract.Stage, hds ...handler) {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.scripts[item] == nil {
		s.scripts[item] = map[contract.Stage][]handler{}
	}
	s.scripts[item][st] = append(s.scripts[item][st], hds...)
}

func (s *scen) dispatch(h *harness, in contract.Input, out string) string {
	item := ports.ItemRef(in.Item.Ref)
	s.smu.Lock()
	q := s.scripts[item][in.Stage]
	var hd handler
	if len(q) > 0 {
		hd, s.scripts[item][in.Stage] = q[0], q[1:]
	}
	s.smu.Unlock()
	if hd == nil {
		s.t.Errorf("item %s: unscripted %s run (%s)", item, in.Stage, in.RunID)
		return ""
	}
	return hd(h, in, out)
}

func branchOf(item ports.ItemRef) string { return "agent/" + string(item) }

// impl pushes one commit as the coder and opens the change. Its text
// carries the run's push token, which the worker must mask.
func (s *scen) impl() handler {
	return func(h *harness, in contract.Input, out string) string {
		head := h.push(branchOf(ports.ItemRef(in.Item.Ref)))
		write(h.t, out, "change.md", "The change. "+s.leak(h))
		return resultJSON(in, "done", map[string]any{"head": head, "change": map[string]any{"title": "Add it", "body_file": "change.md"}})
	}
}

// leak returns the current run's tokens, as a careless handler would
// echo them (PRODUCT-SPEC §8 item 7).
func (s *scen) leak(h *harness) string {
	h.mu.Lock()
	spec := h.runs[len(h.runs)-1]
	h.mu.Unlock()
	var out []string
	for _, src := range []ports.TokenSource{spec.Read, spec.Push} {
		if src == nil {
			continue
		}
		if tok, err := src.Token(context.Background()); err == nil {
			out = append(out, "token="+tok.Secret())
		}
	}
	return strings.Join(out, " ")
}

// review is a scripted review: rulings for findings (open when not
// named), a ruling per manual comment by its position in rule_manual
// (addressed when not named), and new findings. before runs first.
type review struct {
	findings map[string]contract.FindingStatus
	manual   map[int]contract.ManualRulingValue
	add      []newF
	before   func(in contract.Input)
}

func (s *scen) rev(r review) handler {
	return func(h *harness, in contract.Input, out string) string {
		if r.before != nil {
			r.before(in)
		}
		write(h.t, out, "s.md", "Summary. "+s.leak(h))
		rs := []map[string]any{}
		for _, id := range in.Review.RuleFindings {
			st := r.findings[id]
			if st == "" {
				st = contract.StatusOpen
			}
			rs = append(rs, map[string]any{"finding": id, "status": st})
		}
		ms := []map[string]any{}
		for i, id := range in.Review.RuleManual {
			v := r.manual[i]
			if v == "" {
				v = contract.RulingAddressed
			}
			ms = append(ms, map[string]any{"comment": id, "ruling": v})
		}
		fs := []map[string]any{}
		for i, f := range r.add {
			body := write(h.t, out, fmt.Sprintf("n%d.md", i), "finding body: secret detail")
			fs = append(fs, map[string]any{"score": f.score, "path": "a.go", "line": 1, "title": "T", "body_file": body})
		}
		return resultJSON(in, "done", map[string]any{"commit": in.Review.Commit, "kind": in.Review.Kind, "summary_file": "s.md",
			"rulings": rs, "manual_rulings": ms, "new_findings": fs})
	}
}

// clean is a review that rules everything addressed and adds nothing.
func (s *scen) clean() handler {
	return func(h *harness, in contract.Input, out string) string {
		all := map[string]contract.FindingStatus{}
		for _, id := range in.Review.RuleFindings {
			all[id] = contract.StatusAddressed
		}
		return s.rev(review{findings: all})(h, in, out)
	}
}

// address is a scripted address run: push moves the head; finding and
// manual give each reply's kind (explained when nil); during runs with
// the coder run live, before returning.
type address struct {
	push    bool
	finding func(id string) contract.ReplyKind
	manual  func(i int, id string) contract.ReplyKind
	during  func(in contract.Input)
}

func (s *scen) addr(a address) handler {
	return func(h *harness, in contract.Input, out string) string {
		head := in.Workspace.Head
		if a.push {
			head = string(h.push(branchOf(ports.ItemRef(in.Item.Ref))))
		}
		rs, ms := []map[string]any{}, []map[string]any{}
		for _, id := range in.Address.AnswerFindings {
			k := contract.ReplyExplained
			if a.finding != nil {
				k = a.finding(id)
			}
			rs = append(rs, map[string]any{"finding": id, "kind": k, "body_file": write(h.t, out, "r"+id+".md", "reply "+id+" "+s.leak(h))})
		}
		for i, id := range in.Address.AnswerManual {
			k := contract.ReplyExplained
			if a.manual != nil {
				k = a.manual(i, id)
			}
			ms = append(ms, map[string]any{"comment": id, "kind": k, "body_file": write(h.t, out, fmt.Sprintf("m%d.md", i), "manual reply")})
		}
		if a.during != nil {
			a.during(in)
		}
		return resultJSON(in, "done", map[string]any{"head": head, "replies": rs, "manual_replies": ms})
	}
}

// at runs f at d after the start, in workflow time.
func (s *scen) at(d time.Duration, f func()) { s.env.RegisterDelayedCallback(f, d) }

func (s *scen) change(item ports.ItemRef) ports.Change {
	s.t.Helper()
	ch, err := s.f.Host().ChangeForItem(context.Background(), item)
	if err != nil || ch == nil {
		s.t.Fatalf("item %s has no open change: %v", item, err)
	}
	return *ch
}

func (s *scen) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

// itemStates returns the item's states, in order, without repeats.
func (s *scen) itemStates(item ports.ItemRef) []core.State {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	var out []core.State
	for _, d := range s.rec.decisions {
		if d.Item.Item != string(item) {
			continue
		}
		if n := len(out); n == 0 || out[n-1] != d.Item.State {
			out = append(out, d.Item.State)
		}
	}
	return out
}

// lastRow is the item's row after its last decision.
func (s *scen) lastRow(item ports.ItemRef) projection.ItemRow {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	var r projection.ItemRow
	for _, d := range s.rec.decisions {
		if d.Item.Item == string(item) {
			r = d.Item
		}
	}
	return r
}

// stagesOf returns the stages run for the item, in order.
func (s *scen) stagesOf(item ports.ItemRef) []contract.Stage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []contract.Stage
	for _, in := range s.ins {
		if in.Item.Ref == string(item) {
			out = append(out, in.Stage)
		}
	}
	return out
}

// reviewKinds lists the change's review records as kind/round.
func (s *scen) reviewKinds(ch ports.ChangeRef) []string {
	var out []string
	for _, r := range s.reviews(ch) {
		k := string(r.Kind)
		if r.Round != nil {
			k += fmt.Sprintf("/%d", *r.Round)
		}
		out = append(out, k)
	}
	return out
}

// tokenShape matches GitHub token shapes, whatever issued them.
var tokenShape = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`)

// noCredentials checks every log the run wrote, the projection database
// and every text the worker posted for any token the run issued or
// anything shaped like one (step 8, PRODUCT-SPEC §8 item 7).
func (s *scen) noCredentials() {
	s.t.Helper()
	sources := map[string]string{"workflow and activity log": s.logs.String(), "posted text": s.posted.all()}
	for _, root := range []string{s.a.RunsDir, s.dbDir} {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() && fi.Name() == "out" {
				return filepath.SkipDir // the handler's own files (work plan #25 D5)
			}
			if err == nil && !fi.IsDir() {
				if b, err := os.ReadFile(p); err == nil {
					sources[p] = string(b)
				}
			}
			return nil
		})
	}
	issued := s.sec.live()
	s.sec.mu.Lock()
	all := append([]ports.Token(nil), s.sec.issued...)
	s.sec.mu.Unlock()
	if len(all) == 0 {
		s.t.Fatal("the scenario issued no token, so the check proves nothing")
	}
	for name, text := range sources {
		for _, tok := range all {
			if strings.Contains(text, tok.Secret()) {
				s.t.Errorf("a %s token appears in %s", tok.Identity, name)
			}
		}
		if m := tokenShape.FindString(text); m != "" {
			s.t.Errorf("a token-shaped string appears in %s", name)
		}
	}
	if len(issued) != 0 {
		s.t.Errorf("%d tokens are still live after the run", len(issued))
	}
}

// board reads the board rows of the project.
func (s *scen) board() []projection.BoardRow {
	s.t.Helper()
	rows, err := s.db.Board(context.Background(), "p", s.env.Now())
	if err != nil {
		s.t.Fatal(err)
	}
	return rows
}

// project queries the project workflow's state.
func (s *scen) project() ProjectView {
	s.t.Helper()
	v, err := s.env.QueryWorkflow(QueryState)
	if err != nil {
		s.t.Fatal(err)
	}
	var pv ProjectView
	s.must(v.Get(&pv))
	return pv
}

// holdsSlot reports whether the project counts the item against WIP.
func (s *scen) holdsSlot(item ports.ItemRef) bool {
	_, ok := s.project().State.Items[string(item)]
	return ok
}
