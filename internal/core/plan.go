package core

import (
	"fmt"
	"slices"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// The planning states (PRODUCT-SPEC §6). StateMaterialised is a plan whose
// items were all created (work plan #155 D1).
const (
	StateMinting              State = "minting"
	StatePlanProposed         State = "plan_proposed"
	StatePlanRejected         State = "plan_rejected"
	StateMaterialising        State = "materialising"
	StateMaterialised         State = "materialised"
	StateMintEscalated        State = "mint_escalated"
	StateMaterialiseEscalated State = "materialise_escalated"
	StatePlanAbandoned        State = "plan_abandoned"
	StateComplete             State = "complete"
)

// StageMaterialise is the deterministic materialise stage. It has no
// handler, so it is not one of the contract's stages (FR-17 AC5).
const StageMaterialise contract.Stage = "materialise"

// Planning escalation reasons, beside the item reasons a run can also give
// (needs_owner, retries_exhausted, time_limit, worker_lost; work plan #155
// D3).
const (
	ReasonInvalidTaskList  = "invalid_task_list"
	ReasonPlanNotApproved  = "plan_not_approved"
	ReasonMergedByNonOwner = "merged_by_non_owner"
)

// EventComplete is the notification that minting found nothing left to
// plan (FR-9 AC5, work plan #155 D13).
const EventComplete contract.StatusEvent = "complete"

// EventMintRefused is the notification that the daily limit refused a mint
// run, so the mint ended (FR-9 AC1, D5, work plan #155 D10).
const EventMintRefused contract.StatusEvent = "mint_refused"

// The RecordPlan events (FR-12 AC4, FR-17 AC9, work plan #155 D12).
const (
	PlanRecordRejected  = "rejected"
	PlanRecordAbandoned = "abandoned"
)

// PlanState is everything the core knows about one plan, from the owner's
// mint to its end (PRODUCT-SPEC §6). It holds references only, never plan
// or tracker text. DecidePlan never changes the value it is given.
type PlanState struct {
	State State

	// WorkflowID and MintID make run IDs and idempotency keys. MintID is
	// the provisional mint-<n> (INTEGRATION-SPEC §5.2, work plan #155 D14).
	WorkflowID string
	MintID     string

	// PlanKey and Proposal come from mint's done result (FR-9 AC7).
	PlanKey  string
	Proposal string

	// Run is the current mint or materialise run, nil when none is.
	Run *PlanRun
	// Refused is why the last mint ended without running: the daily limit
	// refused it. It never restarts by itself; the owner runs mint again
	// (FR-9 AC1, D5, work plan #155 D10).
	Refused *PlanRefusal

	Escalation *PlanEscalation

	// Items and Tracking are what materialise created (FR-17 AC2, AC7).
	Items    []string
	Tracking string

	// Seq counts decisions; Starts counts run starts (run_id suffix).
	Seq    int
	Starts int
}

// PlanRun is one mint or materialise run.
type PlanRun struct {
	Stage    contract.Stage
	Attempt  int
	Crashes  int
	RunID    string
	Deadline time.Time
}

// PlanRefusal is a mint run refused at admission: the reason and the
// attempt it would have been (work plan #155 D10).
type PlanRefusal struct {
	Reason  string
	Attempt int
}

// PlanEscalation is why a plan waits for the owner. Merge names the change
// a non-owner account merged (FR-12 AC4, work plan #155 D6).
type PlanEscalation struct {
	Reason string
	Stage  contract.Stage
	Merge  string
}

// Clone returns a deep copy, so DecidePlan never aliases its input.
func (s PlanState) Clone() PlanState {
	c := s
	if s.Run != nil {
		r := *s.Run
		c.Run = &r
	}
	if s.Escalation != nil {
		e := *s.Escalation
		c.Escalation = &e
	}
	if s.Refused != nil {
		f := *s.Refused
		c.Refused = &f
	}
	c.Items = slices.Clone(s.Items)
	return c
}

// MintRequested: the owner ran `agent-loop mint` and MintAllowed allowed
// it (FR-9 AC1). It starts a plan, or mints again from mint_escalated or
// complete (§6, work plan #155 D8).
type MintRequested struct {
	At
	WorkflowID string
	MintID     string
}

// MintOutcome is a done mint: the proposal the worker opened and the plan
// key the minter chose (FR-9 AC7, INTEGRATION-SPEC §5.2).
type MintOutcome struct {
	Proposal string
	PlanKey  string
}

// MaterialiseResult is the end of a materialise run (FR-17 AC7). Invalid
// says the task list failed validation, and NotApproved names the latest
// change to the plan when the owner didn't merge it; both create nothing
// (FR-17 AC6, AC8). The workflow names the problems in its own comment.
type MaterialiseResult struct {
	At
	RunID       string
	Outcome     contract.Outcome
	Items       []string
	Tracking    string
	Invalid     bool
	NotApproved string
	Reason      string
}

// PlanFixMerged: a change to the plan was merged on the base branch
// (FR-17 AC8, FR-12 AC4).
type PlanFixMerged struct {
	At
	By     Role
	Change string
}

// PlanAbandoned: `agent-loop plan abandon <plan>` (FR-17 AC9). API
// commands act as the owner.
type PlanAbandoned struct{ At }

func (MintRequested) Name() string     { return "mint_requested" }
func (MaterialiseResult) Name() string { return "materialise_result" }
func (PlanFixMerged) Name() string     { return "plan_fix_merged" }
func (PlanAbandoned) Name() string     { return "plan_abandoned" }

// Materialise runs the deterministic materialise stage as the loop
// identity (FR-17). Its outcome comes back as MaterialiseResult.
type Materialise struct {
	Key      string
	RunID    string
	PlanKey  string
	Proposal string
	Attempt  int
	Deadline time.Time
}

// MarkProposal sets or clears a marker on the plan proposal (FR-10 AC1,
// §5.1: on a proposal needs-owner is informational).
type MarkProposal struct {
	Key      string
	Proposal string
	Marker   string
	On       bool
}

// RecordPlan records the plan as rejected or abandoned on its proposal or
// tracking item (FR-12 AC4, FR-17 AC9, work plan #155 D12).
type RecordPlan struct {
	Key      string
	Event    string
	Proposal string
	Tracking string
}

// EndPlan says the plan reached an end state, so its workflow can end.
// Reason is the state, or mint_refused for a mint the daily limit refused
// (work plan #155 D10).
type EndPlan struct{ Reason string }

func (Materialise) Kind() string  { return "materialise" }
func (MarkProposal) Kind() string { return "mark_proposal" }
func (RecordPlan) Kind() string   { return "record_plan" }
func (EndPlan) Kind() string      { return "end_plan" }

func (Materialise) isAction()  {}
func (MarkProposal) isAction() {}
func (RecordPlan) isAction()   {}
func (EndPlan) isAction()      {}

// PlanBasis is the plan state a planning decision saw (work plan #155 D2).
type PlanBasis struct {
	Proposal string
	PlanKey  string
	Attempt  int
}

// DecidePlan decides one event for one plan (PRODUCT-SPEC §6). Like
// Decide it is deterministic, never changes s, reads no clock but the
// event's, and always ends with the decision record (NFR-5 AC1).
func DecidePlan(s PlanState, e Event, p Policy) (PlanState, []Action) {
	d := &planDecision{s: s.Clone(), p: p, at: e.When(), rule: "ignored"}
	d.s.Seq++
	d.route(e)
	b := &PlanBasis{Proposal: s.Proposal, PlanKey: s.PlanKey}
	if s.Run != nil {
		b.Attempt = s.Run.Attempt
	}
	rec := DecisionRecord{Seq: d.s.Seq, At: d.at, Event: e.Name(), From: s.State, To: d.s.State,
		Result: d.result, Rule: d.rule, Plan: b}
	for _, a := range d.actions {
		rec.Actions = append(rec.Actions, a.Kind())
	}
	return d.s, append(d.actions, RecordDecision{Record: rec})
}

type planDecision struct {
	s       PlanState
	p       Policy
	at      time.Time
	actions []Action
	rule    string
	result  *ResultRef
}

func (d *planDecision) emit(a ...Action) { d.actions = append(d.actions, a...) }

// key is <workflow id>#<mint id>:<verb>:<seq> (TECHNICAL-SPEC §9, work
// plan #155 D14).
func (d *planDecision) key(verb string) string {
	return fmt.Sprintf("%s#%s:%s:%d", d.s.WorkflowID, d.s.MintID, verb, d.s.Seq)
}

func (d *planDecision) route(e Event) {
	switch e := e.(type) {
	case MintRequested:
		d.mintRequested(e)
	case StageResult:
		d.mintResult(e)
	case MaterialiseResult:
		d.materialiseResult(e)
	case StageCrashed:
		d.crashed(e)
	case StageTimedOut:
		if r := d.current(e.RunID); r != nil {
			d.escalateRun(ReasonTimeLimit, "FR-16 AC1 time limit")
		}
	case DailyLimitReached:
		d.dailyLimit(e)
	case ChangeMerged:
		d.proposalMerged(e)
	case ChangeClosed:
		d.proposalClosed(e)
	case MarkerChanged:
		if e.OnChange && e.Marker == MarkerNeedsOwner && !e.Added {
			d.rule = "FR-12 AC4 needs-owner removed from a plan proposal: ignored"
		}
	case PlanFixMerged:
		d.fixMerged(e)
	case PlanAbandoned:
		d.abandoned()
	}
}

// mintRequested starts a mint (FR-9 AC1, §6 `[*] --> minting`,
// `mint_escalated --> minting`, `complete --> minting`).
func (d *planDecision) mintRequested(e MintRequested) {
	switch d.s.State {
	case StateNone, StateMintEscalated, StateComplete:
	default:
		return
	}
	if e.WorkflowID != "" {
		d.s.WorkflowID = e.WorkflowID
	}
	if e.MintID != "" {
		d.s.MintID = e.MintID
	}
	d.s.Escalation, d.s.Refused = nil, nil
	d.start(contract.StageMint, 1, 0)
	d.rule = "FR-9 AC1 owner runs mint"
}

// start starts a mint or materialise run with a new run ID and time limit.
func (d *planDecision) start(st contract.Stage, attempt, crashes int) {
	d.s.Starts++
	r := PlanRun{Stage: st, Attempt: attempt, Crashes: crashes,
		RunID:    fmt.Sprintf("%s#%s:%s:%d:%d", d.s.WorkflowID, d.s.MintID, st, attempt, d.s.Starts),
		Deadline: d.at.Add(d.p.limits(st).TimeLimit)}
	d.s.Run = &r
	if st == contract.StageMint {
		d.s.State = StateMinting
		d.emit(RunStage{RunID: r.RunID, Stage: st, Attempt: attempt, Deadline: r.Deadline})
		return
	}
	d.s.State = StateMaterialising
	d.emit(Materialise{Key: d.key("materialise"), RunID: r.RunID, PlanKey: d.s.PlanKey, Proposal: d.s.Proposal,
		Attempt: attempt, Deadline: r.Deadline})
}

// current returns the running run when id names it.
func (d *planDecision) current(id string) *PlanRun {
	r := d.s.Run
	if r == nil || r.RunID != id {
		return nil
	}
	if (d.s.State == StateMinting && r.Stage == contract.StageMint) ||
		(d.s.State == StateMaterialising && r.Stage == StageMaterialise) {
		return r
	}
	return nil
}

func (d *planDecision) mintResult(e StageResult) {
	r := d.current(e.RunID)
	if r == nil || r.Stage != contract.StageMint {
		return
	}
	d.result = &ResultRef{Stage: r.Stage, RunID: r.RunID, Outcome: e.Outcome}
	if e.Stage != r.Stage || !contract.Allowed(r.Stage, e.Outcome) {
		d.failed("FR-18 AC3 result not allowed for mint: failed")
		return
	}
	switch e.Outcome {
	case contract.OutcomeFailed:
		d.failed("FR-18 AC1 failed")
	case contract.OutcomeNeedsOwner:
		d.escalateRun(ReasonNeedsOwner, "FR-10 AC4 needs_owner")
	case contract.OutcomeComplete:
		d.s.State, d.s.Run = StateComplete, nil
		d.emit(Notify{Key: d.key("notify"), Event: EventComplete}, EndPlan{Reason: string(StateComplete)})
		d.rule = "FR-9 AC5 nothing left to plan: complete"
	case contract.OutcomeDone:
		if e.Mint == nil || e.Mint.Proposal == "" || e.Mint.PlanKey == "" {
			d.failed("FR-18 AC3 done without a proposal: failed")
			return
		}
		d.s.State, d.s.Run = StatePlanProposed, nil
		d.s.Proposal, d.s.PlanKey = e.Mint.Proposal, e.Mint.PlanKey
		d.emit(Wait{Reason: "plan proposed: waiting for the owner"})
		d.rule = "FR-9 AC3 plan proposal opened"
	}
}

func (d *planDecision) materialiseResult(e MaterialiseResult) {
	r := d.current(e.RunID)
	if r == nil || r.Stage != StageMaterialise {
		return
	}
	d.result = &ResultRef{Stage: r.Stage, RunID: r.RunID, Outcome: e.Outcome}
	// FR-17 AC8: an unapproved plan is never read, so no task list was
	// validated and plan_not_approved wins over invalid_task_list.
	switch {
	case e.NotApproved != "":
		d.escalateRun(ReasonPlanNotApproved, "FR-17 AC8 latest change to the plan not merged by the owner")
		d.s.Escalation.Merge = e.NotApproved
	case e.Invalid:
		d.escalateRun(ReasonInvalidTaskList, "FR-17 AC6 invalid task list: nothing created")
	case e.Outcome == contract.OutcomeDone:
		d.s.State, d.s.Run = StateMaterialised, nil
		d.s.Items, d.s.Tracking = slices.Clone(e.Items), e.Tracking
		d.emit(EndPlan{Reason: string(StateMaterialised)})
		d.rule = "FR-17 AC2 items created"
	default:
		d.failed("FR-17 AC7 failed")
	}
}

// failed runs the stage again while attempts remain, then escalates
// (FR-16 AC1, FR-17 AC7).
func (d *planDecision) failed(rule string) {
	r := d.s.Run
	if r.Attempt < d.p.limits(r.Stage).Attempts {
		d.start(r.Stage, r.Attempt+1, 0)
		d.rule = rule + ": retry (FR-16 AC1)"
		return
	}
	d.escalateRun(ReasonRetriesExhausted, rule+": retries used up (FR-16 AC1)")
}

// crashed reruns the same attempt; the 4th crash of one attempt escalates
// (FR-16 AC1, D42).
func (d *planDecision) crashed(e StageCrashed) {
	r := d.current(e.RunID)
	if r == nil {
		return
	}
	if r.Crashes >= MaxCrashReruns {
		d.escalateRun(ReasonWorkerLost, "FR-16 AC1 fourth crash of one attempt (D42)")
		return
	}
	d.start(r.Stage, r.Attempt, r.Crashes+1)
	d.rule = "FR-16 AC1 crash rerun with the same attempt (D42)"
}

// dailyLimit: the daily limit refused the mint run at admission. The mint
// ends refused, with its reason recorded and the owner notified; it never
// waits and never restarts by itself, so events for the refused run are
// ignored and the owner runs mint again (FR-9 AC1, D5, work plan #155 D10).
func (d *planDecision) dailyLimit(e DailyLimitReached) {
	r := d.current(e.RunID)
	if r == nil || r.Stage != contract.StageMint {
		return
	}
	reason := string(ReasonDailyLimit)
	d.s.State, d.s.Run, d.s.Escalation = StateNone, nil, nil
	d.s.Refused = &PlanRefusal{Reason: reason, Attempt: r.Attempt}
	d.emit(Notify{Key: d.key("notify"), Event: EventMintRefused, Reason: reason},
		EndPlan{Reason: string(EventMintRefused)})
	d.rule = "FR-9 AC1 daily limit reached: mint refused, the owner runs mint again (D5, work plan #155 D10)"
}

// escalateRun escalates from the running stage: mint to mint_escalated,
// materialise to materialise_escalated.
func (d *planDecision) escalateRun(reason, rule string) {
	to := StateMintEscalated
	if d.s.State == StateMaterialising {
		to = StateMaterialiseEscalated
	}
	d.escalate(to, PlanEscalation{Reason: reason, Stage: d.s.Run.Stage}, rule)
}

// escalate sends the plan to the owner: it marks the proposal when there
// is one, and always notifies (FR-10 AC1, work plan #155 D11).
func (d *planDecision) escalate(to State, e PlanEscalation, rule string) {
	d.s.State, d.s.Run = to, nil
	d.s.Escalation = &e
	if d.s.Proposal != "" {
		d.emit(d.mark(true))
	}
	d.emit(Notify{Key: d.key("notify"), Event: contract.EventEscalate, Reason: e.Reason},
		Wait{Reason: "escalated: " + e.Reason})
	d.rule = rule
}

func (d *planDecision) mark(on bool) MarkProposal {
	return MarkProposal{Key: d.key(fmt.Sprintf("marker:%s:%t", MarkerNeedsOwner, on)), Proposal: d.s.Proposal,
		Marker: MarkerNeedsOwner, On: on}
}

// proposalMerged: only an owner merge approves the plan (FR-13 AC7,
// FR-12 AC4, work plan #155 D6).
func (d *planDecision) proposalMerged(e ChangeMerged) {
	if d.s.State != StatePlanProposed || e.Change != d.s.Proposal {
		return
	}
	if e.By != RoleOwner {
		d.escalate(StateMaterialiseEscalated, PlanEscalation{Reason: ReasonMergedByNonOwner, Stage: StageMaterialise, Merge: e.Change},
			"FR-12 AC4 proposal merged by a non-owner account: recorded, no materialise")
		return
	}
	d.start(StageMaterialise, 1, 0)
	d.rule = "FR-17 AC1 owner merged the proposal: materialise"
}

// proposalClosed: a close without merging by any human (an owner or
// another account) rejects the plan; a close by one of the three
// identities is not a human's and is ignored (FR-12 AC4, work plan #155
// D5).
func (d *planDecision) proposalClosed(e ChangeClosed) {
	if d.s.State != StatePlanProposed || e.Change != d.s.Proposal {
		return
	}
	if e.By != RoleOwner && e.By != RoleOther {
		d.rule = "FR-12 AC4 proposal closed by an identity, not a human: ignored (work plan #155 D5)"
		return
	}
	d.s.State = StatePlanRejected
	d.emit(RecordPlan{Key: d.key("record:" + PlanRecordRejected), Event: PlanRecordRejected, Proposal: d.s.Proposal},
		EndPlan{Reason: string(StatePlanRejected)})
	d.rule = "FR-12 AC4 proposal closed without merging: rejected"
}

// fixMerged: an owner-merged change to a plan in materialise_escalated
// restarts materialise with a new attempt count and time limit; a
// non-owner merge escalates again; in any other state it starts nothing
// (FR-17 AC8, D33).
func (d *planDecision) fixMerged(e PlanFixMerged) {
	if d.s.State != StateMaterialiseEscalated {
		d.rule = "FR-17 AC8 merged change to a plan not in materialise_escalated: starts nothing (D33)"
		return
	}
	if e.By != RoleOwner {
		d.escalate(StateMaterialiseEscalated, PlanEscalation{Reason: ReasonMergedByNonOwner, Stage: StageMaterialise, Merge: e.Change},
			"FR-17 AC8 plan change merged by a non-owner account: escalated again")
		return
	}
	d.s.Escalation = nil
	if d.s.Proposal != "" {
		d.emit(d.mark(false))
	}
	d.start(StageMaterialise, 1, 0)
	d.rule = "FR-17 AC8 owner merged a plan fix: materialise restarts with a new attempt count"
}

// abandoned: the owner abandons a plan in materialise_escalated (FR-17 AC9).
func (d *planDecision) abandoned() {
	if d.s.State != StateMaterialiseEscalated {
		d.rule = "FR-17 AC9 abandon refused: the plan is not in materialise_escalated"
		return
	}
	d.s.State, d.s.Escalation = StatePlanAbandoned, nil
	if d.s.Proposal != "" {
		d.emit(d.mark(false))
	}
	d.emit(RecordPlan{Key: d.key("record:" + PlanRecordAbandoned), Event: PlanRecordAbandoned, Proposal: d.s.Proposal, Tracking: d.s.Tracking},
		EndPlan{Reason: string(StatePlanAbandoned)})
	d.rule = "FR-17 AC9 owner abandoned the plan"
}
