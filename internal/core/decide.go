package core

import (
	"fmt"
	"slices"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// Decide is deterministic: the same state, event and policy always give the
// same result (TECHNICAL-SPEC §4). It never changes s, and it reads no
// clock: the only time it uses is the event's. The last action is always
// the decision record (NFR-5 AC1, work plan D1).
//
// Owner actions are decided first (owner.go). An item an owner action holds
// while work is in flight decides that work's events through held. Merge
// decisions are in decideMerging (merge.go), which sees every other event
// while the item is merging.
func Decide(s ItemState, e Event, p Policy) (ItemState, []Action) {
	d := &decision{s: s.Clone(), p: p, at: e.When(), rule: "ignored"}
	d.s.Seq++
	d.route(e)
	rec := DecisionRecord{
		Seq:    d.s.Seq,
		At:     d.at,
		Event:  e.Name(),
		From:   s.State,
		To:     d.s.State,
		Basis:  basisOf(s, p),
		Result: d.result,
		Rule:   d.rule,
	}
	for _, a := range d.actions {
		rec.Actions = append(rec.Actions, a.Kind())
	}
	return d.s, append(d.actions, RecordDecision{Record: rec})
}

// decision accumulates one call's new state, actions and rule.
type decision struct {
	s       ItemState
	p       Policy
	at      time.Time
	actions []Action
	rule    string
	result  *ResultRef
}

func (d *decision) emit(a ...Action) { d.actions = append(d.actions, a...) }

// key is an idempotency key: workflow ID, claim number, verb and decision
// number (TECHNICAL-SPEC §9).
func (d *decision) key(verb string) string {
	return fmt.Sprintf("%s#%d:%s:%d", d.s.WorkflowID, d.s.Claim, verb, d.s.Seq)
}

// marker sets or clears a marker. Its key names the marker and the value,
// so two marker writes in one decision never share a key (review R2).
func (d *decision) marker(name string, on bool) SetMarker {
	return SetMarker{Key: d.key(fmt.Sprintf("marker:%s:%t", name, on)), Marker: name, On: on}
}

func (d *decision) route(e Event) {
	switch {
	case d.owner(e):
	case isHold(d.s.State) && d.under() != "":
		d.held(e)
	default:
		d.plain(e)
	}
}

func (d *decision) plain(e Event) {
	if d.s.State != StateMerging || !d.decideMerging(e) {
		d.dispatch(e)
	}
}

func (d *decision) dispatch(e Event) {
	switch e := e.(type) {
	case Started:
		d.started(e)
	case StageResult:
		d.stageResult(e)
	case StageCrashed:
		d.crashed(e)
	case StageTimedOut:
		d.timedOut(e)
	case DailyLimitReached:
		d.dailyLimit(e)
	case HeadMoved:
		d.headMoved(e)
	case ManualCommentChanged:
		d.manualChanged(e)
	case BaseUpdated:
		d.baseUpdated(e)
	case BaseConflicts:
		d.baseConflicts()
	case BaseUpdateRefused:
		d.baseRefused()
	case DependencyClosed:
		d.dependencyClosed(e)
	case DayBoundary:
		d.dayBoundary()
	case TrustRefused:
		d.trustRefused(e)
	}
}

func basisOf(s ItemState, p Policy) Basis {
	b := Basis{
		RoundsUsed:       s.RoundsUsed,
		RoundAllowance:   s.Allowance(p),
		BaseUpdates:      s.BaseUpdates,
		BlockingFindings: s.Blocking(p),
		UnclearedManual:  s.UnclearedManual(),
	}
	if s.Change != nil {
		b.Head = s.Change.Head
	}
	if s.LatestReview != nil {
		b.ReviewCommit = s.LatestReview.Commit
		b.ReviewKind = s.LatestReview.Kind
	}
	return b
}

// started handles the claim: it adopts an open change and rebuilds the
// review state from its records (FR-3 AC3, FR-6 AC5), then picks the first
// stage by the next-stage rules (work plan D3).
func (d *decision) started(e Started) {
	if d.s.State != StateNone {
		return
	}
	d.s.WorkflowID, d.s.Item, d.s.Claim = e.WorkflowID, e.Item, e.Claim
	if e.Change != nil {
		d.adopt(*e.Change, e.Reviews)
	}
	d.s.RoundsAdded = e.RoundsAdded
	d.s.ApprovedCommits = slices.Clone(e.ApprovedCommits)
	d.s.ClosedChanges = e.ClosedChanges
	d.nextStage(false)
}

func (d *decision) headMoved(e HeadMoved) {
	if d.s.Change == nil || e.Commit == "" || e.Commit == d.s.Change.Head {
		return
	}
	d.s.Change.Head = e.Commit
	d.s.Change.Pusher = e.Pusher
	if e.Pusher == PusherOwner {
		d.s.OwnerPushed = true
	}
	if e.Pusher == PusherUnknown && d.s.State == StateInReview {
		// Every commit of the push, not only its head (work plan #11 D6).
		for _, c := range append(slices.Clone(e.Commits), e.Commit) {
			if !slices.Contains(d.s.UnknownPushes, c) {
				d.s.UnknownPushes = append(d.s.UnknownPushes, c)
			}
		}
	}
	d.rule = "FR-12 AC4 head moved recorded"
}

func (d *decision) manualChanged(e ManualCommentChanged) {
	i := slices.IndexFunc(d.s.ManualComments, func(m ManualComment) bool { return m.ID == e.Comment })
	if i < 0 {
		d.s.ManualComments = append(d.s.ManualComments, ManualComment{ID: e.Comment})
		i = len(d.s.ManualComments) - 1
	}
	m := &d.s.ManualComments[i]
	m.Empty = e.Empty
	m.HumanCleared = e.HumanCleared
	if e.Reopened {
		m.RuledAddressed = false
	}
	d.rule = "FR-7 AC9 manual comment recorded"
}

// escalate sends the item to the owner (FR-10 AC1).
func (d *decision) escalate(e Escalation, rule string) {
	d.s.State = StateEscalated
	d.s.Run = nil
	d.s.UpdatingBase = false
	d.s.Merge = nil
	d.s.Escalation = &e
	d.s.PendingEscalation = false
	d.announce(e)
	d.rule = rule
}

// announce emits an escalation's marker, status record, notification and
// wait (FR-10 AC1, FR-11 AC3).
func (d *decision) announce(e Escalation) {
	d.emit(
		d.marker(MarkerNeedsOwner, true),
		PostStatus{Key: d.key("escalate"), Event: contract.EventEscalate, Reason: e.Reason, Stage: e.Stage,
			FindingIDs: slices.Clone(e.FindingIDs), CommentIDs: slices.Clone(e.CommentIDs), Dependency: e.Dependency,
			Commits: slices.Clone(e.Commits), Gates: slices.Clone(e.Gates)},
		Notify{Key: d.key("notify"), Event: contract.EventEscalate, Reason: e.Reason},
		Wait{Reason: "escalated: " + e.Reason},
	)
}

// resumed is the owner's resume of an escalated item (FR-10 AC2, AC5, AC8).
// On a paused item it settles the escalation and the item stays paused
// (work plan #12 D5).
func (d *decision) resumed() {
	if (d.s.State != StateEscalated && d.s.State != StatePaused) || d.s.Escalation == nil {
		return
	}
	esc := *d.s.Escalation
	added := 0
	if esc.Reason == ReasonRoundLimit {
		added = d.p.resumeRounds()
		d.s.RoundsAdded += added
	}
	d.s.Escalation, d.s.PendingEscalation = nil, false
	d.emit(
		d.marker(MarkerNeedsOwner, false),
		PostStatus{Key: d.key("resume"), Event: contract.EventResume, Reason: esc.Reason, RoundsAdded: &added},
	)
	d.approve(esc)
	switch {
	case d.s.State == StatePaused:
		d.s.ResumeAddress = esc.AfterBlockingRound
		d.rule = "FR-12 AC4 needs-owner removed on a paused item: stays paused"
	case d.s.Running != "":
		d.s.State, d.s.Running = d.s.Running, ""
		d.rule = "FR-10 AC2 resume: the running work continues"
	case d.s.BlockedOn != "":
		d.s.State = StateBlocked
		d.emit(Wait{Reason: "blocked on " + d.s.BlockedOn})
		d.rule = "FR-10 AC2 resume: still blocked on an open dependency (work plan #12 D2)"
	default:
		d.s.Run = nil // a new attempt count and time limit (FR-10 AC8, D20)
		d.nextStage(esc.AfterBlockingRound)
	}
}

func (d *decision) dependencyClosed(e DependencyClosed) {
	if d.s.State != StateBlocked || e.Dependency != d.s.BlockedOn {
		return
	}
	dep := d.s.BlockedOn
	d.s.BlockedOn = ""
	d.emit(d.marker(MarkerBlocked, false))
	if !e.Done {
		d.escalate(Escalation{Reason: ReasonDependencyNotDone, Dependency: dep}, "FR-18 AC1 dependency closed without being done")
		return
	}
	d.nextStage(false)
}

// blockRemoved ends a block now (FR-12 AC4). A held item stays held.
func (d *decision) blockRemoved() {
	switch {
	case d.s.State == StateBlocked:
		d.s.BlockedOn = ""
		d.emit(d.marker(MarkerBlocked, false))
		d.nextStage(false)
	case d.s.BlockedOn != "" && d.s.Claimed():
		d.unblock()
		d.rule = "FR-12 AC4 owner removed blocked: the item stays " + string(d.s.State)
	}
}

func (d *decision) dayBoundary() {
	if d.s.State != StateWaitingLimit {
		return
	}
	d.nextStage(false)
}
