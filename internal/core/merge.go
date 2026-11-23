package core

import (
	"fmt"
	"slices"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// MergeStage is the merge stage while the item is merging (FR-7).
type MergeStage struct {
	// Deadline is the merge stage's time limit, set on entering merging
	// (FR-16 AC1, work plan #11 D8).
	Deadline time.Time
	// Calling is set while the merge call runs, Pinned is its commit.
	Calling bool
	Pinned  string
	// NotMergeable is set after the code host refused the merge, until the
	// re-check (I15, work plan #11 D7).
	NotMergeable bool
}

// GateState is a required gate's state on a commit (INTEGRATION-SPEC §9.7).
type GateState string

// The gate states, as ports.GateState spells them.
const (
	GatePass    GateState = "pass"
	GateFail    GateState = "fail"
	GatePending GateState = "pending"
	GateMissing GateState = "missing"
)

// Gate is one required gate's result on the change head.
type Gate struct {
	Name  string
	State GateState
}

// MergeChecked is the workflow's answer to CheckMerge: the facts read right
// before merging (FR-7, work plan #11 D1). Head and Pusher are the change's
// current head and who pushed it. Gates are the required gates on Head.
// Manual is every manual comment on the change as read with them, so a
// comment added or cleared since the last poll counts at this check
// (TECHNICAL-SPEC §8.1 "right before merging", work plan #25 D3).
type MergeChecked struct {
	At
	Head         string
	Pusher       Pusher
	Trust        TrustReport
	ContainsBase bool
	Gates        []Gate
	Manual       []ManualCommentChanged `json:",omitempty"`
}

// MergeOutcome is a merge call's outcome (INTEGRATION-SPEC §8.3).
type MergeOutcome string

// The merge outcomes, as ports.MergeOutcome spells them.
const (
	MergeMerged       MergeOutcome = "merged"
	MergeHeadMoved    MergeOutcome = "head_moved"
	MergeNotMergeable MergeOutcome = "not_mergeable"
	MergeFailed       MergeOutcome = "failed"
)

// MergeResult is the outcome of the Merge action.
type MergeResult struct {
	At
	Outcome MergeOutcome
}

// enterMerging starts the merge stage: a new deadline, then a check
// (FR-7, work plan #11 D8).
func (d *decision) enterMerging() {
	d.s.State = StateMerging
	d.s.Run = nil
	d.s.Merge = &MergeStage{Deadline: d.at.Add(d.p.limits(StageMerge).TimeLimit)}
	d.emit(CheckMerge{Change: d.s.Change.Ref, Head: d.s.Change.Head})
}

// decideMerging takes the merge events while the item is merging. Other
// events (head moves, manual comments) are recorded by the common handlers.
func (d *decision) decideMerging(e Event) bool {
	switch e := e.(type) {
	case MergeChecked:
		d.mergeChecked(e)
	case MergeResult:
		d.mergeResult(e)
	case ProjectResumed:
		// The pause held the merge checks; the stage gets a new time limit
		// (FR-10 AC8, FR-16 AC1, work plan #12 D12).
		d.s.Merge.Deadline = d.at.Add(d.p.limits(StageMerge).TimeLimit)
		d.rule = "FR-10 AC8 project resumed: new merge time limit"
	default:
		return false
	}
	return true
}

// mergeChecked acts on the first merge-condition failure in the
// TECHNICAL-SPEC §8.1 order, or merges when every condition holds (FR-7).
// Each cause is handled alone; the conditions are checked again afterwards.
func (d *decision) mergeChecked(e MergeChecked) {
	m := d.s.Merge
	if m == nil || m.Calling {
		return
	}
	if e.Head != "" && e.Head != d.s.Change.Head {
		d.headMoved(HeadMoved{At: e.At, Commit: e.Head, Pusher: e.Pusher})
	}
	d.freshManual(e.Manual)
	head := d.s.Change.Head

	// 1. Untrusted commit (FR-13 AC5).
	if v := e.Trust.Check(d.s.ApprovedCommits); v.Untrusted {
		d.escalateUntrusted(v, StageMerge, "TECHNICAL-SPEC §8.1 cause 1: untrusted change")
		return
	}
	// 2. Head moved after the review (FR-7 AC1). A manual review is never a
	// review record, so it never covers the head (FR-7 AC9).
	if d.s.LatestReview == nil || d.s.LatestReview.Commit != head {
		d.startFromMerge(plan{stage: contract.StageReview, kind: d.reviewKind()},
			"TECHNICAL-SPEC §8.1 cause 2: head moved after the review")
		return
	}
	// FR-7 AC2 on the reviewed head (work plan #11 D9).
	if len(d.s.Blocking(d.p)) > 0 {
		d.startFromMerge(plan{stage: contract.StageAddress, mode: contract.ModeFindings},
			"FR-7 AC2 the reviewed head has a blocking finding")
		return
	}
	// 3. Head lacks the base tip (FR-7 AC4, AC8, D41).
	if !e.ContainsBase {
		d.beginBaseUpdate()
		if d.s.State != StateEscalated {
			d.rule = "TECHNICAL-SPEC §8.1 cause 3: head lacks the base tip, base update (D41)"
		}
		return
	}
	// 4. A manual review comment isn't cleared (FR-7 AC9, AC10, D43). No
	// round limit overrides it (FR-6 AC9).
	if uncleared := d.s.UnclearedManual(); len(uncleared) > 0 {
		if len(d.s.answerableManual()) > 0 {
			d.startFromMerge(plan{stage: contract.StageAddress, mode: contract.ModeManual},
				"TECHNICAL-SPEC §8.1 cause 4: manual review comment not cleared")
			return
		}
		d.escalate(Escalation{Reason: ReasonEmptyReviewNeedsDismissal, Stage: StageMerge, CommentIDs: uncleared},
			"TECHNICAL-SPEC §8.1 cause 4: only empty changes-requested reviews left (D43)")
		return
	}
	// 5. Branch protection refused (I15), or a gate failed.
	if m.NotMergeable {
		d.escalate(Escalation{Reason: ReasonMergeRefused, Stage: StageMerge},
			"TECHNICAL-SPEC §8.1 cause 5: merge refused as not mergeable, no earlier cause holds (I15)")
		return
	}
	var failed []string
	pending := false
	for _, g := range e.Gates {
		switch g.State {
		case GatePass:
		case GateFail:
			failed = append(failed, g.Name)
		default:
			pending = true
		}
	}
	if len(failed) > 0 {
		d.escalate(Escalation{Reason: ReasonGateFailed, Stage: StageMerge, Gates: failed},
			"TECHNICAL-SPEC §8.1 cause 5: a required gate failed")
		return
	}
	// 6. A required gate has no result yet (FR-16 AC1).
	if pending {
		if !d.at.Before(m.Deadline) {
			d.escalate(Escalation{Reason: ReasonTimeLimit, Stage: StageMerge},
				"TECHNICAL-SPEC §8.1 cause 6: merge stage time limit")
			return
		}
		d.emit(CheckMerge{Change: d.s.Change.Ref, Head: head, Recheck: true}, Wait{Reason: "required gate pending"})
		d.rule = "TECHNICAL-SPEC §8.1 cause 6: required gate pending, check again"
		return
	}
	// Every condition holds: merge pinned to the reviewed commit (FR-7 AC5).
	m.Calling, m.Pinned = true, d.s.LatestReview.Commit
	d.emit(Merge{Key: d.key("merge"), Change: d.s.Change.Ref, Pinned: m.Pinned})
	d.rule = "FR-7 every merge condition holds: merge pinned to the reviewed head"
}

// startFromMerge leaves merging for an agent stage, through the pre-stage
// checks.
func (d *decision) startFromMerge(pl plan, rule string) {
	d.start(pl, 1, 0)
	if d.s.State != StateEscalated {
		d.rule = rule
	}
}

func (d *decision) mergeResult(e MergeResult) {
	m := d.s.Merge
	if m == nil || !m.Calling {
		return
	}
	switch e.Outcome {
	case MergeMerged:
		d.s.State = StateMerged
		d.s.Merge = nil
		d.followUp()
	case MergeHeadMoved, MergeNotMergeable:
		// Checked again from the top: a moved head meets cause 1 before
		// cause 2, and a refusal is cause 5 only if causes 1-4 don't hold
		// (I15, work plan #11 D7).
		m.Calling, m.Pinned = false, ""
		m.NotMergeable = m.NotMergeable || e.Outcome == MergeNotMergeable
		d.emit(CheckMerge{Change: d.s.Change.Ref, Head: d.s.Change.Head})
		d.rule = fmt.Sprintf("FR-7 AC6 merge call %s: check again from the top (I15)", e.Outcome)
	default:
		d.escalate(Escalation{Reason: ReasonMergeFailed, Stage: StageMerge}, "TECHNICAL-SPEC §8.1 cause 5: merge call failed")
	}
}

// followUp files one follow-up for the unresolved findings after the loop's
// own merge (FR-8 AC1, AC3, AC4, work plan #11 D11, D12).
func (d *decision) followUp() {
	var rows []FollowUpRow
	for _, f := range d.s.Findings {
		if f.Unresolved() {
			rows = append(rows, FollowUpRow{ID: f.ID, Score: f.Score, Status: f.Status, Comment: f.Comment})
		}
	}
	if len(rows) == 0 {
		d.rule = "FR-7 merged, no unresolved finding: no follow-up (FR-8 AC4)"
		return
	}
	d.emit(FileFollowUp{Key: "followup:" + d.s.Change.Ref, Item: d.s.Item, Change: d.s.Change.Ref, Rows: rows})
	d.rule = "FR-7 merged: follow-up for unresolved findings (FR-8 AC1)"
}

// freshManual records the manual comments as a stage read them (work plan
// #25 D3). A comment a human had cleared and no longer is blocks again
// (FR-7 AC9), as the item workflow does for feed events.
func (d *decision) freshManual(ms []ManualCommentChanged) {
	rule := d.rule
	for _, mc := range ms {
		if i := slices.IndexFunc(d.s.ManualComments, func(m ManualComment) bool { return m.ID == mc.Comment }); i >= 0 &&
			d.s.ManualComments[i].HumanCleared && !mc.HumanCleared {
			mc.Reopened = true
		}
		d.manualChanged(mc)
	}
	d.rule = rule
}
