package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// The test fixtures: the fake adapter as tracker and code host, a fake
// workspace and git backed by the fake's branches, a fake runner whose
// handlers are Go functions, and a fake secret store.

const sha0 = "0000000000000000000000000000000000000000"

// store is a SecretStore that issues a fresh, GitHub-shaped token per call
// and records what it issued and revoked.
type store struct {
	mu      sync.Mutex
	n       int
	issued  []ports.Token
	revoked map[string]bool
}

func (s *store) Token(_ context.Context, id ports.Identity, sc ports.Scope) (ports.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	secret := fmt.Sprintf("ghs_%s%0*d", id, 36-len(id), s.n)
	t := ports.NewToken(secret, id, sc, time.Now().Add(time.Hour))
	s.issued = append(s.issued, t)
	return t, nil
}

func (s *store) Revoke(_ context.Context, t ports.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked == nil {
		s.revoked = map[string]bool{}
	}
	s.revoked[t.Secret()] = true
	return nil
}

// live returns the tokens not revoked.
func (s *store) live() []ports.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.Token
	for _, t := range s.issued {
		if !s.revoked[t.Secret()] {
			out = append(out, t)
		}
	}
	return out
}

func (s *store) identities() map[ports.Identity]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[ports.Identity]int{}
	for _, t := range s.issued {
		m[t.Identity]++
	}
	return m
}

// handler is a fake handler: it may push through the harness and returns
// result.json's content. Files it names go in out.
type handler func(h *harness, in contract.Input, out string) string

// harness wires Activities to the fake adapter.
type harness struct {
	t     *testing.T
	f     *fake.Fake
	a     *Activities
	sec   *store
	mu    sync.Mutex
	heads map[string]ports.Commit // remote branch heads
	files map[string]int          // path -> lines, at every commit
	// parents are the parents of commits a handler merged (fakeGit.Parents).
	parents map[ports.Commit][]ports.Commit
	runs    []ports.RunSpec
	ins     []contract.Input
	next    handler
	ends    map[string]func()
	coder   []string // coder run IDs in order
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := fake.New(fake.DefaultConfig())
	h := &harness{t: t, f: f, sec: &store{}, heads: map[string]ports.Commit{}, files: map[string]int{}, parents: map[ports.Commit][]ports.Commit{}, ends: map[string]func(){}}
	base, err := f.Push(fake.Owner, "main")
	if err != nil {
		t.Fatal(err)
	}
	h.heads["main"] = base
	h.a = &Activities{
		Config: Config{
			Project: "p", TrackerAdapter: "github", CodeHostAdapter: "github",
			Repo:         ports.RepoRef{URL: "https://github.com/o/r", BaseBranch: "main"},
			RepoName:     "o/r",
			BranchPrefix: "agent/",
			Stages: map[contract.Stage]StageConfig{
				contract.StageImplement: {Command: []string{"agent-loop-claude", "implement"}, TimeLimit: time.Hour},
				contract.StageReview:    {Command: []string{"agent-loop-claude", "review"}, TimeLimit: time.Hour},
				contract.StageAddress:   {Command: []string{"agent-loop-claude", "address"}, TimeLimit: time.Hour},
			},
			Owners:         []ports.OwnerAccount{{Login: fake.Owner.Login, ID: fake.Owner.ID}},
			CoderSignature: workspace.Signature{Name: "coder", Email: "coder@example.com"},
			RunsDir:        t.TempDir(),
		},
		Tracker:    f.Tracker(),
		Host:       f.Host(),
		Workspace:  (*fakeWS)(h),
		Runner:     (*fakeRunner)(h),
		Secrets:    h.sec,
		Git:        (*fakeGit)(h),
		CoderRuns:  (*fakeRuns)(h),
		Now:        f.Now,
		RetryDelay: time.Millisecond,
	}
	return h
}

// push pushes one commit to branch as the coder, during the coder run.
func (h *harness) push(branch string) ports.Commit {
	h.t.Helper()
	c, err := h.f.Push(fake.Coder, branch)
	if err != nil {
		h.t.Fatalf("push %s: %v", branch, err)
	}
	h.mu.Lock()
	h.heads[branch] = c
	h.mu.Unlock()
	return c
}

func (h *harness) head(branch string) ports.Commit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.heads[branch]
}

// item creates a ready item.
func (h *harness) item() ports.ItemRef {
	h.t.Helper()
	ref, err := h.f.NewItem(fake.Owner, "title", "body", ports.MarkerReady, ports.MarkerTypeFeature)
	if err != nil {
		h.t.Fatal(err)
	}
	return ref
}

var runN int

func (h *harness) req(item ports.ItemRef, rs core.RunStage) StageRequest {
	if rs.RunID == "" {
		runN++
		rs.RunID = fmt.Sprintf("item/p/%s#1:%s:1:%d", item, rs.Stage, runN)
	}
	if rs.Attempt == 0 {
		rs.Attempt = 1
	}
	return StageRequest{WorkflowID: "item/p/" + string(item), Item: string(item), ItemURL: "fake://issues/" + string(item), ItemType: "feature", Claim: 1, Run: rs}
}

func (h *harness) run(req StageRequest, hd handler) StageReport {
	h.t.Helper()
	h.next = hd
	rep, err := h.a.RunStage(context.Background(), req)
	if err != nil {
		h.t.Fatalf("RunStage: %v", err)
	}
	return rep
}

// implemented opens the item's change through an implement run and
// returns the change.
func (h *harness) implemented(item ports.ItemRef) ports.Change {
	h.t.Helper()
	rep := h.run(h.req(item, core.RunStage{Stage: contract.StageImplement}), implementer("agent/"+string(item)))
	if rep.Result == nil || rep.Result.Outcome != contract.OutcomeDone {
		h.t.Fatalf("implement: %+v", rep)
	}
	ch, err := h.f.Host().Change(context.Background(), ports.ChangeRef(rep.Result.Implement.Change))
	if err != nil {
		h.t.Fatal(err)
	}
	return ch
}

func write(t *testing.T, out, name, s string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(out, name), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func resultJSON(in contract.Input, outcome string, obj any) string {
	m := map[string]any{"contract_version": "1.0", "run_id": in.RunID, "result": outcome, outcome: obj}
	b, _ := json.Marshal(m)
	return string(b)
}

// implementer pushes one commit and returns done.
func implementer(branch string) handler {
	return func(h *harness, in contract.Input, out string) string {
		head := h.push(branch)
		write(h.t, out, "change.md", "The change.")
		return resultJSON(in, "done", map[string]any{"head": head, "change": map[string]any{"title": "Add it", "body_file": "change.md"}})
	}
}

type fakeWS harness

func (w *fakeWS) Prepare(_ context.Context, repo ports.RepoRef, b ports.Branch) (ports.Dir, error) {
	h := (*harness)(w)
	d := ports.Dir{Path: h.t.TempDir()}
	switch {
	case b.Detach != "":
		d.Head = b.Detach
	default:
		d.Branch = b.Name
		d.Head = h.head(b.Name)
		if d.Head == "" {
			d.Head = h.head(repo.BaseBranch)
		}
	}
	return d, nil
}

func (w *fakeWS) Release(context.Context, ports.Dir) error { return nil }

type fakeRunner harness

func (r *fakeRunner) Run(_ context.Context, s ports.RunSpec) (ports.RunOutcome, error) {
	h := (*harness)(r)
	data, err := os.ReadFile(s.InputPath)
	if err != nil {
		return ports.RunOutcome{}, err
	}
	in, err := contract.ParseInput(data)
	if err != nil {
		return ports.RunOutcome{}, err
	}
	h.mu.Lock()
	h.runs = append(h.runs, s)
	h.ins = append(h.ins, in)
	hd := h.next
	h.mu.Unlock()
	os.MkdirAll(s.OutDir, 0o700)
	start := time.Now()
	if hd == nil {
		h.t.Fatalf("a handler ran unexpectedly: %s", s.RunID)
	}
	if res := hd(h, in, s.OutDir); res != "" {
		if err := os.WriteFile(s.ResultPath, []byte(res), 0o600); err != nil {
			return ports.RunOutcome{}, err
		}
	}
	return ports.RunOutcome{ResultPath: s.ResultPath, Started: start, Ended: time.Now()}, nil
}

type fakeGit harness

func (g *fakeGit) RemoteHead(_ context.Context, _ ports.RepoRef, branch string) (ports.Commit, error) {
	if c := (*harness)(g).head(branch); c != "" {
		return c, nil
	}
	return "", fmt.Errorf("branch %s: %w", branch, ports.ErrNotFound)
}

func (g *fakeGit) FetchedHead(ctx context.Context, _ ports.Dir, branch string) (ports.Commit, error) {
	return g.RemoteHead(ctx, ports.RepoRef{}, branch)
}

func (g *fakeGit) MergeTree(context.Context, ports.Dir, ports.Commit, ports.Commit) (string, []string, error) {
	return "", nil, fmt.Errorf("not in the fake")
}

func (g *fakeGit) CommitMerge(context.Context, ports.Dir, string, []ports.Commit, string, workspace.Signature) (ports.Commit, error) {
	return "", fmt.Errorf("not in the fake")
}

func (g *fakeGit) Push(context.Context, ports.Dir, ports.RepoRef, ports.Commit, string, ports.Token) error {
	return fmt.Errorf("not in the fake")
}

// Parents: a commit the fake merged the base into has the old head and the
// base tip (see harness.mergeBase); others have none known.
func (g *fakeGit) Parents(_ context.Context, _ ports.Dir, c ports.Commit) ([]ports.Commit, error) {
	h := (*harness)(g)
	h.mu.Lock()
	defer h.mu.Unlock()
	if ps, ok := h.parents[c]; ok {
		return ps, nil
	}
	return []ports.Commit{sha0}, nil
}

func (g *fakeGit) IsAncestor(context.Context, ports.Dir, ports.Commit, ports.Commit) (bool, error) {
	return true, nil
}

func (g *fakeGit) FileLines(_ context.Context, _ ports.Dir, _ ports.Commit, path string) (int, error) {
	h := (*harness)(g)
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.files[path]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("%s: %w", path, ports.ErrNotFound)
}

func (g *fakeGit) ResolvedPaths(context.Context, ports.Dir, ports.Commit) ([]string, error) {
	return nil, nil
}

type fakeRuns harness

func (r *fakeRuns) Started(_ context.Context, item ports.ItemRef, runID string, _ time.Time) error {
	h := (*harness)(r)
	end, err := h.f.StartCoderRun(item)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.ends[runID] = end
	h.coder = append(h.coder, runID)
	h.mu.Unlock()
	return nil
}

func (r *fakeRuns) Ended(_ context.Context, _ ports.ItemRef, runID string, _ time.Time, _ ports.Commit) error {
	h := (*harness)(r)
	h.mu.Lock()
	end := h.ends[runID]
	delete(h.ends, runID)
	h.mu.Unlock()
	if end != nil {
		end()
	}
	return nil
}

// reviews returns the change's review records, decoded.
func (h *harness) reviews(ch ports.ChangeRef) []contract.Record {
	h.t.Helper()
	rrs, err := h.f.Host().ReviewRecords(context.Background(), ch)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []contract.Record
	for _, rr := range rrs {
		var rec contract.Record
		if err := json.Unmarshal(rr.Record.Data, &rec); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func ids(fs []core.Finding) string {
	var s []string
	for _, f := range fs {
		s = append(s, fmt.Sprintf("%s:%d:%s", f.ID, f.Score, f.Status))
	}
	return strings.Join(s, " ")
}
