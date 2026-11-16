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
// Extension points. Merge decisions (T6) go in decideMerging, which sees
// every event while the item is merging, and may start a base update
// (beginBaseUpdate) or a manual-mode address run (startAddress). Owner and
// supervisor rules (T7) add their events to dispatch.
func Decide(s ItemState, e Event, p Policy) (ItemState, []Action) {
	d := &decision{s: s.Clone(), p: p, at: e.When(), rule: "ignored"}
	d.s.Seq++
	if d.s.State != StateMerging || !d.decideMerging(e) {
		d.dispatch(e)
	}
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
	case Resumed:
		d.resumed()
	case DependencyClosed:
		d.dependencyClosed(e)
	case BlockRemoved:
		d.blockRemoved()
	case DayBoundary:
		d.dayBoundary()
	}
}

// decideMerging is the merge decisions' extension point (T6, work plan
// D21). It returns true when it handled the event. Until T6, it handles
// nothing, so facts (head moves, manual comments) are still recorded by the
// common handlers and the item stays in merging.
func (d *decision) decideMerging(Event) bool { return false }

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
		c := *e.Change
		d.s.Change = &c
	}
	d.s.RoundsAdded = e.RoundsAdded
	d.s.RoundsUsed, d.s.BaseUpdates = CountReviews(e.Reviews)
	for _, r := range e.Reviews {
		if !r.Stale {
			d.s.Findings = slices.Clone(r.Findings)
			d.s.LatestReview = &Review{Commit: r.Commit, Kind: r.Kind}
		}
	}
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
	d.s.Escalation = &e
	d.emit(
		SetMarker{Key: d.key("marker"), Marker: MarkerNeedsOwner, On: true},
		PostStatus{Key: d.key("escalate"), Event: contract.EventEscalate, Reason: e.Reason, Stage: e.Stage,
			FindingIDs: slices.Clone(e.FindingIDs), CommentIDs: slices.Clone(e.CommentIDs), Dependency: e.Dependency},
		Notify{Key: d.key("notify"), Event: contract.EventEscalate, Reason: e.Reason},
		Wait{Reason: "escalated: " + e.Reason},
	)
	d.rule = rule
}

// resumed is the owner's resume of an escalated item (FR-10 AC2, AC5, AC8).
func (d *decision) resumed() {
	if d.s.State != StateEscalated || d.s.Escalation == nil {
		return
	}
	esc := *d.s.Escalation
	added := 0
	if esc.Reason == ReasonRoundLimit {
		added = d.p.resumeRounds()
		d.s.RoundsAdded += added
	}
	d.s.Escalation = nil
	d.s.Run = nil // a new attempt count and time limit (FR-10 AC8, D20)
	d.emit(
		SetMarker{Key: d.key("marker"), Marker: MarkerNeedsOwner, On: false},
		PostStatus{Key: d.key("resume"), Event: contract.EventResume, Reason: esc.Reason, RoundsAdded: &added},
	)
	d.nextStage(esc.AfterBlockingRound)
}

func (d *decision) dependencyClosed(e DependencyClosed) {
	if d.s.State != StateBlocked || e.Dependency != d.s.BlockedOn {
		return
	}
	dep := d.s.BlockedOn
	d.s.BlockedOn = ""
	d.emit(SetMarker{Key: d.key("marker"), Marker: MarkerBlocked, On: false})
	if !e.Done {
		d.escalate(Escalation{Reason: ReasonDependencyNotDone, Dependency: dep}, "FR-18 AC1 dependency closed without being done")
		return
	}
	d.nextStage(false)
}

func (d *decision) blockRemoved() {
	if d.s.State != StateBlocked {
		return
	}
	d.s.BlockedOn = ""
	d.emit(SetMarker{Key: d.key("marker"), Marker: MarkerBlocked, On: false})
	d.nextStage(false)
}

func (d *decision) dayBoundary() {
	if d.s.State != StateWaitingLimit {
		return
	}
	d.nextStage(false)
}
