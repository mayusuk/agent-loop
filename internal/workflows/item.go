package workflows

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/projection"
)

// acts is a nil Activities used only to name activities in workflow code.
var acts *Activities

// Defaults (TECHNICAL-SPEC §6.2, §6.5).
const (
	DefaultPollInterval       = 60 * time.Second
	DefaultSupervisorInterval = 10 * time.Minute
	// ItemEventLimit is how many decisions an ItemWorkflow makes before it
	// continues as new (TECHNICAL-SPEC §6.2).
	ItemEventLimit   = 2000
	heartbeatTimeout = 2 * time.Minute
)

// ItemSettings are the project settings an item workflow needs.
type ItemSettings struct {
	Project string
	Policy  core.Policy
	// StageCaps are each stage's required capabilities; a stage with none
	// requires agent (TECHNICAL-SPEC §6.4).
	StageCaps map[contract.Stage][]string `json:",omitempty"`
	// PollInterval is the wait before a merge check again while a gate is
	// pending (TECHNICAL-SPEC §8.1 cause 6). Zero is 60 s.
	PollInterval time.Duration
	// EventLimit overrides ItemEventLimit (tests).
	EventLimit int `json:",omitempty"`
}

// ItemInput starts an ItemWorkflow: the claim, and on continue-as-new the
// state and the signals not yet decided.
type ItemInput struct {
	Settings   ItemSettings
	WorkflowID string
	Item       string
	ItemURL    string
	ItemType   string
	Claim      int
	State      *core.ItemState `json:",omitempty"`
	Pending    []ItemSignal    `json:",omitempty"`
}

// ItemView is the item query's answer, for the board (TECHNICAL-SPEC
// §6.3).
type ItemView struct {
	State      core.ItemState
	NextAction string
	WaitReason string
	// Running is the run ID of the stage run, merge check or base update
	// in flight, if any.
	Running string
}

// task is the one stage run, base update or merge step in flight.
type task struct {
	seq    int
	id     string
	cancel workflow.CancelFunc
}

// outcome is a task's result for the main loop. A nil event means the task
// was stopped and nothing is decided.
type outcome struct {
	seq    int
	event  core.Event
	detail string
}

type itemWF struct {
	ctx       workflow.Context
	in        ItemInput
	s         core.ItemState
	decisions int
	view      projection.View
	running   *task
	seq       int
	results   workflow.Channel
	waiting   map[string]workflow.Channel
	detail    string
	ended     bool
}

func (w *itemWF) coreCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           CoreQueue,
		StartToCloseTimeout: 5 * time.Minute,
		// Adapter activities are idempotent, so Temporal retries them
		// with backoff until they succeed (TECHNICAL-SPEC §6.5).
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute},
	})
}

// ItemWorkflow runs one claimed item's lifecycle (TECHNICAL-SPEC §6.3): it
// decides each event with core.Decide and carries out the actions. Its ID
// is item/<project>/<item>, the item's lock (FR-2 AC4).
func ItemWorkflow(ctx workflow.Context, in ItemInput) error {
	w := &itemWF{ctx: ctx, in: in, results: workflow.NewChannel(ctx), waiting: map[string]workflow.Channel{}}
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (ItemView, error) {
		v := ItemView{State: w.s, NextAction: w.view.NextAction, WaitReason: w.view.WaitReason}
		if w.running != nil {
			v.Running = w.running.id
		}
		return v, nil
	}); err != nil {
		return err
	}
	signals := workflow.GetSignalChannel(ctx, SignalEvent)
	admissions := workflow.GetSignalChannel(ctx, SignalAdmission)
	workflow.Go(ctx, func(ctx workflow.Context) {
		for {
			var r AdmissionReply
			admissions.Receive(ctx, &r)
			if c := w.waiting[r.RunID]; c != nil {
				c.SendAsync(r)
			}
		}
	})

	if in.State != nil {
		w.s = *in.State
	} else {
		var st core.Started
		req := LoadRequest{WorkflowID: in.WorkflowID, Item: in.Item, Claim: in.Claim}
		if err := workflow.ExecuteActivity(w.coreCtx(ctx), acts.LoadItem, req).Get(ctx, &st); err != nil {
			return err
		}
		st.Time = workflow.Now(ctx)
		w.decide(st)
	}
	for _, p := range in.Pending {
		if w.ended {
			break
		}
		if e := p.Event(); e != nil {
			w.decide(e)
		}
	}

	for !w.ended {
		if w.running == nil && w.decisions >= w.eventLimit() {
			return w.continueAsNew(signals)
		}
		var next core.Event
		var detail string
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) {
			var s ItemSignal
			c.Receive(ctx, &s)
			next = s.Event()
		})
		sel.AddReceive(w.results, func(c workflow.ReceiveChannel, _ bool) {
			var o outcome
			c.Receive(ctx, &o)
			if w.running == nil || o.seq != w.running.seq {
				return // a stopped or replaced task
			}
			w.running = nil
			next, detail = o.event, o.detail
		})
		sel.Select(ctx)
		if next == nil {
			continue
		}
		w.detail = detail
		w.decide(next)
		w.detail = ""
	}
	return nil
}

func (w *itemWF) eventLimit() int {
	if w.in.Settings.EventLimit > 0 {
		return w.in.Settings.EventLimit
	}
	return ItemEventLimit
}

// continueAsNew carries the state and every signal not yet decided into a
// new run (TECHNICAL-SPEC §6.2).
func (w *itemWF) continueAsNew(signals workflow.ReceiveChannel) error {
	next := w.in
	s := w.s
	next.State = &s
	next.Pending = nil
	for {
		var sig ItemSignal
		if !signals.ReceiveAsync(&sig) {
			break
		}
		next.Pending = append(next.Pending, sig)
	}
	return workflow.NewContinueAsNewError(w.ctx, ItemWorkflow, next)
}

// decide runs core.Decide and carries out its actions in order.
func (w *itemWF) decide(e core.Event) {
	ctx := w.ctx
	if m, ok := e.(core.ManualCommentChanged); ok && !m.HumanCleared {
		// A comment a human had cleared is open again (work plan D13).
		for _, c := range w.s.ManualComments {
			if c.ID == m.Comment && c.HumanCleared {
				m.Reopened = true
			}
		}
		e = m
	}
	s, actions := core.Decide(w.s, e, w.in.Settings.Policy)
	w.s = s
	w.decisions++
	w.view.NextAction, w.view.WaitReason = "", ""
	for _, a := range actions {
		if _, ok := a.(core.RecordDecision); !ok && w.view.NextAction == "" {
			w.view.NextAction = a.Kind()
		}
		if wt, ok := a.(core.Wait); ok {
			w.view.WaitReason = wt.Reason
		}
	}
	for _, a := range actions {
		if err := w.do(a); err != nil {
			workflow.GetLogger(ctx).Error("action failed", "action", a.Kind(), "error", err)
		}
	}
	if w.s.State == core.StateMerged && !w.ended {
		w.mergedByLoop()
	}
}

// mergedByLoop ends the item after the loop's own merge (#11 D13, work
// plan D14): it clears in-progress, closes the item if the tracker didn't,
// and frees the WIP slot.
func (w *itemWF) mergedByLoop() {
	ctx := w.coreCtx(w.ctx)
	key := fmt.Sprintf("%s#%d:merged", w.in.WorkflowID, w.in.Claim)
	mk := core.SetMarker{Key: key + ":marker", Marker: core.MarkerInProgress, On: false}
	w.log(workflow.ExecuteActivity(ctx, acts.SetMarker, MarkerRequest{Item: w.in.Item, Action: mk}).Get(ctx, nil))
	w.log(workflow.ExecuteActivity(ctx, acts.CloseItem, CloseRequest{Item: w.in.Item, Key: key + ":close_item"}).Get(ctx, nil))
	w.end("merged")
}

func (w *itemWF) log(err error) {
	if err != nil {
		workflow.GetLogger(w.ctx).Error("activity failed", "error", err)
	}
}

func (w *itemWF) change() string {
	if w.s.Change == nil {
		return ""
	}
	return w.s.Change.Ref
}

// do carries out one action. Adapter actions run to completion here;
// stage runs, base updates and merge steps run as a task, and their
// outcome comes back as the next event.
func (w *itemWF) do(a core.Action) error {
	ctx := w.coreCtx(w.ctx)
	in := w.in
	switch a := a.(type) {
	case core.RunStage:
		w.start(a.RunID, func(ctx workflow.Context) (core.Event, string) { return w.runStage(ctx, a) })
	case core.UpdateBase:
		w.start(a.Key, func(ctx workflow.Context) (core.Event, string) { return w.updateBase(ctx, a), "" })
	case core.CheckMerge:
		id := fmt.Sprintf("%s#%d:check_merge:%d", in.WorkflowID, in.Claim, w.s.Seq)
		w.start(id, func(ctx workflow.Context) (core.Event, string) { return w.checkMerge(ctx, id, a), "" })
	case core.Merge:
		w.start(a.Key, func(ctx workflow.Context) (core.Event, string) { return w.merge(ctx, a), "" })
	case core.StopRun:
		if w.running != nil && w.running.id == a.RunID {
			w.running.cancel()
			w.running = nil
		}
	case core.SetMarker:
		return workflow.ExecuteActivity(ctx, acts.SetMarker, MarkerRequest{Item: in.Item, Action: a, Change: w.change()}).Get(ctx, nil)
	case core.PostStatus:
		req := StatusRequest{Item: in.Item, Claim: in.Claim, Action: a}
		if a.Reason == core.ReasonNeedsOwner || a.Event == contract.EventBlocked {
			req.DetailFile = w.detail
		}
		return workflow.ExecuteActivity(ctx, acts.PostStatus, req).Get(ctx, nil)
	case core.Notify:
		return workflow.ExecuteActivity(ctx, acts.Notify, NotifyRequest{Item: in.Item, URL: in.ItemURL, Action: a}).Get(ctx, nil)
	case core.FileFollowUp:
		return workflow.ExecuteActivity(ctx, acts.FileFollowUp, FollowUpRequest{Action: a}).Get(ctx, nil)
	case core.ApproveCommits:
		return workflow.ExecuteActivity(ctx, acts.ApproveCommits, ApproveRequest{Item: in.Item, Claim: in.Claim, Action: a}).Get(ctx, nil)
	case core.CloseItem:
		return workflow.ExecuteActivity(ctx, acts.CloseItem, CloseRequest{Item: in.Item, Key: a.Key}).Get(ctx, nil)
	case core.EndItem:
		if a.Reason == core.ReasonReleased {
			rel := projection.Release{Item: in.Item, Claim: in.Claim, At: workflow.Now(w.ctx)}
			w.log(workflow.ExecuteActivity(ctx, acts.RecordRelease, rel).Get(ctx, nil))
		}
		w.end(a.Reason)
	case core.RecordDecision:
		req := DecisionRequest{State: w.s, Policy: in.Settings.Policy, Record: a.Record, View: w.view}
		return workflow.ExecuteActivity(ctx, acts.RecordDecision, req).Get(ctx, nil)
	case core.Wait:
	}
	return nil
}

// end stops any task, tells the project the item left (freeing its WIP
// slot), and ends the workflow.
func (w *itemWF) end(reason string) {
	if w.running != nil {
		w.running.cancel()
		w.running = nil
	}
	w.ended = true
	sig := ItemEnded{WorkflowID: w.in.WorkflowID, Item: w.in.Item, Reason: reason}
	ctx, _ := workflow.NewDisconnectedContext(w.ctx)
	if err := workflow.SignalExternalWorkflow(ctx, ProjectID(w.in.Settings.Project), "", SignalItemEnded, sig).Get(ctx, nil); err != nil {
		workflow.GetLogger(w.ctx).Warn("project not told the item ended", "error", err)
	}
}

// start runs f as the item's one task. A task already in flight is
// stopped: its outcome no longer counts.
func (w *itemWF) start(id string, f func(workflow.Context) (core.Event, string)) {
	if w.running != nil {
		w.running.cancel()
	}
	w.seq++
	tctx, cancel := workflow.WithCancel(w.ctx)
	t := &task{seq: w.seq, id: id, cancel: cancel}
	w.running = t
	workflow.Go(tctx, func(ctx workflow.Context) {
		e, detail := f(ctx)
		if ctx.Err() != nil {
			e = nil
		}
		w.results.Send(ctx, outcome{seq: t.seq, event: e, detail: detail})
	})
}

// admit asks the project to start a stage (TECHNICAL-SPEC §6.3, work plan
// D4) and waits for its answer. ok is false when the task was stopped.
func (w *itemWF) admit(ctx workflow.Context, id string, agent bool) (core.Admission, bool) {
	ch := workflow.NewBufferedChannel(ctx, 1)
	w.waiting[id] = ch
	defer delete(w.waiting, id)
	req := AdmitRequest{WorkflowID: w.in.WorkflowID, Item: w.in.Item, RunID: id, Agent: agent}
	for {
		err := workflow.SignalExternalWorkflow(ctx, ProjectID(w.in.Settings.Project), "", SignalAdmit, req).Get(ctx, nil)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return "", false
		}
		workflow.GetLogger(ctx).Warn("admission request not delivered; trying again", "error", err)
		if workflow.Sleep(ctx, w.pollInterval()) != nil {
			return "", false
		}
	}
	var r AdmissionReply
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(ch, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &r) })
	sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
	sel.Select(ctx)
	if ctx.Err() != nil {
		return "", false
	}
	return r.Admission, true
}

func (w *itemWF) pollInterval() time.Duration {
	if w.in.Settings.PollInterval > 0 {
		return w.in.Settings.PollInterval
	}
	return DefaultPollInterval
}

// stageLimit is the stage's time limit: the RunStage StartToCloseTimeout
// (TECHNICAL-SPEC §6.5, #12 D17).
func stageLimit(p core.Policy, st contract.Stage) time.Duration {
	if l := p.Stages[st].TimeLimit; l > 0 {
		return l
	}
	return core.DefaultTimeLimit
}

// runStage is one agent stage: the pre-stage trust check, the project's
// admission, then the RunStage activity (TECHNICAL-SPEC §6.3, §6.5, §8.3).
func (w *itemWF) runStage(ctx workflow.Context, a core.RunStage) (core.Event, string) {
	cctx := w.coreCtx(ctx)
	if a.Trust != nil && a.Change != "" {
		var tr core.TrustReport
		if err := workflow.ExecuteActivity(cctx, acts.TrustCheck, a.Change).Get(ctx, &tr); err != nil {
			return nil, ""
		}
		if tr.Check(a.Trust.Approved).Untrusted {
			return core.TrustRefused{At: core.At{Time: workflow.Now(ctx)}, RunID: a.RunID, Report: tr}, ""
		}
	}
	adm, ok := w.admit(ctx, a.RunID, true)
	if !ok {
		return nil, ""
	}
	if adm == core.AdmitWaitingLimit {
		return core.DailyLimitReached{At: core.At{Time: workflow.Now(ctx)}, RunID: a.RunID}, ""
	}

	run := projection.StageRun{RunID: a.RunID, Item: w.in.Item, Stage: a.Stage, Attempt: a.Attempt, Started: workflow.Now(ctx)}
	switch {
	case a.Review != nil:
		run.Kind = string(a.Review.Kind)
	case a.Address != nil:
		run.Kind = string(a.Address.Mode)
	}
	w.log(workflow.ExecuteActivity(cctx, acts.RunStarted, run).Get(ctx, nil))

	// The limit counts from the admitted start, so the deadline the core
	// set when it decided isn't passed on (#12 D17).
	rs := a
	rs.Deadline = time.Time{}
	req := StageRequest{WorkflowID: w.in.WorkflowID, Item: w.in.Item, ItemURL: w.in.ItemURL, ItemType: w.in.ItemType, Claim: w.in.Claim, Run: rs}
	sctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           CapabilityQueue(w.in.Settings.StageCaps[a.Stage]),
		StartToCloseTimeout: stageLimit(w.in.Settings.Policy, a.Stage),
		HeartbeatTimeout:    heartbeatTimeout,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var rep StageReport
	err := workflow.ExecuteActivity(sctx, acts.RunStage, req).Get(ctx, &rep)
	end := RunEnd{RunID: a.RunID, LogPath: rep.LogPath}
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	dctx = w.coreCtx(dctx)
	var e core.Event
	switch {
	case ctx.Err() != nil:
		end.Result = "stopped"
	case err != nil:
		var te *temporal.TimeoutError
		if errors.As(err, &te) && te.TimeoutType() == enumspb.TIMEOUT_TYPE_START_TO_CLOSE {
			end.Result = "timed_out"
			e = core.StageTimedOut{At: core.At{Time: workflow.Now(ctx)}, RunID: a.RunID}
		} else {
			// A lost heartbeat or a run that couldn't be made: a crash,
			// run again with the same attempt (FR-16 AC1, D42).
			end.Result = contract.ReasonWorkerLost
			e = core.StageCrashed{At: core.At{Time: workflow.Now(ctx)}, RunID: a.RunID}
		}
	default:
		e = rep.Event()
		if rep.TimedOut != nil {
			end.Result = "timed_out"
		} else if rep.Result != nil {
			end.Result = string(rep.Result.Outcome)
		}
		if rep.Cost != nil && *rep.Cost > 0 {
			sp := SpendReport{Item: w.in.Item, RunID: a.RunID, Cost: *rep.Cost, At: workflow.Now(ctx)}
			w.log(workflow.SignalExternalWorkflow(dctx, ProjectID(w.in.Settings.Project), "", SignalSpent, sp).Get(dctx, nil))
		}
	}
	end.Ended = workflow.Now(ctx)
	if end.Result == "" {
		end.Result = contract.ReasonWorkerLost
	}
	w.log(workflow.ExecuteActivity(dctx, acts.RunFinished, end).Get(dctx, nil))
	return e, rep.DetailFile
}

// updateBase is the agent-free base merge (D41). It needs no daily-limit
// unit but waits out a pause (FR-10 AC3).
func (w *itemWF) updateBase(ctx workflow.Context, a core.UpdateBase) core.Event {
	if _, ok := w.admit(ctx, a.Key, false); !ok {
		return nil
	}
	sctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           CoreQueue,
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    heartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute},
	})
	var rep BaseUpdateReport
	if err := workflow.ExecuteActivity(sctx, acts.UpdateBase, BaseUpdateRequest{Item: w.in.Item, Action: a}).Get(ctx, &rep); err != nil {
		return nil
	}
	return rep.Event()
}

// checkMerge reads the merge facts, after one poll interval when a gate
// had no result yet (TECHNICAL-SPEC §8.1 cause 6).
func (w *itemWF) checkMerge(ctx workflow.Context, id string, a core.CheckMerge) core.Event {
	if a.Recheck && workflow.Sleep(ctx, w.pollInterval()) != nil {
		return nil
	}
	if _, ok := w.admit(ctx, id, false); !ok {
		return nil
	}
	var pusher core.Pusher
	if w.s.Change != nil {
		pusher = w.s.Change.Pusher
	}
	var mc core.MergeChecked
	if err := workflow.ExecuteActivity(w.coreCtx(ctx), acts.CheckMerge, MergeCheckRequest{Action: a, Pusher: pusher}).Get(ctx, &mc); err != nil {
		return nil
	}
	return mc
}

// merge merges the change, pinned to the reviewed commit (FR-7 AC5).
func (w *itemWF) merge(ctx workflow.Context, a core.Merge) core.Event {
	if _, ok := w.admit(ctx, a.Key, false); !ok {
		return nil
	}
	var mr core.MergeResult
	if err := workflow.ExecuteActivity(w.coreCtx(ctx), acts.Merge, a).Get(ctx, &mr); err != nil {
		return nil
	}
	return mr
}
