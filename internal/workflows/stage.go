package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/runners"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// StageRequest is one RunStage call: the core's RunStage action and the
// item it is for.
type StageRequest struct {
	WorkflowID string
	Item       string
	ItemURL    string
	ItemType   string // "feature" or "bug"
	Claim      int
	Run        core.RunStage
}

// StageReport is how a stage run ended, as the event for core.Decide:
// either a result (FR-18) or a stop at the time limit (FR-18 AC4).
type StageReport struct {
	Result   *core.StageResult   `json:",omitempty"`
	TimedOut *core.StageTimedOut `json:",omitempty"`
	// DetailFile is the redacted text of a needs_owner question or a
	// blocked reason, kept on the worker for the status comment that
	// escalates or blocks the item. It never enters workflow history.
	DetailFile string `json:",omitempty"`
	// LogPath is the run's redacted log (stage_runs).
	LogPath string `json:",omitempty"`
	// Cost is the valid result's usage.cost, which the project adds to the
	// day's spend (FR-16 AC3, INTEGRATION-SPEC §5.1). Nil when the result
	// reports none or is invalid (work plan #21 D2).
	Cost *float64 `json:",omitempty"`
}

// Event returns the report's event.
func (r StageReport) Event() core.Event {
	if r.TimedOut != nil {
		return *r.TimedOut
	}
	if r.Result != nil {
		return *r.Result
	}
	return nil
}

// run is the state of one RunStage call.
type run struct {
	a      *Activities
	req    StageRequest
	stage  contract.Stage
	branch string
	dir    ports.Dir
	in     contract.Input
	runDir string
	outDir string
	change *ports.Change
	// limit is the run's time limit, and deadline when it ends: the
	// admitted start plus the stage limit, cut to the run's deadline. The
	// runner enforces limit, and input.json carries deadline (INTEGRATION-SPEC §4).
	limit    time.Duration
	deadline time.Time
}

// key is a record's idempotency key: <workflow id>#<claim>:<parts...>
// (TECHNICAL-SPEC §9). It holds no run ID, so a crash rerun, which gets a new
// run ID (INTEGRATION-SPEC §4.1), finds what the interrupted run posted
// (NFR-2 AC1, work plan D3).
func (r *run) key(parts ...string) ports.IdemKey {
	return ports.IdemKey(fmt.Sprintf("%s#%d:%s", r.req.WorkflowID, r.req.Claim, strings.Join(parts, ":")))
}

// itemBranch is the implement branch: <prefix><item> for n <= 1, otherwise
// <prefix><item>-<n> after n-1 changes closed without merging
// (INTEGRATION-SPEC §3.3, core.RunStage.Branch).
func itemBranch(prefix, item string, n int) string {
	if n <= 1 {
		return prefix + item
	}
	return prefix + item + "-" + strconv.Itoa(n)
}

func (r *run) result(o contract.Outcome) *core.StageResult {
	return &core.StageResult{At: core.At{Time: r.a.now()}, RunID: r.req.Run.RunID, Stage: r.stage, Outcome: o}
}

func (r *run) failed(reason string) StageReport {
	res := r.result(contract.OutcomeFailed)
	res.Reason = oneLine(reason)
	return StageReport{Result: res}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); len(rs) > contract.MaxFailedReason {
		s = string(rs[:contract.MaxFailedReason-1]) + "…"
	}
	return s
}

// RunStage runs one implement, review or address handler (INTEGRATION-SPEC
// §3–§5): it prepares a fresh workspace, issues the stage identity's
// tokens, writes input.json with references only (NFR-8 AC1), runs the
// handler, revokes the tokens, validates the result and posts its records
// as the stage's identity (I1, §5.2). An implement on an item with an open
// change adopts it and runs no handler (FR-3 AC3). An error means the run
// could not be made or finished (the workflow treats it as a crash); every
// handler outcome, including an invalid result, is a StageReport.
func (a *Activities) RunStage(ctx context.Context, req StageRequest) (StageReport, error) {
	rep, err := a.runStage(ctx, req)
	if err != nil || rep.Result == nil || req.Run.Stage != contract.StageReview || req.Run.Change == "" {
		return rep, err
	}
	// The manual comments as the review leaves them (work plan #25 D3).
	// The review is posted by now, so a read that still fails after the
	// retries leaves Manual unset rather than making the run a crash: the
	// next poll and the merge check read the comments again (review R3).
	var mcs []core.Event
	if err := a.retry(ctx, func() (err error) {
		mcs, err = a.manualEvents(ctx, ports.ChangeRef(req.Run.Change), core.At{Time: a.now()})
		return err
	}); err != nil {
		return rep, nil
	}
	for _, e := range mcs {
		rep.Result.Manual = append(rep.Result.Manual, e.(core.ManualCommentChanged))
	}
	return rep, nil
}

func (a *Activities) runStage(ctx context.Context, req StageRequest) (StageReport, error) {
	r := &run{a: a, req: req, stage: req.Run.Stage, branch: itemBranch(a.BranchPrefix, req.Item, req.Run.Branch)}
	sc, ok := a.Stages[r.stage]
	switch {
	case req.WorkflowID == "" || req.Claim < 1:
		return StageReport{}, errors.New("workflows: a stage run needs the workflow ID and the claim for its record keys")
	case r.stage != contract.StageImplement && r.stage != contract.StageReview && r.stage != contract.StageAddress:
		return StageReport{}, fmt.Errorf("workflows: stage %q has no v0.1 handler", r.stage)
	case !ok || len(sc.Command) == 0:
		return StageReport{}, fmt.Errorf("workflows: no command for stage %s", r.stage)
	case r.stage == contract.StageReview && req.Run.Review == nil:
		return StageReport{}, errors.New("workflows: review run without a review spec")
	case r.stage == contract.StageAddress && req.Run.Address == nil:
		return StageReport{}, errors.New("workflows: address run without an address spec")
	case r.stage != contract.StageImplement && req.Run.Change == "":
		return StageReport{}, fmt.Errorf("workflows: %s run without a change", r.stage)
	}

	// FR-3 AC3: an open change is adopted; implement doesn't run.
	if r.stage == contract.StageImplement {
		if rep, ok, err := r.adopt(ctx); err != nil || ok {
			return rep, err
		}
	}
	if req.Run.Change != "" {
		ch, err := a.Host.Change(ctx, ports.ChangeRef(req.Run.Change))
		if err != nil {
			return StageReport{}, err
		}
		r.change = &ch
		if ch.Branch != "" {
			r.branch = ch.Branch
		}
	}
	// NFR-2: a review this run already posted is not run again.
	if r.stage == contract.StageReview {
		if rep, ok, err := r.posted(ctx); err != nil || ok {
			return rep, err
		}
	}

	var err error
	if r.runDir, err = a.runDir(req.Run.RunID); err != nil {
		return StageReport{}, err
	}
	r.outDir = filepath.Join(r.runDir, "out")
	logPath := filepath.Join(r.runDir, "run.log")

	// The stage's identity only (INTEGRATION-SPEC §3.4, TECHNICAL-SPEC §7.3):
	// the loop identity is never issued here (PRODUCT-SPEC §8 item 8).
	id := ports.IdentityCoder
	if r.stage == contract.StageReview {
		id = ports.IdentityReviewer
	}
	coder := id == ports.IdentityCoder
	if coder && a.CoderRuns != nil {
		if err := a.CoderRuns.Started(ctx, ports.ItemRef(req.Item), req.Run.RunID, a.now()); err != nil {
			return StageReport{}, fmt.Errorf("workflows: coder run: %w", err)
		}
	}
	tokens := secrets.NewRun(a.Secrets)
	revoked := false
	revoke := func() error {
		if revoked {
			return nil
		}
		revoked = true
		return tokens.Close(context.WithoutCancel(ctx))
	}
	// The coder run ends after its tokens are revoked, so no coder token is
	// live outside a recorded run (I11).
	ended := !coder || a.CoderRuns == nil
	endRun := func() error {
		if ended {
			return nil
		}
		ended = true
		bg := context.WithoutCancel(ctx)
		head, _ := a.Git.RemoteHead(bg, a.Repo, r.branch)
		return a.CoderRuns.Ended(bg, ports.ItemRef(req.Item), req.Run.RunID, a.now(), head)
	}
	defer func() {
		revoke()
		endRun()
	}()
	readLease, err := tokens.Issue(ctx, id, a.handlerReadScope())
	if err != nil {
		return StageReport{}, fmt.Errorf("workflows: read token: %w", err)
	}
	read := a.redacting(readLease)
	var push ports.TokenSource // none for review (FR-13 AC2)
	if coder {
		l, err := tokens.Issue(ctx, id, a.handlerPushScope())
		if err != nil {
			return StageReport{}, fmt.Errorf("workflows: push token: %w", err)
		}
		push = a.redacting(l)
	}

	b := ports.Branch{Name: r.branch}
	if r.stage == contract.StageReview {
		b = ports.Branch{Detach: ports.Commit(req.Run.Review.Commit)}
	}
	if r.dir, err = a.Workspace.Prepare(ctx, a.Repo, b); err != nil {
		return StageReport{}, fmt.Errorf("workflows: workspace: %w", err)
	}
	defer a.Workspace.Release(context.WithoutCancel(ctx), r.dir)

	r.limit = r.timeLimit(sc.TimeLimit)
	r.deadline = a.now().Add(r.limit)
	if err := r.writeInput(ctx); err != nil {
		return StageReport{}, err
	}
	inputPath := filepath.Join(r.runDir, "input.json")
	spec := ports.RunSpec{
		RunID:      req.Run.RunID,
		Command:    sc.Command,
		Dir:        r.dir,
		RepoURL:    a.Repo.URL,
		InputPath:  inputPath,
		ResultPath: filepath.Join(r.runDir, "result.json"),
		OutDir:     r.outDir,
		LogPath:    logPath,
		TimeLimit:  r.limit,
		Runner:     sc.Runner,
		Egress:     a.Egress,
		Push:       push,
		Read:       read,
	}

	stopBeat := a.heartbeat(ctx)
	out, runErr := a.Runner.Run(ctx, spec)
	stopBeat()
	// Revoke at once, so nothing the handler left behind can push while the
	// worker posts (TECHNICAL-SPEC §7.3, INTEGRATION-SPEC §9.8 step 6).
	revErr := revoke()
	if err := endRun(); err != nil {
		return StageReport{}, fmt.Errorf("workflows: coder run: %w", err)
	}
	if revErr != nil {
		return StageReport{}, fmt.Errorf("workflows: revoke the run's tokens: %w", revErr)
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return StageReport{}, runErr
		}
		if out.Started.IsZero() {
			// The handler never started, for example an unsandboxed
			// handler on the local runner: a failed run (FR-18 AC3).
			return r.failed("the handler did not start: " + runErr.Error()), nil
		}
		return StageReport{}, runErr
	}
	if out.TimedOut {
		return StageReport{TimedOut: &core.StageTimedOut{At: core.At{Time: a.now()}, RunID: req.Run.RunID}, LogPath: logPath}, nil
	}
	res, verr := runners.ReadResult(r.in, out, r.outDir)
	var rep StageReport
	if verr != nil {
		rep = r.failed(res.Failed.Reason)
	} else {
		rep, err = r.post(ctx, res)
		if err != nil {
			return StageReport{}, err
		}
		if res.Usage != nil && res.Usage.Cost != nil {
			c := *res.Usage.Cost
			rep.Cost = &c
		}
	}
	rep.LogPath = logPath
	return rep, nil
}

// adopt returns a done implement for the item's open change, if it has one
// (FR-3 AC3, INTEGRATION-SPEC §4.2).
func (r *run) adopt(ctx context.Context) (StageReport, bool, error) {
	ch, err := r.a.Host.ChangeForItem(ctx, ports.ItemRef(r.req.Item))
	if err != nil {
		return StageReport{}, false, err
	}
	if ch == nil || ch.State != ports.ChangeStateOpen {
		return StageReport{}, false, nil
	}
	res := r.result(contract.OutcomeDone)
	res.Implement = &core.ImplementOutcome{Change: string(ch.Ref), Head: string(ch.Head)}
	return StageReport{Result: res}, true, nil
}

// timeLimit is the stage's limit, cut to the run's deadline.
func (r *run) timeLimit(limit time.Duration) time.Duration {
	if d := r.req.Run.Deadline; !d.IsZero() {
		if left := d.Sub(r.a.now()); limit <= 0 || left < limit {
			limit = left
		}
	}
	if limit <= 0 {
		limit = time.Second
	}
	return limit
}

// heartbeat calls the Heartbeat hook until the returned stop is called.
func (a *Activities) heartbeat(ctx context.Context) func() {
	if a.Heartbeat == nil {
		return func() {}
	}
	every := a.HeartbeatEvery
	if every <= 0 {
		every = 30 * time.Second
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				a.Heartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

// redactingSource adds every token it hands out to the redactor, so posted
// text never carries the run's tokens (FR-13 AC3).
type redactingSource struct {
	src ports.TokenSource
	red *secrets.Redactor
}

func (s redactingSource) Token(ctx context.Context) (ports.Token, error) {
	t, err := s.src.Token(ctx)
	if err == nil && t.Secret() != "" {
		s.red.Add(t.Secret(), t.ExpiresAt.Add(time.Hour))
	}
	return t, err
}

func (a *Activities) redacting(l *secrets.Lease) ports.TokenSource {
	src := redactingSource{src: l, red: a.redactor()}
	src.Token(context.Background())
	return src
}

// writeInput writes input.json: references only, never tracker text or a
// session reference (NFR-8 AC1, INTEGRATION-SPEC §4).
func (r *run) writeInput(ctx context.Context) error {
	a, rs := r.a, r.req.Run
	id := "coder"
	if r.stage == contract.StageReview {
		id = "reviewer"
	}
	in := contract.Input{
		ContractVersion: contract.Version,
		RunID:           rs.RunID,
		Stage:           r.stage,
		Attempt:         rs.Attempt,
		Identity:        id,
		Project:         contract.InputProject{Name: a.Project, Tracker: a.TrackerAdapter, CodeHost: a.CodeHostAdapter},
		Repo:            contract.InputRepo{URL: a.Repo.URL, BaseBranch: a.Repo.BaseBranch},
		Workspace:       contract.Workspace{Path: r.dir.Path, Branch: r.dir.Branch, Head: string(r.dir.Head)},
		Item:            contract.ItemRef{Ref: r.req.Item, URL: r.req.ItemURL, Type: r.req.ItemType},
	}
	if in.Attempt < 1 {
		in.Attempt = 1
	}
	// A review's worktree is detached at the reviewed commit (INTEGRATION-SPEC
	// §3.3), but the input still names the change's branch: the schema
	// requires it (work plan #91 D2).
	if in.Workspace.Branch == "" {
		in.Workspace.Branch = r.branch
	}
	in.Deadline = r.deadline.UTC().Format(time.RFC3339)
	if r.change != nil {
		tip, err := a.Git.FetchedHead(ctx, r.dir, a.Repo.BaseBranch)
		if err != nil {
			return fmt.Errorf("workflows: base tip: %w", err)
		}
		in.Change = &contract.ChangeRef{Ref: string(r.change.Ref), URL: r.change.URL, Head: string(r.change.Head), BaseTip: string(tip)}
	}
	switch r.stage {
	case contract.StageReview:
		rv := rs.Review
		ri := &contract.ReviewInput{
			Kind:              rv.Kind,
			Commit:            rv.Commit,
			BlockingThreshold: a.threshold(),
			RuleFindings:      nonNil(rv.RuleFindings),
			RuleManual:        nonNil(rv.RuleManual),
		}
		if rv.Kind == contract.KindRound {
			n := rv.Round
			ri.Round = &n
		}
		if rv.Kind == contract.KindBaseUpdate {
			paths, err := a.Git.ResolvedPaths(ctx, r.dir, ports.Commit(rv.Commit))
			if err != nil {
				return fmt.Errorf("workflows: resolved paths: %w", err)
			}
			ri.ResolvedPaths = nonNil(paths)
		}
		in.Review = ri
	case contract.StageAddress:
		ad := rs.Address
		in.Address = &contract.AddressInput{Mode: ad.Mode, AnswerFindings: nonNil(ad.AnswerFindings), AnswerManual: nonNil(ad.AnswerManual)}
	}
	r.in = in
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.runDir, "input.json"), data, 0o600)
}

func (a *Activities) threshold() int {
	if a.BlockingThreshold > 0 {
		return a.BlockingThreshold
	}
	return core.DefaultBlockingThreshold
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// post acts on a valid result: the stage checks that need the repository
// or the tracker (contract work plan D3), then the records.
func (r *run) post(ctx context.Context, res contract.Result) (StageReport, error) {
	switch res.Result {
	case contract.OutcomeFailed:
		return r.failed(res.Failed.Reason), nil
	case contract.OutcomeNeedsOwner:
		file, err := r.detail(res.NeedsOwner.QuestionFile)
		if err != nil {
			return StageReport{}, err
		}
		return StageReport{Result: r.result(contract.OutcomeNeedsOwner), DetailFile: file}, nil
	case contract.OutcomeBlocked:
		return r.blocked(ctx, res.Blocked)
	case contract.OutcomeDone:
		switch r.stage {
		case contract.StageImplement:
			return r.postImplement(ctx, res.Done.Implement)
		case contract.StageReview:
			return r.postReview(ctx, res.Done.Review)
		case contract.StageAddress:
			return r.postAddress(ctx, res.Done.Address)
		}
	}
	return r.failed(fmt.Sprintf("result %q is not handled for %s", res.Result, r.stage)), nil
}

// detail writes the redacted text of a result file to detail.md in the run
// directory and returns its path.
func (r *run) detail(rel string) (string, error) {
	s, err := readOut(r.outDir, rel)
	if err != nil {
		return "", err
	}
	p := filepath.Join(r.runDir, "detail.md")
	return p, os.WriteFile(p, []byte(r.a.redactor().Redact(s)), 0o600)
}

// blocked reports the named dependency's state (core work plan D14). The
// dependency is added to the item's dependencies first, so its state comes
// from the tracker's dependency view, where a merged change counts as done
// (FR-2 AC6).
func (r *run) blocked(ctx context.Context, b *contract.Blocked) (StageReport, error) {
	file, err := r.detail(b.ReasonFile)
	if err != nil {
		return StageReport{}, err
	}
	res := r.result(contract.OutcomeBlocked)
	res.Blocked = &core.BlockedOutcome{}
	rep := StageReport{Result: res, DetailFile: file}
	if b.Dependency == nil {
		return rep, nil
	}
	dep := ports.ItemRef(strings.TrimSpace(*b.Dependency))
	res.Blocked.Dependency = string(dep)
	res.Blocked.State = core.DependencyNotDone
	item := ports.ItemRef(r.req.Item)
	err = r.a.retry(ctx, func() error { return r.a.Tracker.AddDependency(ctx, item, dep, r.key("depend", string(dep))) })
	if errors.Is(err, ports.ErrNotFound) {
		return rep, nil // no such item: it can never be done
	}
	if err != nil {
		return StageReport{}, err
	}
	deps, err := r.a.Tracker.Dependencies(ctx, item)
	if err != nil {
		return StageReport{}, err
	}
	for _, d := range deps {
		if d.Ref != dep {
			continue
		}
		switch {
		case d.ChangeMerged || (d.CloseReason == ports.CloseCompleted && r.a.isOwner(d.ClosedBy)):
			res.Blocked.State = core.DependencyDone
		case d.CloseReason == ports.CloseOpen || d.CloseReason == "":
			res.Blocked.State = core.DependencyOpen
		}
	}
	return rep, nil
}

func (a *Activities) isOwner(act *ports.Actor) bool {
	if act == nil {
		return false
	}
	for _, o := range a.Owners {
		if o.ID != "" && o.ID == act.ID {
			return true
		}
	}
	return false
}

// branchN is n of the change record: 1 for <prefix><item>, k for
// <prefix><item>-<k> (INTEGRATION-SPEC §6.2).
func (r *run) branchN() int {
	rest := strings.TrimPrefix(r.branch, r.a.BranchPrefix+r.req.Item)
	if k, err := strconv.Atoi(strings.TrimPrefix(rest, "-")); err == nil && strings.HasPrefix(rest, "-") && k >= 2 {
		return k
	}
	return 1
}

// postImplement checks that done.head is the branch's remote head, then
// opens the change as the coder identity, linked to the item, or adopts the
// one that appeared meanwhile (INTEGRATION-SPEC §5.2 implement).
func (r *run) postImplement(ctx context.Context, d *contract.ImplementDone) (StageReport, error) {
	a := r.a
	head, err := a.Git.RemoteHead(ctx, a.Repo, r.branch)
	if errors.Is(err, ports.ErrNotFound) {
		return r.failed(fmt.Sprintf("done.head %s: the branch %s is not on the remote", d.Head, r.branch)), nil
	}
	if err != nil {
		return StageReport{}, err
	}
	if string(head) != d.Head {
		return r.failed(fmt.Sprintf("done.head %s is not the remote head %s of %s", d.Head, head, r.branch)), nil
	}
	if rep, ok, err := r.adopt(ctx); err != nil || ok {
		return rep, err
	}
	body, err := readOut(r.outDir, d.Change.BodyFile)
	if err != nil {
		return StageReport{}, err
	}
	key := r.key("open_change", r.branch)
	rec, err := record(contract.Record{Type: contract.RecordChange, Key: string(key), Item: r.req.Item, N: r.branchN()})
	if err != nil {
		return StageReport{}, err
	}
	n := ports.NewChange{
		Item: ports.ItemRef(r.req.Item), Branch: r.branch, Base: a.Repo.BaseBranch,
		Title: a.text(d.Change.Title), Body: a.text(body), Record: rec,
	}
	var ref ports.ChangeRef
	err = a.retry(ctx, func() (err error) {
		ref, err = a.Host.OpenChange(ctx, n, ports.IdentityCoder, key)
		return err
	})
	if err != nil {
		return StageReport{}, err
	}
	res := r.result(contract.OutcomeDone)
	res.Implement = &core.ImplementOutcome{Change: string(ref), Head: d.Head}
	return StageReport{Result: res}, nil
}
