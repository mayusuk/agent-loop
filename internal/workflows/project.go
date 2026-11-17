package workflows

import (
	"errors"
	"fmt"
	"slices"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mayusuk/agent-loop/internal/core"
)

// ProjectConfig is a project's supervisor settings from the profile
// (TECHNICAL-SPEC §6.2, §13).
type ProjectConfig struct {
	Name string
	Item ItemSettings
	Pick core.PickPolicy
	// DailyRuns is limits.daily_agent_runs (zero is the default, 20) and
	// DailySpend limits.daily_spend (zero is no spend limit), FR-16 AC3.
	DailyRuns  int
	DailySpend float64
	Timezone   string
	// PollInterval and SupervisorInterval: zero is 60 s and 10 minutes.
	PollInterval       time.Duration
	SupervisorInterval time.Duration
}

// RunningItem is an item with a running ItemWorkflow.
type RunningItem struct {
	WorkflowID string
	Claim      int
}

// Spend is the day's reported handler spend (FR-16 AC3).
type Spend struct {
	Day    string
	Amount float64
}

// ProjectState is what ProjectWorkflow carries across continue-as-new
// (TECHNICAL-SPEC §6.2).
type ProjectState struct {
	Cursors Cursors
	Daily   core.DailyCount
	Spend   Spend
	Items   map[string]RunningItem
	// Waiting are the items refused at the daily limit, which the day
	// boundary wakes (FR-16 AC4).
	Waiting map[string]string
	// Held are admission requests held by a pause (work plan D5).
	Held             []AdmitRequest
	Paused           bool
	DeploymentPaused bool
	// LastPick is the last pick's verdicts, for the board and dry runs.
	LastPick []core.Verdict
}

// ProjectInput starts a ProjectWorkflow; State is nil on the first start.
type ProjectInput struct {
	Config ProjectConfig
	State  *ProjectState `json:",omitempty"`
}

// ProjectView is the project query's answer.
type ProjectView struct {
	State             ProjectState
	DailyLimitReached bool
}

type projectWF struct {
	ctx workflow.Context
	cfg ProjectConfig
	st  ProjectState
	loc *time.Location
}

func (p *projectWF) coreCtx() workflow.Context {
	return workflow.WithActivityOptions(p.ctx, workflow.ActivityOptions{
		TaskQueue:           CoreQueue,
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute},
	})
}

// ProjectWorkflow is a project's supervisor (TECHNICAL-SPEC §6.2): every
// poll interval it routes the change feeds to the item workflows and picks;
// every supervisor interval it reconciles and continues as new. It holds
// WIP, the daily agent-run count and spend, the day-boundary timer, the
// project pause, and the admission of every stage start.
func ProjectWorkflow(ctx workflow.Context, in ProjectInput) error {
	p := &projectWF{ctx: ctx, cfg: in.Config}
	loc, err := location(in.Config.Timezone)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("bad time zone", "config", err)
	}
	p.loc = loc
	if in.State != nil {
		p.st = *in.State
	} else {
		// A fresh start rebuilds the day's count from stage_runs
		// (TECHNICAL-SPEC §10).
		now := workflow.Now(ctx)
		var n int
		if err := workflow.ExecuteActivity(p.coreCtx(), acts.DailyRuns, DailyRequest{Day: now, Timezone: in.Config.Timezone}).Get(ctx, &n); err != nil {
			return err
		}
		p.st.Daily = core.DailyCount{Day: dayOf(now, loc), Runs: n}
		p.register()
	}
	if p.st.Items == nil {
		p.st.Items = map[string]RunningItem{}
	}
	if p.st.Waiting == nil {
		p.st.Waiting = map[string]string{}
	}
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (ProjectView, error) {
		return ProjectView{State: p.st, DailyLimitReached: p.limitReached(workflow.Now(ctx))}, nil
	}); err != nil {
		return err
	}

	admit := workflow.GetSignalChannel(ctx, SignalAdmit)
	spent := workflow.GetSignalChannel(ctx, SignalSpent)
	ended := workflow.GetSignalChannel(ctx, SignalItemEnded)
	pause := workflow.GetSignalChannel(ctx, SignalPause)
	dpause := workflow.GetSignalChannel(ctx, SignalDeploymentPause)
	handle := func(c workflow.ReceiveChannel, async bool) bool {
		switch c {
		case admit:
			var r AdmitRequest
			if !recv(ctx, c, &r, async) {
				return false
			}
			p.admit(r)
		case spent:
			var s SpendReport
			if !recv(ctx, c, &s, async) {
				return false
			}
			p.spend(s)
		case ended:
			var e ItemEnded
			if !recv(ctx, c, &e, async) {
				return false
			}
			if cur, ok := p.st.Items[e.Item]; ok && cur.WorkflowID == e.WorkflowID {
				delete(p.st.Items, e.Item)
			}
			delete(p.st.Waiting, e.Item)
		case pause, dpause:
			var s PauseSignal
			if !recv(ctx, c, &s, async) {
				return false
			}
			was := p.paused()
			if c == pause {
				p.st.Paused = s.Paused
			} else {
				p.st.DeploymentPaused = s.Paused
			}
			if was && !p.paused() {
				p.resumed()
			}
		}
		return true
	}
	channels := []workflow.ReceiveChannel{admit, spent, ended, pause, dpause}

	pollEvery := orDuration(in.Config.PollInterval, DefaultPollInterval)
	superEvery := orDuration(in.Config.SupervisorInterval, DefaultSupervisorInterval)
	tctx, cancelTimers := workflow.WithCancel(ctx)
	defer cancelTimers()
	poll := workflow.NewTimer(tctx, pollEvery)
	super := workflow.NewTimer(tctx, superEvery)
	day := workflow.NewTimer(tctx, core.NextDayBoundary(workflow.Now(ctx), loc).Sub(workflow.Now(ctx)))
	if in.State == nil {
		p.pass(false)
	}
	for {
		sel := workflow.NewSelector(ctx)
		for _, c := range channels {
			sel.AddReceive(c, func(c workflow.ReceiveChannel, _ bool) { handle(c, false) })
		}
		reconcile := false
		sel.AddFuture(poll, func(workflow.Future) {
			p.pass(false)
			poll = workflow.NewTimer(tctx, pollEvery)
		})
		sel.AddFuture(super, func(workflow.Future) { reconcile = true })
		sel.AddFuture(day, func(workflow.Future) {
			p.dayBoundary()
			now := workflow.Now(ctx)
			day = workflow.NewTimer(tctx, core.NextDayBoundary(now, loc).Sub(now))
		})
		sel.Select(ctx)
		if reconcile || workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			if reconcile {
				p.pass(true)
			}
			// Everything signalled so far is handled before the state
			// moves to the new run.
			for _, c := range channels {
				for handle(c, true) {
				}
			}
			cancelTimers()
			st := p.st
			return workflow.NewContinueAsNewError(ctx, ProjectWorkflow, ProjectInput{Config: in.Config, State: &st})
		}
	}
}

func recv(ctx workflow.Context, c workflow.ReceiveChannel, v any, async bool) bool {
	if async {
		return c.ReceiveAsync(v)
	}
	c.Receive(ctx, v)
	return true
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func dayOf(t time.Time, loc *time.Location) string { return t.In(loc).Format(time.DateOnly) }

// register asks the deployment for its pause and every later change (work
// plan D6). A deployment that isn't running leaves the project unpaused.
func (p *projectWF) register() {
	ctx := p.ctx
	err := workflow.SignalExternalWorkflow(ctx, DeploymentID, "", SignalRegister, RegisterSignal{WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID}).Get(ctx, nil)
	if err != nil {
		workflow.GetLogger(ctx).Warn("deployment workflow not reached", "error", err)
	}
}

func (p *projectWF) paused() bool { return p.st.Paused || p.st.DeploymentPaused }

// roll starts a new day's count and spend when now is on a later day.
func (p *projectWF) roll(now time.Time) {
	d := dayOf(now, p.loc)
	if p.st.Daily.Day != d {
		p.st.Daily = core.DailyCount{Day: d}
	}
	if p.st.Spend.Day != d {
		p.st.Spend = Spend{Day: d}
	}
}

// spendReached: the profile sets a spend limit and the day's reported spend
// reached it (FR-16 AC3, INTEGRATION-SPEC §5.1).
func (p *projectWF) spendReached(now time.Time) bool {
	p.roll(now)
	return p.cfg.DailySpend > 0 && p.st.Spend.Amount >= p.cfg.DailySpend
}

func (p *projectWF) limitReached(now time.Time) bool {
	return p.st.Daily.Reached(now, p.loc, p.cfg.DailyRuns) || p.spendReached(now)
}

// admit answers an admission request (TECHNICAL-SPEC §6.3, FR-16 AC3). A
// pause holds it; an agent stage at the run or spend limit waits, and the
// owner is told once a day.
func (p *projectWF) admit(r AdmitRequest) {
	now := workflow.Now(p.ctx)
	if p.paused() {
		if !slices.ContainsFunc(p.st.Held, func(h AdmitRequest) bool { return h.RunID == r.RunID }) {
			p.st.Held = append(p.st.Held, r)
		}
		return
	}
	var adm core.Admission
	var notify bool
	if r.Agent && p.spendReached(now) {
		adm, notify = core.AdmitWaitingLimit, !p.st.Daily.Notified
		p.st.Daily.Notified = true
	} else {
		p.st.Daily, adm, notify = core.Admit(p.st.Daily, core.AdmitRequest{Now: now, Agent: r.Agent, Limit: p.cfg.DailyRuns, Loc: p.loc})
	}
	if adm == core.AdmitWaitingLimit {
		p.st.Waiting[r.Item] = r.WorkflowID
	}
	if notify {
		p.notify(fmt.Sprintf("%s:daily_limit:%s", ProjectID(p.cfg.Name), p.st.Daily.Day), "daily_limit",
			"The daily limit is reached: no new agent stage starts until 00:00.")
	}
	ctx := p.ctx
	if err := workflow.SignalExternalWorkflow(ctx, r.WorkflowID, "", SignalAdmission, AdmissionReply{RunID: r.RunID, Admission: adm}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("admission not delivered", "item", r.Item, "error", err)
	}
}

// spend adds a run's reported cost to the day's spend (FR-16 AC3).
func (p *projectWF) spend(s SpendReport) {
	p.roll(workflow.Now(p.ctx))
	p.st.Spend.Amount += s.Cost
}

// resumed: the project and the deployment are both unpaused. The held
// admissions are answered, and every item hears ProjectResumed, which gives
// a merging item a new merge time limit (#12 D12).
func (p *projectWF) resumed() {
	held := p.st.Held
	p.st.Held = nil
	for _, r := range held {
		p.admit(r)
	}
	p.broadcast(MustSignal(core.ProjectResumed{At: core.At{Time: workflow.Now(p.ctx)}}), p.itemIDs())
}

// dayBoundary resets the day's count and spend at 00:00 in the profile's
// time zone, wakes every item that waited for the limit, and tells the
// owner (FR-16 AC4).
func (p *projectWF) dayBoundary() {
	now := workflow.Now(p.ctx)
	next, _, notify := core.DayReset(p.st.Daily, now, p.loc)
	p.st.Daily = next
	p.st.Spend = Spend{Day: next.Day}
	waiting := p.st.Waiting
	p.st.Waiting = map[string]string{}
	if len(waiting) == 0 && !notify {
		return
	}
	var ids []string
	for item := range waiting {
		if _, ok := p.st.Items[item]; ok {
			ids = append(ids, item)
		}
	}
	slices.Sort(ids)
	p.broadcast(MustSignal(core.DayBoundary{At: core.At{Time: now}}), ids)
	p.notify(fmt.Sprintf("%s:daily_reset:%s", ProjectID(p.cfg.Name), next.Day), "daily_reset",
		"The daily limit was reset: waiting items continue.")
}

func (p *projectWF) notify(key, reason, msg string) {
	ctx := p.coreCtx()
	if err := workflow.ExecuteActivity(ctx, acts.NotifyProject, ProjectNotice{Key: key, Reason: reason, Message: msg}).Get(ctx, nil); err != nil {
		workflow.GetLogger(p.ctx).Warn("notification failed", "error", err)
	}
}

// itemIDs are the running items, sorted so every replay signals in one
// order.
func (p *projectWF) itemIDs() []string {
	var ids []string
	for item := range p.st.Items {
		ids = append(ids, item)
	}
	slices.Sort(ids)
	return ids
}

func (p *projectWF) broadcast(s ItemSignal, items []string) {
	for _, item := range items {
		p.signal(item, s)
	}
}

func (p *projectWF) signal(item string, s ItemSignal) {
	ri, ok := p.st.Items[item]
	if !ok {
		return
	}
	ctx := p.ctx
	if err := workflow.SignalExternalWorkflow(ctx, ri.WorkflowID, "", SignalEvent, s).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("item not signalled", "item", item, "error", err)
	}
}

// pass is one supervisor pass (TECHNICAL-SPEC §6.2): poll and route, on a
// full pass reconcile, then pick and claim.
func (p *projectWF) pass(full bool) {
	ctx := p.coreCtx()
	var rep PollReport
	if err := workflow.ExecuteActivity(ctx, acts.Poll, PollRequest{Cursors: p.st.Cursors, Items: p.itemIDs()}).Get(ctx, &rep); err != nil {
		workflow.GetLogger(p.ctx).Error("poll failed", "error", err)
	} else {
		p.st.Cursors = rep.Cursors
		for _, r := range rep.Events {
			p.signal(r.Item, r.Signal)
		}
	}
	if full {
		var closed []Routed
		if err := workflow.ExecuteActivity(ctx, acts.Reconcile, p.itemIDs()).Get(ctx, &closed); err == nil {
			for _, r := range closed {
				p.signal(r.Item, r.Signal)
			}
		}
	}
	p.pick()
}

// pick claims candidates while WIP, the daily limit and the pauses allow
// (FR-2 AC1–AC3, AC6), then starts each claimed item's workflow.
func (p *projectWF) pick() {
	ctx := p.coreCtx()
	var cands []core.Candidate
	if err := workflow.ExecuteActivity(ctx, acts.Candidates).Get(ctx, &cands); err != nil {
		workflow.GetLogger(p.ctx).Error("candidates failed", "error", err)
		return
	}
	now := workflow.Now(p.ctx)
	view := core.ProjectView{
		Paused: p.st.Paused, DeploymentPaused: p.st.DeploymentPaused,
		DailyLimitReached: p.limitReached(now), Claimed: len(p.st.Items),
	}
	res := core.Pick(p.cfg.Name, cands, view, p.cfg.Pick)
	p.st.LastPick = res.Verdicts
	for _, e := range res.Escalate {
		p.logErr(workflow.ExecuteActivity(ctx, acts.EscalateDependency, e).Get(ctx, nil))
	}
	for _, item := range res.Picks {
		if _, ok := p.st.Items[item]; ok {
			continue
		}
		var plan ClaimPlanReport
		if err := workflow.ExecuteActivity(ctx, acts.PlanClaim, item).Get(ctx, &plan); err != nil {
			p.logErr(err)
			continue
		}
		var claimed bool
		if err := workflow.ExecuteActivity(ctx, acts.Claim, plan).Get(ctx, &claimed); err != nil || !claimed {
			p.logErr(err)
			continue
		}
		if err := p.startItem(plan); err != nil {
			p.logErr(err)
		}
	}
}

func (p *projectWF) logErr(err error) {
	if err != nil {
		workflow.GetLogger(p.ctx).Error("supervisor step failed", "error", err)
	}
}

// ErrItemRunning is the start of an item whose workflow is running: the
// workflow ID is the item's lock (FR-2 AC4).
var ErrItemRunning = errors.New("workflows: the item's workflow is already running")

// startItem starts the claimed item's workflow with ID item/<project>/<item>
// (TECHNICAL-SPEC §6.1): a running workflow with that ID refuses the start,
// and a closed one doesn't. The item workflow outlives the project's run
// (continue-as-new), so it is abandoned, not cancelled, when that run ends.
func (p *projectWF) startItem(plan ClaimPlanReport) error {
	in := ItemInput{
		Settings: p.cfg.Item, WorkflowID: plan.Plan.WorkflowID, Item: plan.Plan.Item,
		ItemURL: plan.URL, ItemType: plan.ItemType, Claim: plan.Plan.Claim,
	}
	in.Settings.Project = p.cfg.Name
	cctx := workflow.WithChildOptions(p.ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            plan.Plan.WorkflowID,
		TaskQueue:             CoreQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		ParentClosePolicy:     enumspb.PARENT_CLOSE_POLICY_ABANDON,
	})
	f := workflow.ExecuteChildWorkflow(cctx, ItemWorkflow, in)
	if err := f.GetChildWorkflowExecution().Get(p.ctx, nil); err != nil {
		var started *temporal.ChildWorkflowExecutionAlreadyStartedError
		var running *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &started) || errors.As(err, &running) {
			return fmt.Errorf("%w: %s", ErrItemRunning, plan.Plan.WorkflowID)
		}
		return err
	}
	p.st.Items[plan.Plan.Item] = RunningItem{WorkflowID: plan.Plan.WorkflowID, Claim: plan.Plan.Claim}
	workflow.Go(p.ctx, func(ctx workflow.Context) {
		if err := f.Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("item workflow failed", "item", plan.Plan.Item, "error", err)
		}
	})
	return nil
}
