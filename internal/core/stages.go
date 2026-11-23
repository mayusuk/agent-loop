package core

import (
	"fmt"
	"slices"

	"github.com/mayusuk/agent-loop/contract"
)

// plan is a stage the core chose to run next.
type plan struct {
	stage contract.Stage
	kind  contract.ReviewKind  // review only
	mode  contract.AddressMode // address only
}

func planOf(r *Run) plan {
	p := plan{stage: r.Stage}
	if r.Review != nil {
		p.kind = r.Review.Kind
	}
	if r.Address != nil {
		p.mode = r.Address.Mode
	}
	return p
}

func stateFor(st contract.Stage) State {
	switch st {
	case contract.StageImplement:
		return StateImplementing
	case contract.StageReview:
		return StateInReview
	case contract.StageAddress:
		return StateAddressing
	}
	return StateMerging
}

// runID is <workflow id>#<claim>:<stage>:<attempt>:<start>, where start
// numbers every handler start of the claim, so each start, crash reruns
// included, has its own ID (INTEGRATION-SPEC §4.1, work plan D11).
func (d *decision) runID(st contract.Stage, attempt int) string {
	d.s.Starts++
	return fmt.Sprintf("%s#%d:%s:%d:%d", d.s.WorkflowID, d.s.Claim, st, attempt, d.s.Starts)
}

// start runs an agent stage after the first pre-stage check (TECHNICAL-SPEC
// §8.2): a counted round with the allowance used up is refused and
// escalated (FR-6 AC8). The second check, the daily limit, is the
// workflow's when it acts on RunStage (work plan D12).
func (d *decision) start(pl plan, attempt, crashes int) {
	r := Run{Stage: pl.stage, Attempt: attempt, Crashes: crashes}
	switch pl.stage {
	case contract.StageReview:
		// The chosen kind is kept for every rerun until a non-stale review
		// completes (work plan D25).
		d.s.NextKind = pl.kind
		if pl.kind == contract.KindRound && d.s.RoundsUsed >= d.s.Allowance(d.p) {
			d.escalate(Escalation{Reason: ReasonRoundLimit, Stage: contract.StageReview, CommentIDs: d.s.UnclearedManual()},
				"FR-6 AC8 counted round refused, round allowance used up")
			return
		}
		spec := &ReviewSpec{Kind: pl.kind, Commit: d.s.Change.Head, RuleFindings: d.s.UnresolvedFindings(), RuleManual: slices.Clone(d.s.PendingRulings)}
		if pl.kind == contract.KindRound {
			spec.Round = d.s.RoundsUsed + 1
		} else {
			spec.Seq = d.s.UncountedReviews
		}
		r.Review = spec
	case contract.StageAddress:
		spec := &AddressSpec{Mode: pl.mode}
		if pl.mode != contract.ModeBaseUpdate {
			spec.AnswerFindings = d.s.UnresolvedFindings()
			spec.AnswerManual = d.s.answerableManual()
		}
		r.Address = spec
		r.StartHead = d.s.Change.Head
	}
	r.RunID = d.runID(pl.stage, attempt)
	r.Deadline = d.at.Add(d.p.limits(pl.stage).TimeLimit)
	d.s.State = stateFor(pl.stage)
	d.s.Run = &r
	d.s.Merge = nil
	d.emit(d.runStageAction(&r))
}

// runStageAction is the RunStage for r, with copies of its inputs. Implement
// on an existing change and address ask for the pre-stage trust check
// (FR-13 AC5, work plan #11 D2).
func (d *decision) runStageAction(r *Run) RunStage {
	a := RunStage{RunID: r.RunID, Stage: r.Stage, Attempt: r.Attempt, Deadline: r.Deadline, Review: r.Review, Address: r.Address}
	if d.needsTrust(r.Stage) {
		a.Trust = &TrustCheck{Approved: slices.Clone(d.s.ApprovedCommits)}
	}
	if d.s.Change != nil {
		a.Change, a.Head = d.s.Change.Ref, d.s.Change.Head
	}
	if r.Stage == contract.StageImplement {
		a.Branch = d.s.ClosedChanges + 1
	}
	if a.Review != nil {
		rv := *a.Review
		rv.RuleFindings, rv.RuleManual = slices.Clone(rv.RuleFindings), slices.Clone(rv.RuleManual)
		a.Review = &rv
	}
	if a.Address != nil {
		ad := *a.Address
		ad.AnswerFindings, ad.AnswerManual = slices.Clone(ad.AnswerFindings), slices.Clone(ad.AnswerManual)
		a.Address = &ad
	}
	return a
}

// startAddress starts an address run in a mode.
func (d *decision) startAddress(mode contract.AddressMode) {
	d.start(plan{stage: contract.StageAddress, mode: mode}, 1, 0)
}

// beginBaseUpdate starts a base update (FR-5 AC6, D41) for merge cause 3.
// The agent-free merge comes first; only conflicts start an
// address run. It escalates when max_base_updates is used up (FR-6 AC7, D8).
func (d *decision) beginBaseUpdate() {
	if d.s.BaseUpdates >= d.p.maxBaseUpdates() {
		d.escalate(Escalation{Reason: ReasonBaseUpdateLimit, Stage: contract.StageAddress}, "FR-6 AC7 base-update limit used up")
		return
	}
	d.s.State = StateAddressing
	d.s.Run = nil
	d.s.Merge = nil
	d.s.UpdatingBase = true
	d.emit(UpdateBase{Key: d.key("base_update"), Change: d.s.Change.Ref, Head: d.s.Change.Head})
	d.rule = "FR-5 AC6 base update without an agent (D41)"
}

func (d *decision) baseUpdated(e BaseUpdated) {
	if !d.s.UpdatingBase {
		return
	}
	d.s.UpdatingBase = false
	d.s.Change.Head, d.s.Change.Pusher = e.Head, PusherCoder
	d.start(plan{stage: contract.StageReview, kind: contract.KindBaseUpdate}, 1, 0)
	d.rule = "FR-6 AC6 base_update review after a base update"
}

func (d *decision) baseConflicts() {
	if !d.s.UpdatingBase {
		return
	}
	d.s.UpdatingBase = false
	d.startAddress(contract.ModeBaseUpdate)
	d.rule = "FR-5 AC6 base merge has conflicts: address run in base_update mode"
}

func (d *decision) baseRefused() {
	if !d.s.UpdatingBase {
		return
	}
	d.s.UpdatingBase = false
	d.enterMerging()
	d.rule = "INTEGRATION-SPEC §5.3 head moved before the base merge: check merge again"
}

// current returns the running stage run when id names it.
func (d *decision) current(id string) *Run {
	if d.s.Run == nil || d.s.Run.RunID != id {
		return nil
	}
	switch d.s.State {
	case StateImplementing, StateInReview, StateAddressing:
		return d.s.Run
	}
	return nil
}

func (d *decision) stageResult(e StageResult) {
	r := d.current(e.RunID)
	if r == nil {
		return // not the running run (work plan D11, D23)
	}
	d.result = &ResultRef{Stage: r.Stage, RunID: r.RunID, Outcome: e.Outcome}
	d.freshManual(e.Manual)
	if r.Stage == contract.StageReview && d.unknownPushDuringReview() {
		return
	}
	if e.Stage != r.Stage || !contract.Allowed(r.Stage, e.Outcome) {
		d.failed("FR-18 AC3 result not allowed for the stage: failed")
		return
	}
	switch e.Outcome {
	case contract.OutcomeFailed:
		d.failed("FR-18 AC1 failed")
	case contract.OutcomeNeedsOwner:
		d.escalate(Escalation{Reason: ReasonNeedsOwner, Stage: r.Stage}, "FR-10 AC4 needs_owner")
	case contract.OutcomeBlocked:
		d.blocked(e.Blocked)
	case contract.OutcomeDone:
		switch r.Stage {
		case contract.StageImplement:
			d.implementDone(e.Implement)
		case contract.StageReview:
			d.reviewDone(e.Review)
		case contract.StageAddress:
			d.addressDone(e.Address)
		}
	}
}

// failed runs the same stage again while attempts remain, then escalates
// (FR-16 AC1, FR-18 AC1, work plan D9).
func (d *decision) failed(rule string) {
	r := d.s.Run
	if r.Attempt < d.p.limits(r.Stage).Attempts {
		d.start(planOf(r), r.Attempt+1, 0)
		if d.s.State != StateEscalated {
			d.rule = rule + ": retry (FR-16 AC1)"
		}
		return
	}
	d.escalate(Escalation{Reason: ReasonRetriesExhausted, Stage: r.Stage}, rule+": retries used up (FR-16 AC1)")
}

// crashed reruns the same attempt in a new session; the 4th crash of one
// attempt escalates (FR-16 AC1, D42, NFR-1 AC2, work plan D10).
func (d *decision) crashed(e StageCrashed) {
	r := d.current(e.RunID)
	if r == nil {
		return
	}
	if r.Crashes >= MaxCrashReruns {
		d.escalate(Escalation{Reason: ReasonWorkerLost, Stage: r.Stage}, "FR-16 AC1 fourth crash of one attempt (D42)")
		return
	}
	d.start(planOf(r), r.Attempt, r.Crashes+1)
	d.rule = "FR-16 AC1 crash rerun with the same attempt (D42)"
}

func (d *decision) timedOut(e StageTimedOut) {
	r := d.current(e.RunID)
	if r == nil {
		return
	}
	d.escalate(Escalation{Reason: ReasonTimeLimit, Stage: r.Stage}, "FR-18 AC4 time limit")
}

// dailyLimit moves the item to waiting_limit, keeping the refused run
// (FR-16 AC3, work plan D12).
func (d *decision) dailyLimit(e DailyLimitReached) {
	if d.current(e.RunID) == nil {
		return
	}
	d.s.State = StateWaitingLimit
	d.emit(Wait{Reason: "daily limit reached"})
	d.rule = "FR-16 AC3 daily limit reached before an agent stage"
}

func (d *decision) blocked(b *BlockedOutcome) {
	st := d.s.Run.Stage
	if b == nil || b.Dependency == "" {
		d.escalate(Escalation{Reason: ReasonBlockedNoDependency, Stage: st}, "FR-18 AC1 blocked with no dependency")
		return
	}
	switch b.State {
	case DependencyOpen:
		d.s.State = StateBlocked
		d.s.Run = nil
		d.s.BlockedOn = b.Dependency
		d.emit(
			d.marker(MarkerBlocked, true),
			PostStatus{Key: d.key("blocked"), Event: contract.EventBlocked, Stage: st, Dependency: b.Dependency},
			Wait{Reason: "blocked on " + b.Dependency},
		)
		d.rule = "FR-18 AC1 blocked on an open dependency"
	case DependencyDone:
		d.s.Run = nil
		d.nextStage(false)
	default:
		d.escalate(Escalation{Reason: ReasonDependencyNotDone, Stage: st, Dependency: b.Dependency}, "FR-18 AC1 dependency closed without being done")
	}
}

func (d *decision) implementDone(o *ImplementOutcome) {
	if o == nil || o.Change == "" || o.Head == "" {
		d.failed("FR-18 AC2 implement done without a change: failed")
		return
	}
	d.s.Change = &Change{Ref: o.Change, Head: o.Head, Pusher: PusherCoder}
	d.s.Run = nil
	d.nextStage(false)
}

func (d *decision) reviewDone(o *ReviewOutcome) {
	r := d.s.Run
	if o == nil {
		d.failed("FR-18 AC2 review done without a review: failed")
		return
	}
	kind := r.Review.Kind
	if o.Stale || o.Commit != d.s.Change.Head {
		// Recorded, not counted, reviewed again with the same kind
		// (FR-4 AC3, FR-6 AC5, D36, work plan D7, D17).
		d.s.Run = nil
		d.start(plan{stage: contract.StageReview, kind: kind}, 1, 0)
		if d.s.State != StateEscalated {
			d.rule = "FR-4 AC3 stale review: review again with the same kind (D36)"
		}
		return
	}
	switch kind {
	case contract.KindRound:
		d.s.RoundsUsed++
	case contract.KindBaseUpdate:
		d.s.BaseUpdates++
	}
	if kind != contract.KindRound {
		d.s.UncountedReviews++
	}
	d.s.Findings = slices.Clone(o.Findings)
	d.s.LatestReview = &Review{Commit: o.Commit, Kind: kind}
	for _, mr := range o.ManualRulings {
		i := slices.Index(d.s.PendingRulings, mr.Comment)
		if i < 0 {
			continue
		}
		d.s.PendingRulings = slices.Delete(d.s.PendingRulings, i, i+1)
		if mr.Ruling != contract.RulingAddressed {
			continue
		}
		for j := range d.s.ManualComments {
			if d.s.ManualComments[j].ID == mr.Comment {
				d.s.ManualComments[j].RuledAddressed = true
			}
		}
	}
	d.s.NextKind = ""
	d.s.OwnerPushed = false
	d.s.Run = nil
	blocking := d.s.Blocking(d.p)
	switch {
	case len(blocking) == 0:
		d.enterMerging()
		d.rule = "FR-6 AC2 no blocking finding: merge"
	case d.s.RoundsUsed >= d.s.Allowance(d.p):
		d.escalate(Escalation{Reason: ReasonRoundLimit, Stage: contract.StageReview, AfterBlockingRound: true,
			FindingIDs: blocking, CommentIDs: d.s.UnclearedManual()},
			"FR-6 AC3 blocking finding with the round allowance used up (FR-6 AC9)")
	default:
		d.startAddress(contract.ModeFindings)
		d.rule = "FR-6 AC2 blocking finding: address"
	}
}

// reviewKindAfter is the kind of the review after an address run
// (FR-6 AC6).
func reviewKindAfter(m contract.AddressMode) contract.ReviewKind {
	switch m {
	case contract.ModeManual:
		return contract.KindManualReply
	case contract.ModeBaseUpdate:
		return contract.KindBaseUpdate
	}
	return contract.KindRound
}

func (d *decision) addressDone(o *AddressOutcome) {
	r := d.s.Run
	if o == nil {
		d.failed("FR-18 AC2 address done without replies: failed")
		return
	}
	spec := r.Address
	moved := o.Head != "" && o.Head != r.StartHead
	if spec.Mode == contract.ModeBaseUpdate {
		if !moved {
			d.failed("FR-5 AC6 base_update run did not move the head: failed")
			return
		}
	} else {
		replied := func(rs []ReplyOutcome, want []string) bool {
			for _, id := range want {
				if !slices.ContainsFunc(rs, func(x ReplyOutcome) bool { return x.To == id && x.Counts }) {
					return false
				}
			}
			return true
		}
		if !(moved || replied(o.Replies, spec.AnswerFindings)) || !replied(o.ManualReplies, spec.AnswerManual) {
			d.failed("FR-5 AC2 head didn't move and a reply is missing: failed")
			return
		}
		for _, mr := range o.ManualReplies {
			if mr.Counts && slices.Contains(spec.AnswerManual, mr.To) && !slices.Contains(d.s.PendingRulings, mr.To) {
				d.s.PendingRulings = append(d.s.PendingRulings, mr.To)
			}
		}
	}
	if moved {
		d.s.Change.Head, d.s.Change.Pusher = o.Head, PusherCoder
	}
	d.s.Run = nil
	d.start(plan{stage: contract.StageReview, kind: reviewKindAfter(spec.Mode)}, 1, 0)
	if d.s.State != StateEscalated {
		d.rule = "FR-6 AC6 review after an address run takes its kind"
	}
}

// CountReviews counts the non-stale round and base_update reviews in a
// change's review records (FR-6 AC5, AC7, work plan D8).
func CountReviews(records []ReviewRecord) (rounds, baseUpdates int) {
	for _, r := range records {
		if r.Stale {
			continue
		}
		switch r.Kind {
		case contract.KindRound:
			rounds++
		case contract.KindBaseUpdate:
			baseUpdates++
		}
	}
	return rounds, baseUpdates
}
