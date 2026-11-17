package core

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// The merge conditions (FR-7), the TECHNICAL-SPEC §8.1 order, the merge
// outcomes (INTEGRATION-SPEC §8.3, I15) and the follow-up (FR-8).

const time10m = 10 * time.Minute

// merging returns a harness whose round-1 review of h1 was clean, so the
// item is merging.
func merging(t *testing.T, p Policy, fs ...Finding) *harness {
	t.Helper()
	return inReview(t, p).reviewed(fs).state(StateMerging)
}

// clean is a merge check where every condition holds on h1.
func clean() MergeChecked {
	return MergeChecked{Head: "h1", Pusher: PusherCoder, ContainsBase: true,
		Gates: []Gate{{Name: "lint", State: GatePass}, {Name: "test", State: GatePass}}}
}

func (h *harness) check(c MergeChecked) *harness {
	h.t.Helper()
	if c.Time.IsZero() {
		c.At = h.at()
	}
	return h.do(c)
}

func (h *harness) mergeResult(o MergeOutcome) *harness {
	h.t.Helper()
	return h.do(MergeResult{At: h.at(), Outcome: o})
}

func (h *harness) mergeAction() Merge {
	h.t.Helper()
	for _, a := range h.acts {
		if m, ok := a.(Merge); ok {
			return m
		}
	}
	h.t.Fatalf("no Merge in %v (rule %q)", h.record().Actions, h.record().Rule)
	return Merge{}
}

func (h *harness) checkMerge() CheckMerge {
	h.t.Helper()
	for _, a := range h.acts {
		if c, ok := a.(CheckMerge); ok {
			return c
		}
	}
	h.t.Fatalf("no CheckMerge in %v", h.record().Actions)
	return CheckMerge{}
}

func hasAction[T Action](acts []Action) bool {
	return slices.ContainsFunc(acts, func(a Action) bool { _, ok := a.(T); return ok })
}

// TestFR7AC6_EachCauseIsChosenFirst starts with every cause holding and
// removes them one at a time, top down: each time the first remaining cause
// is the one acted on (TECHNICAL-SPEC §8.1).
func TestFR7AC6_EachCauseIsChosenFirst(t *testing.T) {
	type facts struct {
		untrusted, headMoved, noBase, manual, gateFailed, gatePending bool
	}
	build := func(f facts) MergeChecked {
		c := clean()
		if f.untrusted {
			c.Trust = TrustReport{Causes: []string{CauseRulesetChanged}, Commits: []string{"h1"}}
		}
		if f.headMoved {
			c.Head, c.Pusher = "h2", PusherOwner
		}
		c.ContainsBase = !f.noBase
		c.Gates = nil
		if f.gateFailed {
			c.Gates = append(c.Gates, Gate{Name: "lint", State: GateFail})
		}
		if f.gatePending {
			c.Gates = append(c.Gates, Gate{Name: "test", State: GatePending})
		}
		return c
	}
	all := facts{true, true, true, true, true, true}
	for _, tc := range []struct {
		name string
		f    facts
		want func(*harness)
	}{
		{"1 untrusted commit", all, func(h *harness) {
			ps := h.escalation(ReasonUntrustedCommit)
			if !slices.Equal(ps.Commits, []string{"h1"}) || ps.Stage != StageMerge {
				t.Fatalf("status = %+v", ps)
			}
		}},
		{"2 head moved", facts{false, true, true, true, true, true}, func(h *harness) {
			h.reviewKind(contract.KindOwnerPush) // FR-6 AC6: an owner pushed
			if h.runStage().Review.Commit != "h2" {
				t.Fatal("review must cover the new head")
			}
		}},
		{"3 head lacks base tip", facts{false, false, true, true, true, true}, func(h *harness) {
			h.state(StateAddressing)
			h.noRunStage() // D41: no agent run
			if !hasAction[UpdateBase](h.acts) {
				t.Fatalf("actions = %v", h.record().Actions)
			}
		}},
		{"4 manual comment not cleared", facts{false, false, false, true, true, true}, func(h *harness) {
			h.addressMode(contract.ModeManual)
			if !slices.Equal(h.runStage().Address.AnswerManual, []string{"MC-1"}) {
				t.Fatalf("address = %+v", h.runStage().Address)
			}
		}},
		{"5 gate failed", facts{false, false, false, false, true, true}, func(h *harness) {
			if ps := h.escalation(ReasonGateFailed); !slices.Equal(ps.Gates, []string{"lint"}) {
				t.Fatalf("status = %+v", ps)
			}
		}},
		{"6 gate pending", facts{false, false, false, false, false, true}, func(h *harness) {
			h.state(StateMerging)
			if c := h.checkMerge(); !c.Recheck || c.Head != "h1" {
				t.Fatalf("check = %+v", c)
			}
			if hasAction[Merge](h.acts) {
				t.Fatal("merged with a pending gate")
			}
		}},
		{"all hold: merge", facts{}, func(h *harness) {
			h.state(StateMerging)
			h.mergeAction()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := merging(t, Policy{})
			h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1", HumanCleared: !tc.f.manual})
			h.check(build(tc.f))
			tc.want(h)
		})
	}
}

func TestFR7AC6_OnlyEmptyChangesRequestedReviewsEscalate(t *testing.T) {
	h := merging(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-2", Empty: true})
	h.check(clean())
	if ps := h.escalation(ReasonEmptyReviewNeedsDismissal); !slices.Equal(ps.CommentIDs, []string{"MC-2"}) {
		t.Fatalf("status = %+v", ps)
	}
}

func TestFR7AC6_OtherManualCommentsAreAnsweredBeforeAnEmptyReview(t *testing.T) {
	h := merging(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-2", Empty: true})
	h.check(clean()).addressMode(contract.ModeManual)
	if got := h.runStage().Address.AnswerManual; !slices.Equal(got, []string{"MC-1"}) {
		t.Fatalf("answer_manual = %v, an empty review is never answered (D43)", got)
	}
}

func TestFR7AC6_HeadMovedByCoderIsARoundReview(t *testing.T) {
	merging(t, Policy{}).check(MergeChecked{Head: "h2", Pusher: PusherCoder, ContainsBase: true}).reviewKind(contract.KindRound)
}

func TestFR6AC8_HeadMovedAtMergeIsRefusedAtTheRoundLimit(t *testing.T) {
	h := merging(t, Policy{MaxRounds: 1})
	h.check(MergeChecked{Head: "h2", Pusher: PusherCoder, ContainsBase: true})
	h.escalation(ReasonRoundLimit)
	h.noRunStage()
}

func TestFR6AC7_BaseUpdateAtMergeEscalatesAtTheLimit(t *testing.T) {
	h := merging(t, Policy{MaxBaseUpdates: 1})
	h.check(MergeChecked{Head: "h1", ContainsBase: false})
	h.do(BaseUpdated{At: h.at(), Head: "h2"}).reviewKind(contract.KindBaseUpdate)
	h.reviewed(nil).state(StateMerging)
	h.check(MergeChecked{Head: "h2", ContainsBase: false})
	h.escalation(ReasonBaseUpdateLimit)
}

func TestFR7AC2_BlockingFindingAtTheCheckIsAddressed(t *testing.T) {
	h := merging(t, Policy{}, open2)
	h.p.BlockingThreshold = 2 // the profile's threshold was lowered
	h.check(clean()).addressMode(contract.ModeFindings)
}

// TestFR7AC2_BlockingFindingSitsBetweenCauses2And3 pins where the work plan
// #11 D9 guard sits in the §8.1 order: causes 1 and 2 win over it, and it
// wins over causes 3 and 4.
func TestFR7AC2_BlockingFindingSitsBetweenCauses2And3(t *testing.T) {
	// Causes 3 and 4 also hold in every case.
	later := func(h *harness) MergeChecked {
		h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
		c := clean()
		c.ContainsBase = false
		return c
	}
	for _, tc := range []struct {
		name string
		edit func(*MergeChecked)
		want func(*harness)
	}{
		{"cause 1 wins", func(c *MergeChecked) {
			c.Trust = TrustReport{Causes: []string{CauseRulesetChanged}, Commits: []string{"h1"}}
		}, func(h *harness) { h.escalation(ReasonUntrustedCommit) }},
		{"cause 2 wins", func(c *MergeChecked) { c.Head, c.Pusher = "h2", PusherOwner }, func(h *harness) {
			h.reviewKind(contract.KindOwnerPush)
			if h.runStage().Review.Commit != "h2" {
				t.Fatal("review must cover the new head")
			}
		}},
		{"guard beats causes 3 and 4", func(*MergeChecked) {}, func(h *harness) {
			h.addressMode(contract.ModeFindings)
			if hasAction[UpdateBase](h.acts) {
				t.Fatal("base update chosen over the blocking finding")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := merging(t, Policy{}, open2)
			h.p.BlockingThreshold = 2
			c := later(h)
			tc.edit(&c)
			h.check(c)
			tc.want(h)
		})
	}
}

func TestFR16AC1_PendingGateEscalatesAtTheMergeTimeLimit(t *testing.T) {
	h := merging(t, Policy{Stages: map[contract.Stage]StageLimits{StageMerge: {TimeLimit: time10m}}})
	deadline := h.s.Merge.Deadline
	if !deadline.Equal(h.record().At.Add(time10m)) {
		t.Fatalf("deadline = %v", deadline)
	}
	pending := clean()
	pending.Gates = []Gate{{Name: "test", State: GateMissing}}
	h.check(pending).state(StateMerging)
	pending.At = At{deadline.Add(-1)}
	h.check(pending).state(StateMerging)
	pending.At = At{deadline}
	h.check(pending)
	if ps := h.escalation(ReasonTimeLimit); ps.Stage != StageMerge {
		t.Fatalf("status = %+v", ps)
	}
	// A resume enters merging again with a new deadline (work plan #11 D8).
	h.do(Resumed{At: At{deadline.Add(time10m)}}).state(StateMerging)
	if !h.s.Merge.Deadline.Equal(deadline.Add(2 * time10m)) {
		t.Fatalf("deadline after resume = %v", h.s.Merge.Deadline)
	}
}

func TestFR7AC5_MergeIsPinnedToTheReviewedHead(t *testing.T) {
	h := merging(t, Policy{})
	h.check(clean())
	m := h.mergeAction()
	if m.Pinned != "h1" || m.Change != "57" || m.Pinned != h.s.LatestReview.Commit {
		t.Fatalf("merge = %+v", m)
	}
	// A push after the check fails the merge: checked again, then reviewed.
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
	h.check(clean()).state(StateMerging) // ignored while the call runs
	if hasAction[Merge](h.acts) {
		t.Fatal("a second merge call while one runs")
	}
	h.mergeResult(MergeHeadMoved).state(StateMerging)
	if c := h.checkMerge(); c.Recheck || c.Head != "h2" {
		t.Fatalf("check = %+v", c)
	}
	h.check(MergeChecked{Head: "h2", Pusher: PusherOwner, ContainsBase: true}).reviewKind(contract.KindOwnerPush)
}

func TestFR7AC5_HeadMovedOutcomeMeetsTheTrustCheckFirst(t *testing.T) {
	h := merging(t, Policy{})
	h.check(clean()).mergeResult(MergeHeadMoved)
	h.check(MergeChecked{Head: "h2", Pusher: PusherUnknown, ContainsBase: true,
		Trust: TrustReport{Causes: []string{CausePusherUnknown}, Commits: []string{"h2"}}})
	h.escalation(ReasonPusherUnknown)
}

func TestI15_NotMergeableIsCheckedAgainFromTheTop(t *testing.T) {
	t.Run("an earlier cause holds", func(t *testing.T) {
		h := merging(t, Policy{})
		h.check(clean()).mergeResult(MergeNotMergeable).state(StateMerging)
		h.checkMerge()
		h.check(MergeChecked{Head: "h1", ContainsBase: false}).state(StateAddressing) // cause 3, not escalated
		if !hasAction[UpdateBase](h.acts) {
			t.Fatalf("actions = %v", h.record().Actions)
		}
		h.do(BaseUpdated{At: h.at(), Head: "h2"}).reviewed(nil).state(StateMerging)
		if h.s.Merge.NotMergeable {
			t.Fatal("the refusal must not outlive the merge stage")
		}
	})
	t.Run("no earlier cause: protection refused", func(t *testing.T) {
		h := merging(t, Policy{})
		h.check(clean()).mergeResult(MergeNotMergeable)
		pending := clean()
		pending.Gates = []Gate{{Name: "test", State: GatePending}}
		h.check(pending) // cause 5 comes before cause 6
		h.escalation(ReasonMergeRefused)
	})
}

func TestFR7AC6_FailedMergeCallEscalates(t *testing.T) {
	h := merging(t, Policy{})
	h.check(clean()).mergeResult(MergeFailed)
	h.escalation(ReasonMergeFailed)
}

func TestFR7_MergeEventsOutsideTheirPhaseAreIgnored(t *testing.T) {
	h := merging(t, Policy{})
	h.mergeResult(MergeMerged).state(StateMerging) // no call runs
	if h.record().Rule != "ignored" {
		t.Fatalf("rule = %q", h.record().Rule)
	}
	h = inReview(t, Policy{})
	h.check(clean()).state(StateInReview)
	if h.record().Rule != "ignored" {
		t.Fatalf("rule = %q", h.record().Rule)
	}
}

func TestFR7AC9_ManualApprovalNeverAllowsMerge(t *testing.T) {
	// A manual review is never a review record, so after an owner's push
	// only a new loop review can cover the head, whatever humans approved.
	h := merging(t, Policy{})
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
	c := clean()
	c.Head = "h2"
	h.check(c).reviewKind(contract.KindOwnerPush)
	h.noMerge()
}

func (h *harness) noMerge() {
	h.t.Helper()
	if hasAction[Merge](h.acts) {
		h.t.Fatal("unexpected Merge")
	}
}

func TestFR7AC9_UnclearedManualCommentBlocksUntilCleared(t *testing.T) {
	h := merging(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.check(clean()).addressMode(contract.ModeManual)
	h.addressed("h1").reviewKind(contract.KindManualReply) // FR-7 AC10, head didn't move
	h.reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingOpen}).state(StateMerging)
	h.check(clean()).addressMode(contract.ModeManual) // ruled open: still blocks
	h.addressed("h1").reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingAddressed})
	// A human reopens it before the check: it blocks again.
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1", Reopened: true})
	h.check(clean()).addressMode(contract.ModeManual)
	h.noMerge()
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1", HumanCleared: true})
	h.addressed("h1").reviewed(nil).check(clean()).mergeAction()
}

func TestFR6AC9_RoundLimitNeverOverridesAnUnclearedManualComment(t *testing.T) {
	h := merging(t, Policy{MaxRounds: 1})
	if h.s.RoundsUsed != h.s.Allowance(h.p) {
		t.Fatal("setup: the allowance should be used up")
	}
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	for range 3 {
		h.check(clean()).addressMode(contract.ModeManual)
		h.addressed("h1").reviewKind(contract.KindManualReply)
		h.reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingOpen}).state(StateMerging)
	}
	if h.s.RoundsUsed != 1 {
		t.Fatalf("rounds = %d, manual_reply reviews use no allowance", h.s.RoundsUsed)
	}
	h.check(clean()).addressMode(contract.ModeManual)
	h.addressed("h1").reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingAddressed})
	h.check(clean()).mergeAction()
}

func TestFR8AC1_FollowUpListsOnlyReferences(t *testing.T) {
	partial := Finding{ID: "F4", Score: 3, Status: contract.StatusPartiallyAddressed, Comment: "c-4"}
	withComment := Finding{ID: "F2", Score: 2, Status: contract.StatusOpen, Comment: "c-2"}
	withdrawn := f("F5", 1, contract.StatusWithdrawn)
	h := merging(t, Policy{}, withComment, fixed4, partial, withdrawn)
	h.check(clean()).mergeResult(MergeMerged).state(StateMerged)
	var fu *FileFollowUp
	for _, a := range h.acts {
		if x, ok := a.(FileFollowUp); ok {
			fu = &x
		}
	}
	if fu == nil {
		t.Fatalf("no follow-up: %v", h.record().Actions)
	}
	want := []FollowUpRow{{ID: "F2", Score: 2, Status: contract.StatusOpen, Comment: "c-2"},
		{ID: "F4", Score: 3, Status: contract.StatusPartiallyAddressed, Comment: "c-4"}}
	if fu.Key != "followup:57" || fu.Item != "42" || fu.Change != "57" || !reflect.DeepEqual(fu.Rows, want) {
		t.Fatalf("follow-up = %+v", fu)
	}
	// The row type can't carry a title or reviewer text: it has only these
	// fields (FR-8 AC1, §8.5).
	var names []string
	rt := reflect.TypeOf(FollowUpRow{})
	for i := range rt.NumField() {
		names = append(names, rt.Field(i).Name)
	}
	if !slices.Equal(names, []string{"ID", "Score", "Status", "Comment"}) {
		t.Fatalf("FollowUpRow fields = %v", names)
	}
	ft := reflect.TypeOf(FileFollowUp{})
	if ft.NumField() != 4 {
		t.Fatalf("FileFollowUp has %d fields, want Key, Item, Change, Rows", ft.NumField())
	}
}

func TestFR8AC3_FollowUpKeyIsPerChange(t *testing.T) {
	// The key names only the change, so a retried decision or a later claim
	// never files a second one (INTEGRATION-SPEC §6.6).
	for range 2 {
		h := merging(t, Policy{}, open2)
		h.check(clean()).mergeResult(MergeMerged)
		for _, a := range h.acts {
			if fu, ok := a.(FileFollowUp); ok && fu.Key != "followup:57" {
				t.Fatalf("key = %s", fu.Key)
			}
		}
	}
}

func TestFR8AC4_NoFollowUpWithoutUnresolvedFindings(t *testing.T) {
	h := merging(t, Policy{}, fixed4, f("F5", 1, contract.StatusWithdrawn))
	h.check(clean()).mergeResult(MergeMerged).state(StateMerged)
	if hasAction[FileFollowUp](h.acts) {
		t.Fatal("follow-up with nothing unresolved")
	}
	h.check(clean()).state(StateMerged) // merged is final
	if h.record().Rule != "ignored" {
		t.Fatalf("rule = %q", h.record().Rule)
	}
}

// FR-10 AC8, FR-16 AC1: a project pause holds the merge checks, so the
// project resume gives the merge stage a new time limit (review R5, D12).
func TestFR10AC8_ProjectResumeGivesTheMergeANewTimeLimit(t *testing.T) {
	h := merging(t, Policy{Stages: map[contract.Stage]StageLimits{StageMerge: {TimeLimit: time10m}}})
	old := h.s.Merge.Deadline
	resumed := old.Add(3 * time.Hour)
	h.do(ProjectResumed{At: At{resumed}}).state(StateMerging)
	if !h.s.Merge.Deadline.Equal(resumed.Add(time10m)) {
		t.Fatalf("deadline = %v, want %v", h.s.Merge.Deadline, resumed.Add(time10m))
	}
	pending := clean()
	pending.Gates = []Gate{{Name: "test", State: GateMissing}}
	pending.At = At{resumed.Add(time.Minute)}
	h.check(pending).state(StateMerging)
	pending.At = At{resumed.Add(time10m)}
	h.check(pending)
	h.escalation(ReasonTimeLimit)
}

// A project resume changes nothing for an item that isn't merging.
func TestFR10AC8_ProjectResumeOutsideMergingHasNoEffect(t *testing.T) {
	h := inReview(t, Policy{})
	run := *h.s.Run
	h.do(ProjectResumed{At: h.at()}).state(StateInReview)
	if h.s.Run == nil || h.s.Run.RunID != run.RunID || !h.s.Run.Deadline.Equal(run.Deadline) {
		t.Fatalf("run changed: %+v", h.s.Run)
	}
	h.noRunStage()
}
