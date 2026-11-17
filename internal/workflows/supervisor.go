package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/projection"
)

// Projection is the read model the workflows write (TECHNICAL-SPEC §10).
// *projection.DB implements it.
type Projection interface {
	RecordDecision(ctx context.Context, d projection.Decision) error
	StartRun(ctx context.Context, r projection.StageRun) error
	FinishRun(ctx context.Context, runID string, ended time.Time, result, logPath string) error
	RecordRelease(ctx context.Context, r projection.Release) error
	DailyCount(ctx context.Context, project string, day time.Time, loc *time.Location) (int, error)
}

var _ Projection = (*projection.DB)(nil)

// role is the role of an actor, matched by its stable account ID (FR-13
// AC7, INTEGRATION-SPEC §8.1).
func (a *Activities) role(act ports.Actor) core.Role {
	ids := a.Identities
	if len(ids.Owners) == 0 {
		for _, o := range a.Owners {
			ids.Owners = append(ids.Owners, o.ID)
		}
	}
	return ids.Role(act.ID)
}

// LoadRequest asks for the facts of a claimed item for core.Started.
type LoadRequest struct {
	WorkflowID string
	Item       string
	Claim      int
}

// LoadItem reads what a new item workflow starts from (FR-2 AC5, FR-3 AC3,
// FR-6 AC5, TECHNICAL-SPEC §10): the open change and its review records,
// the rounds added by resumes, the approved commits, and the number of
// changes closed without merging (work plan D9).
func (a *Activities) LoadItem(ctx context.Context, req LoadRequest) (core.Started, error) {
	ref := ports.ItemRef(req.Item)
	st := core.Started{At: core.At{Time: a.now()}, WorkflowID: req.WorkflowID, Item: req.Item, Claim: req.Claim}
	recs, err := a.Tracker.FindRecords(ctx, ref, ports.RecordStatus)
	if err != nil {
		return st, err
	}
	for _, r := range decodeRecords(recs) {
		if r.Event == contract.EventResume && r.RoundsAdded != nil {
			st.RoundsAdded += *r.RoundsAdded
		}
	}
	appr, err := a.Tracker.FindRecords(ctx, ref, ports.RecordApprovedCommits)
	if err != nil {
		return st, err
	}
	for _, r := range decodeRecords(appr) {
		st.ApprovedCommits = append(st.ApprovedCommits, r.Commits...)
	}
	ch, err := a.Host.ChangeForItem(ctx, ref)
	if err != nil {
		return st, err
	}
	if ch != nil && ch.State == ports.ChangeStateOpen {
		st.Change = &core.Change{Ref: string(ch.Ref), Head: string(ch.Head)}
		if st.Reviews, err = a.reviewRecords(ctx, ch.Ref); err != nil {
			return st, err
		}
		if n := branchNumber(a.BranchPrefix, req.Item, ch.Branch); n > 0 {
			st.ClosedChanges = n - 1
			return st, nil
		}
	}
	// No change on a loop branch: every loop branch already on the remote
	// was used by a change, so the next implement takes the next number
	// (D44, work plan D9).
	for n := 1; n <= 100; n++ {
		_, err := a.Git.RemoteHead(ctx, a.Repo, itemBranch(a.BranchPrefix, req.Item, n))
		if errors.Is(err, ports.ErrNotFound) {
			st.ClosedChanges = n - 1
			break
		}
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

// branchNumber is n of a loop branch name (INTEGRATION-SPEC §3.3), or 0 for
// a branch outside the prefix.
func branchNumber(prefix, item, branch string) int {
	rest, ok := strings.CutPrefix(branch, prefix+item)
	if !ok {
		return 0
	}
	if rest == "" {
		return 1
	}
	var k int
	if _, err := fmt.Sscanf(rest, "-%d", &k); err == nil && fmt.Sprintf("-%d", k) == rest && k >= 2 {
		return k
	}
	return 0
}

func decodeRecords(recs []ports.Record) []contract.Record {
	var out []contract.Record
	for _, r := range recs {
		var rec contract.Record
		if json.Unmarshal(r.Data, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// reviewRecords are the change's review records as the core reads them,
// oldest first, each finding with its thread (INTEGRATION-SPEC §6.3).
func (a *Activities) reviewRecords(ctx context.Context, ref ports.ChangeRef) ([]core.ReviewRecord, error) {
	rrs, err := a.Host.ReviewRecords(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []core.ReviewRecord
	for _, rr := range rrs {
		var rec contract.Record
		if json.Unmarshal(rr.Record.Data, &rec) != nil {
			continue
		}
		r := core.ReviewRecord{Commit: rec.Commit, Kind: contract.ReviewKind(rec.Kind), Stale: rec.Stale}
		for _, f := range rec.Findings {
			c := f.Comment
			if t, ok := rr.Threads[ports.FindingID(f.ID)]; ok && c == "" {
				c = string(t)
			}
			r.Findings = append(r.Findings, core.Finding{ID: f.ID, Score: f.Score, Status: f.Status, Comment: c})
		}
		out = append(out, r)
	}
	return out, nil
}

// Cursors are the project's change-feed cursors (TECHNICAL-SPEC §6.2).
type Cursors struct {
	Tracker ports.Cursor
	Host    ports.Cursor
}

// PollRequest is one poll: the cursors and the items with a running
// workflow.
type PollRequest struct {
	Cursors Cursors
	Items   []string
}

// Routed is one normalised event for an item's workflow.
type Routed struct {
	Item   string
	Signal ItemSignal
}

// PollReport is a poll's events, in feed order, and the new cursors.
type PollReport struct {
	Cursors Cursors
	Events  []Routed
}

// Poll reads both change feeds from the cursors and turns each change into
// the core event for the claimed item it belongs to (TECHNICAL-SPEC §6.2
// step 1). Events for items with no running workflow are dropped: picking
// reads those items afresh (work plan D10). A closed item is also routed
// as DependencyClosed to each running item it blocks (FR-18, work plan
// D11).
func (a *Activities) Poll(ctx context.Context, req PollRequest) (PollReport, error) {
	running := map[string]bool{}
	for _, it := range req.Items {
		running[it] = true
	}
	rep := PollReport{Cursors: req.Cursors}
	evs, cur, err := a.Tracker.Changes(ctx, req.Cursors.Tracker)
	if err != nil {
		return rep, err
	}
	for _, ev := range evs {
		item := string(ev.Item)
		at := core.At{Time: ev.At}
		if d, ok := ev.Detail.(ports.ItemClosed); ok {
			deps, err := a.dependencyClosed(ctx, req.Items, ev.Item, at)
			if err != nil {
				return rep, err
			}
			rep.Events = append(rep.Events, deps...)
			if running[item] {
				rep.Events = append(rep.Events, Routed{Item: item, Signal: MustSignal(core.ItemClosed{At: at, By: a.role(d.Actor)})})
			}
			continue
		}
		if !running[item] {
			continue
		}
		var e core.Event
		switch d := ev.Detail.(type) {
		case ports.MarkerChanged:
			e = core.MarkerChanged{At: at, By: a.role(d.Actor), Marker: string(d.Marker), Added: d.On}
		case ports.AssigneeChanged:
			if d.Human {
				e = core.Assigned{At: at, By: a.role(d.Actor)}
			}
		case ports.Commented:
			e = core.Commented{At: at, By: a.role(d.Actor)}
		}
		if e != nil {
			rep.Events = append(rep.Events, Routed{Item: item, Signal: MustSignal(e)})
		}
	}
	rep.Cursors.Tracker = cur

	cevs, hcur, err := a.Host.Changes(ctx, req.Cursors.Host)
	if err != nil {
		return rep, err
	}
	changes := map[ports.ChangeRef]ports.Change{}
	for _, ev := range cevs {
		ch, ok := changes[ev.Change]
		if !ok {
			if ch, err = a.Host.Change(ctx, ev.Change); err != nil {
				return rep, err
			}
			changes[ev.Change] = ch
		}
		item := string(ch.Item)
		if !running[item] {
			continue
		}
		routed, err := a.changeEvents(ctx, ch, ev)
		if err != nil {
			return rep, err
		}
		for _, e := range routed {
			rep.Events = append(rep.Events, Routed{Item: item, Signal: MustSignal(e)})
		}
	}
	rep.Cursors.Host = hcur
	return rep, nil
}

// changeEvents turns one code-host event into core events.
func (a *Activities) changeEvents(ctx context.Context, ch ports.Change, ev ports.ChangeEvent) ([]core.Event, error) {
	at := core.At{Time: ev.At}
	ref := string(ch.Ref)
	switch d := ev.Detail.(type) {
	case ports.HeadMoved:
		e := core.HeadMoved{At: at, Commit: string(d.Commit), Pusher: core.Pusher(d.Pusher)}
		if d.Pusher == ports.PusherUnknown {
			// Every commit the push added, so an untrusted push escalates
			// and is approved as a whole (#11 D6, work plan D12).
			tr, err := a.Host.Trust(ctx, ch.Ref)
			if err != nil {
				return nil, err
			}
			for _, c := range tr.Commits {
				e.Commits = append(e.Commits, string(c))
			}
		}
		return []core.Event{e}, nil
	case ports.Merged:
		return []core.Event{core.ChangeMerged{At: at, By: a.role(d.Actor), Change: ref}}, nil
	case ports.ChangeClosed:
		return []core.Event{core.ChangeClosed{At: at, By: a.role(d.Actor), Change: ref}}, nil
	case ports.ChangeReopened:
		rs, err := a.reviewRecords(ctx, ch.Ref)
		if err != nil {
			return nil, err
		}
		return []core.Event{core.ChangeReopened{At: at, Change: core.Change{Ref: ref, Head: string(ch.Head)}, Reviews: rs}}, nil
	case ports.ReviewSubmitted, ports.ThreadResolved, ports.ThreadUnresolved, ports.ReviewDismissed:
		return a.manualEvents(ctx, ch.Ref, at)
	}
	return nil, nil
}

// manualEvents reports every manual comment on the change with its
// clearing (FR-7 AC9). The item workflow sets Reopened when a comment a
// human had cleared is no longer cleared (work plan D13).
func (a *Activities) manualEvents(ctx context.Context, ref ports.ChangeRef, at core.At) ([]core.Event, error) {
	mcs, err := a.Host.ManualComments(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []core.Event
	for _, m := range mcs {
		out = append(out, core.ManualCommentChanged{
			At: at, Comment: m.ID,
			Empty:        m.Kind == ports.ManualCommentEmptyChangesRequested,
			HumanCleared: m.Cleared && m.ClearedBy != ports.ClearedByNone,
		})
	}
	return out, nil
}

// dependencyClosed routes DependencyClosed to each running item that the
// closed item blocks, with done as the dependency's state says (work plan
// #20 D14).
func (a *Activities) dependencyClosed(ctx context.Context, items []string, closed ports.ItemRef, at core.At) ([]Routed, error) {
	var out []Routed
	for _, it := range items {
		if it == string(closed) {
			continue
		}
		deps, err := a.Tracker.Dependencies(ctx, ports.ItemRef(it))
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d.Ref != closed {
				continue
			}
			done := d.ChangeMerged || (d.CloseReason == ports.CloseCompleted && a.isOwner(d.ClosedBy))
			out = append(out, Routed{Item: it, Signal: MustSignal(core.DependencyClosed{At: at, Dependency: string(closed), Done: done})})
		}
	}
	return out, nil
}

// Candidates reads the open items with the ready marker as core.Pick sees
// them (FR-2 AC1).
func (a *Activities) Candidates(ctx context.Context) ([]core.Candidate, error) {
	items, err := a.Tracker.Candidates(ctx)
	if err != nil {
		return nil, err
	}
	var out []core.Candidate
	for _, it := range items {
		c := core.Candidate{Key: string(it.Ref), Created: it.CreatedAt, Closed: it.CloseReason != ports.CloseOpen && it.CloseReason != ""}
		for _, m := range it.Markers {
			switch m {
			case ports.MarkerReady:
				c.Ready = true
			case ports.MarkerInProgress:
				c.Claimed = true
			case ports.MarkerAborted:
				c.Aborted = true
			case ports.MarkerPaused:
				c.Paused = true
			case ports.MarkerNeedsOwner:
				c.NeedsOwner = true
			case ports.MarkerPlan:
				c.Plan = true
			default:
				if t, ok := strings.CutPrefix(string(m), "type:"); ok {
					c.Types = append(c.Types, t)
				}
			}
		}
		for _, as := range it.Assignees {
			if as.Kind == ports.ActorHuman && a.role(as) != core.RoleCoder {
				c.HumanAssignee = true
			}
		}
		deps, err := a.Tracker.Dependencies(ctx, it.Ref)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			st := core.DependencyNotDone
			switch {
			case d.ChangeMerged || (d.CloseReason == ports.CloseCompleted && a.isOwner(d.ClosedBy)):
				st = core.DependencyDone
			case d.CloseReason == ports.CloseOpen || d.CloseReason == "":
				st = core.DependencyOpen
			}
			c.Dependencies = append(c.Dependencies, core.Dependency{Key: string(d.Ref), State: st})
		}
		if len(c.Dependencies) > 0 {
			recs, err := a.Tracker.FindRecords(ctx, it.Ref, ports.RecordStatus)
			if err != nil {
				return nil, err
			}
			for _, r := range decodeRecords(recs) {
				if r.Reason == core.ReasonDependencyNotDone {
					c.DependencyRecords++
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// ClaimPlanReport is a planned claim: the item's version and claim number,
// and what its workflow needs.
type ClaimPlanReport struct {
	Plan     core.ClaimPlan
	Version  ports.Version
	URL      string
	ItemType string
}

// PlanClaim reads what claiming an item needs (TECHNICAL-SPEC §6.1): its
// version for the compare-and-set and its claim number, one more than its
// claim records. It writes nothing, so Claim's retries all use one key.
func (a *Activities) PlanClaim(ctx context.Context, item string) (ClaimPlanReport, error) {
	ref := ports.ItemRef(item)
	it, err := a.Tracker.Item(ctx, ref)
	if err != nil {
		return ClaimPlanReport{}, err
	}
	recs, err := a.Tracker.FindRecords(ctx, ref, ports.RecordStatus)
	if err != nil {
		return ClaimPlanReport{}, err
	}
	n := 0
	for _, r := range decodeRecords(recs) {
		if r.Event == contract.EventClaim {
			n++
		}
	}
	rep := ClaimPlanReport{Plan: core.ClaimFor(a.Project, item, n), Version: it.Version, URL: it.URL, ItemType: "feature"}
	for _, m := range it.Markers {
		if m == ports.MarkerTypeBug {
			rep.ItemType = "bug"
		}
	}
	return rep, nil
}

// Claim claims the item with compare-and-set (FR-2 AC5, INTEGRATION-SPEC
// §9.3). False means the item changed since PlanClaim read it, and nothing
// starts. A retry with the same plan finds its own claim record.
func (a *Activities) Claim(ctx context.Context, p ClaimPlanReport) (bool, error) {
	_, err := a.Tracker.Claim(ctx, ports.ItemRef(p.Plan.Item), p.Version, ports.IdemKey(p.Plan.StatusKey))
	if errors.Is(err, ports.ErrConflict) {
		return false, nil
	}
	return err == nil, err
}

// EscalateDependency escalates an unclaimed item whose dependency was
// closed without being done: needs-owner and a comment naming the
// dependency (FR-2 AC6).
func (a *Activities) EscalateDependency(ctx context.Context, e core.DependencyEscalation) error {
	if err := a.Tracker.SetMarker(ctx, ports.ItemRef(e.Item), ports.MarkerNeedsOwner, true, ports.IdemKey(e.Key+":marker")); err != nil {
		return err
	}
	_, err := a.PostStatus(ctx, StatusRequest{Item: e.Item, Action: core.PostStatus{
		Key: e.Key, Event: contract.EventEscalate, Reason: core.ReasonDependencyNotDone, Dependency: e.Dependency,
	}})
	return err
}

// Reconcile reads every running item afresh and reports the ones closed
// meanwhile, so a missed event still stops their runs (FR-12 AC1).
func (a *Activities) Reconcile(ctx context.Context, items []string) ([]Routed, error) {
	var out []Routed
	for _, item := range items {
		it, err := a.Tracker.Item(ctx, ports.ItemRef(item))
		if errors.Is(err, ports.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if it.CloseReason == ports.CloseOpen || it.CloseReason == "" {
			continue
		}
		by := core.RoleOther
		if it.ClosedBy != nil {
			by = a.role(*it.ClosedBy)
		}
		out = append(out, Routed{Item: item, Signal: MustSignal(core.ItemClosed{At: core.At{Time: a.now()}, By: by})})
	}
	return out, nil
}

// CloseRequest closes an item.
type CloseRequest struct {
	Item string
	Key  string
}

// CloseItem closes the item as completed, unless it is closed already
// (FR-12 AC4).
func (a *Activities) CloseItem(ctx context.Context, req CloseRequest) error {
	it, err := a.Tracker.Item(ctx, ports.ItemRef(req.Item))
	if err != nil {
		return err
	}
	if it.CloseReason != ports.CloseOpen && it.CloseReason != "" {
		return nil
	}
	return a.Tracker.Close(ctx, ports.ItemRef(req.Item), ports.CloseCompleted, ports.IdemKey(req.Key))
}

// DecisionRequest is one decision for the projection.
type DecisionRequest struct {
	State  core.ItemState
	Policy core.Policy
	Record core.DecisionRecord
	View   projection.View
}

// RecordDecision writes a decision and the item's board row (TECHNICAL-SPEC
// §10, NFR-5 AC1).
func (a *Activities) RecordDecision(ctx context.Context, req DecisionRequest) error {
	if a.Projection == nil {
		return nil
	}
	return a.Projection.RecordDecision(ctx, projection.NewDecision(a.Project, req.State, req.Policy, req.Record, req.View))
}

// RunStarted records a stage run's start (stage_runs, FR-11 AC2).
func (a *Activities) RunStarted(ctx context.Context, r projection.StageRun) error {
	if a.Projection == nil {
		return nil
	}
	r.Project = a.Project
	return a.Projection.StartRun(ctx, r)
}

// RunEnd is how a stage run ended.
type RunEnd struct {
	RunID   string
	Ended   time.Time
	Result  string
	LogPath string
}

// RunFinished records a stage run's end.
func (a *Activities) RunFinished(ctx context.Context, r RunEnd) error {
	if a.Projection == nil {
		return nil
	}
	err := a.Projection.FinishRun(ctx, r.RunID, r.Ended, r.Result, r.LogPath)
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	return err
}

// RecordRelease takes a released claim off the board (FR-11 AC1).
func (a *Activities) RecordRelease(ctx context.Context, r projection.Release) error {
	if a.Projection == nil {
		return nil
	}
	r.Project = a.Project
	return a.Projection.RecordRelease(ctx, r)
}

// DailyRequest asks for a day's agent-run count.
type DailyRequest struct {
	Day      time.Time
	Timezone string
}

// DailyRuns rebuilds the day's agent-run count from stage_runs, for a
// ProjectWorkflow that starts with no state (TECHNICAL-SPEC §10).
func (a *Activities) DailyRuns(ctx context.Context, req DailyRequest) (int, error) {
	if a.Projection == nil {
		return 0, nil
	}
	loc, err := location(req.Timezone)
	if err != nil {
		return 0, err
	}
	return a.Projection.DailyCount(ctx, a.Project, req.Day, loc)
}

// ProjectNotice is a project-level notification: the daily limit was
// reached, or the day's count was reset (FR-16 AC3, AC4).
type ProjectNotice struct {
	Key     string
	Reason  string
	Message string
}

// NotifyProject notifies the owner of a project-level event.
func (a *Activities) NotifyProject(ctx context.Context, n ProjectNotice) error {
	if a.Notifier == nil {
		return nil
	}
	return a.Notifier.Notify(ctx, ports.Notification{Project: a.Project, Reason: n.Reason, Message: n.Message, Key: ports.IdemKey(n.Key)})
}

// location loads a profile time zone; empty is the server's local zone
// (FR-16 AC4).
func location(tz string) (*time.Location, error) {
	if tz == "" {
		return time.Local, nil
	}
	return time.LoadLocation(tz)
}
