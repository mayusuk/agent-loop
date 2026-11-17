package core

import (
	"slices"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
)

// The owner actions of the PRODUCT-SPEC §6 table and the manual actions of
// FR-12 AC4.

func (h *harness) owner(m string, added bool) *harness {
	h.t.Helper()
	return h.do(MarkerChanged{At: h.at(), By: RoleOwner, Marker: m, Added: added})
}

func (h *harness) cmd(c Command) *harness {
	h.t.Helper()
	return h.do(OwnerCommand{At: h.at(), Command: c})
}

func (h *harness) has(pred func(Action) bool) bool {
	return slices.ContainsFunc(h.acts, pred)
}

func (h *harness) markerSet(m string, on, onChange bool) bool {
	return h.has(func(a Action) bool {
		x, ok := a.(SetMarker)
		return ok && x.Marker == m && x.On == on && x.OnChange == onChange
	})
}

func (h *harness) ended(reason string) bool {
	return h.has(func(a Action) bool { x, ok := a.(EndItem); return ok && x.Reason == reason })
}

func (h *harness) stopped() bool {
	return h.has(func(a Action) bool { _, ok := a.(StopRun); return ok })
}

func (h *harness) announced() bool {
	return h.has(func(a Action) bool { x, ok := a.(PostStatus); return ok && x.Event == contract.EventEscalate })
}

// claimedStates builds an item in each §6 claimed state. Every one but
// implementing has a change; running says a handler run is in flight.
type claimedCase struct {
	name    string
	state   State
	running bool
	build   func(*testing.T) *harness
}

func claimedStates() []claimedCase {
	return []claimedCase{
		{"implementing", StateImplementing, true, func(t *testing.T) *harness { return newHarness(t, Policy{}).start(nil) }},
		{"in_review", StateInReview, true, func(t *testing.T) *harness { return inReview(t, Policy{}) }},
		{"addressing", StateAddressing, true, func(t *testing.T) *harness {
			return inReview(t, Policy{}).reviewed([]Finding{open4})
		}},
		{"merging", StateMerging, false, func(t *testing.T) *harness { return inReview(t, Policy{}).reviewed(nil) }},
		{"blocked", StateBlocked, false, func(t *testing.T) *harness {
			h := inReview(t, Policy{}).reviewed([]Finding{open4})
			return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageAddress, Outcome: contract.OutcomeBlocked,
				Blocked: &BlockedOutcome{Dependency: "61", State: DependencyOpen}})
		}},
		{"escalated", StateEscalated, false, func(t *testing.T) *harness {
			return inReview(t, Policy{}).result(contract.OutcomeNeedsOwner)
		}},
		{"waiting_limit", StateWaitingLimit, false, func(t *testing.T) *harness {
			h := inReview(t, Policy{})
			return h.do(DailyLimitReached{At: h.at(), RunID: h.runID()})
		}},
		{"paused", StatePaused, true, func(t *testing.T) *harness { return inReview(t, Policy{}).cmd(CommandPause) }},
		{"aborted", StateAborted, true, func(t *testing.T) *harness { return inReview(t, Policy{}).cmd(CommandAbort) }},
	}
}

func TestFR12AC4_OwnerPausesTheItem(t *testing.T) {
	for _, c := range claimedStates() {
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t).state(c.state)
			h.owner(MarkerPaused, true)
			if c.state == StatePaused || c.state == StateAborted {
				h.state(c.state)
				return
			}
			h.state(StatePaused)
			h.noRunStage()
			if c.state != StatePaused && !h.markerSet(MarkerPaused, true, false) {
				t.Fatalf("paused marker not set: %v", h.record().Actions)
			}
			if c.running != (h.s.Running == c.state) {
				t.Fatalf("running = %q, want the in-flight %s kept: %v", h.s.Running, c.state, c.running)
			}
		})
	}
}

func TestFR12AC4_OwnerResumesAPausedItem(t *testing.T) {
	want := map[string]State{
		"implementing": StateImplementing, "in_review": StateInReview, "addressing": StateAddressing,
		"merging": StateMerging, "blocked": StateBlocked, "escalated": StateEscalated,
		"waiting_limit": StateInReview, "paused": StateInReview,
	}
	for _, c := range claimedStates() {
		if c.state == StateAborted {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t).cmd(CommandPause).state(StatePaused)
			h.owner(MarkerPaused, false).state(want[c.name])
			if !h.markerSet(MarkerPaused, false, false) {
				t.Fatalf("paused marker not removed: %v", h.record().Actions)
			}
		})
	}
}

func TestFR12AC4_OwnerAddsNeedsOwner(t *testing.T) {
	for _, c := range claimedStates() {
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t)
			h.owner(MarkerNeedsOwner, true)
			switch {
			case c.state == StateAborted:
				h.state(StateAborted)
			case c.state == StatePaused:
				h.state(StatePaused)
				h.cmd(CommandResume).escalation(ReasonOwnerEscalated)
			case c.state == StateEscalated:
				h.state(StateEscalated)
				if h.s.Escalation.Reason != ReasonNeedsOwner {
					t.Fatalf("escalation replaced: %+v", h.s.Escalation)
				}
			case c.running:
				// Escalates after the running handler finishes.
				h.state(StateEscalated)
				if h.announced() {
					t.Fatalf("escalated before the run finished: %v", h.record().Actions)
				}
				h.result(contract.OutcomeFailed)
				h.escalation(ReasonOwnerEscalated)
				h.noRunStage()
			default:
				h.escalation(ReasonOwnerEscalated)
			}
		})
	}
}

func TestFR12AC4_OwnerClosesTheChange(t *testing.T) {
	for _, c := range claimedStates() {
		if c.state == StateImplementing {
			continue // no change yet
		}
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t)
			running := h.s.Run != nil && c.running
			h.do(ChangeClosed{At: h.at(), By: RoleOwner, Change: "57"})
			if running != h.stopped() {
				t.Fatalf("stop = %v, want %v", h.stopped(), running)
			}
			switch c.state {
			case StateAborted:
				h.state(StateAborted)
				return
			case StatePaused:
				h.state(StatePaused)
				h.cmd(CommandResume)
			}
			h.escalation(ReasonChangeClosed)
			if h.s.Change != nil || h.s.ClosedChanges != 1 {
				t.Fatalf("change = %+v, closed = %d", h.s.Change, h.s.ClosedChanges)
			}
			h.do(Resumed{At: h.at()}).state(StateImplementing)
			if r := h.runStage(); r.Branch != 2 || r.Change != "" {
				t.Fatalf("implement after a closed change = %+v, want branch 2 (D44)", r)
			}
		})
	}
}

func TestFR12AC4_OwnerAbortsTheItem(t *testing.T) {
	for _, c := range claimedStates() {
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t)
			wasAborted := c.state == StateAborted
			h.owner(MarkerAborted, true).state(StateAborted)
			if wasAborted {
				return
			}
			if !h.markerSet(MarkerAborted, true, false) {
				t.Fatalf("aborted marker missing on the item: %v", h.record().Actions)
			}
			if h.s.Change != nil && !h.markerSet(MarkerAborted, true, true) {
				t.Fatalf("aborted marker missing on the change: %v", h.record().Actions)
			}
			if h.markerSet(MarkerInProgress, false, false) {
				t.Fatal("abort removed in-progress (FR-10 AC7 keeps it)")
			}
		})
	}
}

func TestFR12AC4_OwnerReleasesTheClaim(t *testing.T) {
	releases := map[string]func(*harness) *harness{
		"removes in-progress": func(h *harness) *harness { return h.owner(MarkerInProgress, false) },
		"assigns a human":     func(h *harness) *harness { return h.do(Assigned{At: h.at(), By: RoleOwner}) },
	}
	for rname, rel := range releases {
		for _, c := range claimedStates() {
			t.Run(rname+"/"+c.name, func(t *testing.T) {
				h := rel(c.build(t))
				if c.running {
					// After the running handler finishes.
					h.state(StateReleased)
					if h.ended(ReasonReleased) {
						t.Fatal("released before the run finished")
					}
					h.result(contract.OutcomeFailed)
				}
				h.state(StateReleased)
				if !h.ended(ReasonReleased) || !h.markerSet(MarkerNeedsOwner, false, false) || !h.markerSet(MarkerPaused, false, false) ||
					!h.markerSet(MarkerBlocked, false, false) || !h.markerSet(MarkerAborted, false, false) {
					t.Fatalf("release actions incomplete: %v", h.record().Actions)
				}
				h.noRunStage()
			})
		}
	}
}

func TestFR12AC4_OwnerRemovesAbortedFromTheItem(t *testing.T) {
	h := inReview(t, Policy{}).result(contract.OutcomeNeedsOwner).cmd(CommandAbort).state(StateAborted)
	h.do(MarkerChanged{At: h.at(), By: RoleOwner, Marker: MarkerAborted, Added: false, OnChange: true}).state(StateAborted)
	h.owner(MarkerAborted, false).state(StateReleased)
	if !h.markerSet(MarkerAborted, false, true) || !h.ended(ReasonReleased) {
		t.Fatalf("release didn't clear aborted from the change: %v", h.record().Actions)
	}
}

func TestFR12AC4_OwnerClosesTheItem(t *testing.T) {
	for _, c := range claimedStates() {
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t)
			h.do(ItemClosed{At: h.at(), By: RoleOther}).state(StateClosed)
			if c.running != h.stopped() {
				t.Fatalf("stop = %v, want %v", h.stopped(), c.running)
			}
			if !h.ended(ReasonItemClosed) || !h.markerSet(MarkerInProgress, false, false) {
				t.Fatalf("close actions incomplete: %v", h.record().Actions)
			}
			if h.s.Change != nil && !h.markerSet(MarkerAborted, false, true) {
				t.Fatal("aborted not removed from the change")
			}
		})
	}
}

func TestFR12AC4_OwnerMergesTheChange(t *testing.T) {
	for _, c := range claimedStates() {
		t.Run(c.name, func(t *testing.T) {
			h := c.build(t)
			h.s.Findings = []Finding{open2} // an unresolved finding: still no follow-up
			h.do(ChangeMerged{At: h.at(), By: RoleOwner, Change: "57"}).state(StateMerged)
			if h.has(func(a Action) bool { _, ok := a.(FileFollowUp); return ok }) {
				t.Fatal("follow-up filed after a merge by hand (FR-8 AC4)")
			}
			if !h.ended(ReasonMergedByHand) || !h.has(func(a Action) bool { _, ok := a.(CloseItem); return ok }) ||
				!h.markerSet(MarkerInProgress, false, false) {
				t.Fatalf("merge-by-hand actions incomplete: %v", h.record().Actions)
			}
			if c.running != h.stopped() {
				t.Fatalf("stop = %v, want %v", h.stopped(), c.running)
			}
		})
	}
}

func TestFR12AC4_LoopMergeFeedEventIsNotAHandMerge(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil).state(StateMerging)
	h.do(ChangeMerged{At: h.at(), By: RoleLoop, Change: "57"}).state(StateMerging)
}

func TestFR12AC4_ResultOfARunningHandlerIsRecordedUnderAPause(t *testing.T) {
	h := inReview(t, Policy{}).cmd(CommandPause)
	h.reviewed([]Finding{open4}).state(StatePaused)
	h.noRunStage()
	if h.s.RoundsUsed != 1 || len(h.s.Blocking(h.p)) != 1 {
		t.Fatalf("review not recorded: rounds %d findings %v", h.s.RoundsUsed, h.s.Findings)
	}
	h.cmd(CommandResume).addressMode(contract.ModeFindings) // no completed stage runs again
}

func TestFR12AC4_EscalationFromAResultUnderAPauseWaitsForTheResume(t *testing.T) {
	h := inReview(t, Policy{}).cmd(CommandPause)
	h.result(contract.OutcomeNeedsOwner).state(StatePaused)
	if h.announced() {
		t.Fatal("paused item escalated before it resumed")
	}
	h.cmd(CommandResume).escalation(ReasonNeedsOwner)
}

func TestFR12AC4_ResultUnderAnAbortIsRecordedAndNothingStarts(t *testing.T) {
	h := inReview(t, Policy{}).cmd(CommandAbort)
	h.result(contract.OutcomeNeedsOwner).state(StateAborted)
	if h.announced() || h.s.Escalation != nil {
		t.Fatal("aborted item escalated")
	}
	h.cmd(CommandResume).state(StateAborted)
}

func TestFR12AC4_MergeCallUnderAPauseStillMerges(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil)
	h.do(MergeChecked{At: h.at(), Head: "h1", Pusher: PusherCoder, ContainsBase: true})
	if h.s.Merge == nil || !h.s.Merge.Calling {
		t.Fatal("no merge call")
	}
	h.cmd(CommandPause).state(StatePaused)
	h.do(MergeResult{At: h.at(), Outcome: MergeMerged}).state(StateMerged)
}

func TestFR12AC4_PausedMergingItemStopsChecking(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil).cmd(CommandPause)
	h.do(MergeChecked{At: h.at(), Head: "h1", Pusher: PusherCoder, ContainsBase: true}).state(StatePaused)
	if h.has(func(a Action) bool { _, ok := a.(Merge); return ok }) {
		t.Fatal("merged while paused")
	}
}

func TestFR12AC4_OwnerReopensAClosedChange(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(ChangeClosed{At: h.at(), By: RoleOwner, Change: "57"}).escalation(ReasonChangeClosed)
	h.do(ChangeReopened{At: h.at(), Change: Change{Ref: "57", Head: "h1"},
		Reviews: []ReviewRecord{{Commit: "h1", Kind: contract.KindRound, Findings: []Finding{open4}}}})
	h.addressMode(contract.ModeFindings) // adopted; next stage as in §6
	if h.s.ClosedChanges != 0 || h.s.RoundsUsed != 1 {
		t.Fatalf("closed = %d rounds = %d", h.s.ClosedChanges, h.s.RoundsUsed)
	}
}

func TestFR12AC4_ReopenedChangeOnAPausedItemCancelsTheEscalation(t *testing.T) {
	h := inReview(t, Policy{}).result(contract.OutcomeFailed).cmd(CommandPause)
	h.do(ChangeClosed{At: h.at(), By: RoleOwner, Change: "57"}).state(StatePaused)
	h.do(ChangeReopened{At: h.at(), Change: Change{Ref: "57", Head: "h1"}}).state(StatePaused)
	h.cmd(CommandResume).reviewKind(contract.KindRound)
}

func TestFR12AC4_OwnerRemovesNeedsOwner(t *testing.T) {
	h := inReview(t, Policy{}).result(contract.OutcomeNeedsOwner)
	h.owner(MarkerNeedsOwner, false).reviewKind(contract.KindRound)
	if h.runStage().Attempt != 1 {
		t.Fatal("resume must start a new attempt count (FR-10 AC8)")
	}
}

func TestFR12AC4_NeedsOwnerRemovedWhilePausedStaysPaused(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1}).reviewed([]Finding{open4})
	h.escalation(ReasonRoundLimit)
	h.cmd(CommandPause).owner(MarkerNeedsOwner, false).state(StatePaused)
	if h.s.RoundsAdded != 1 {
		t.Fatalf("rounds added = %d", h.s.RoundsAdded)
	}
	h.cmd(CommandResume).addressMode(contract.ModeFindings) // FR-10 AC5
}

func TestFR12AC4_OwnerRemovesBlocked(t *testing.T) {
	h := claimedStates()[4].build(t).state(StateBlocked)
	h.owner(MarkerBlocked, false).addressMode(contract.ModeFindings)
	// On a paused blocked item, the block ends and the item stays paused.
	h = claimedStates()[4].build(t).cmd(CommandPause)
	h.owner(MarkerBlocked, false).state(StatePaused)
	h.cmd(CommandResume).addressMode(contract.ModeFindings)
}

func TestFR12AC4_DependencyClosesWhilePaused(t *testing.T) {
	h := claimedStates()[4].build(t).cmd(CommandPause)
	h.do(DependencyClosed{At: h.at(), Dependency: "61", Done: false}).state(StatePaused)
	h.cmd(CommandResume).escalation(ReasonDependencyNotDone)
}

func TestFR12AC4_LabelsWithNoEffectOnAClaimedItem(t *testing.T) {
	for _, m := range []string{MarkerReady, "type:bug", "type:feature", MarkerFollowUp, MarkerPlan, MarkerPlanProposal, MarkerInProgress} {
		for _, added := range []bool{true, false} {
			if m == MarkerInProgress && !added {
				continue
			}
			h := inReview(t, Policy{})
			before := h.s.Clone()
			h.owner(m, added).state(StateInReview)
			if len(h.acts) != 1 || h.s.Run.RunID != before.Run.RunID {
				t.Fatalf("%s %v: acted: %v", m, added, h.record().Actions)
			}
		}
	}
}

func TestFR12AC4_OwnerCommentIsDataOnly(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(Commented{At: h.at(), By: RoleOwner}).state(StateInReview)
	if len(h.acts) != 1 {
		t.Fatalf("comment acted: %v", h.record().Actions)
	}
}

func TestFR12AC4_OwnerPushes(t *testing.T) {
	t.Run("running review is stale, its replacement keeps the kind", func(t *testing.T) {
		h := inReview(t, Policy{})
		h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
		h.reviewed(nil).reviewKind(contract.KindRound)
		if h.runStage().Review.Commit != "h2" {
			t.Fatal("replacement review not on the owner's head")
		}
	})
	t.Run("pending merge gets an owner_push review", func(t *testing.T) {
		h := inReview(t, Policy{}).reviewed(nil)
		h.do(MergeChecked{At: h.at(), Head: "h2", Pusher: PusherOwner, ContainsBase: true}).reviewKind(contract.KindOwnerPush)
	})
	t.Run("running implement or address continues", func(t *testing.T) {
		h := inReview(t, Policy{}).reviewed([]Finding{open4})
		id := h.runID()
		h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner}).state(StateAddressing)
		if h.runID() != id {
			t.Fatal("address run replaced")
		}
	})
	t.Run("paused item chooses by FR-10 AC2 on resume", func(t *testing.T) {
		h := inReview(t, Policy{}).reviewed(nil).cmd(CommandPause)
		h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner}).state(StatePaused)
		h.cmd(CommandResume).reviewKind(contract.KindOwnerPush)
	})
}

func TestFR12AC4_UntrustedPushEscalatesAfterTheHandler(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(HeadMoved{At: h.at(), Commit: "x1", Pusher: PusherUnknown}).state(StateInReview)
	h.reviewed(nil).escalation(ReasonPusherUnknown)
}

func TestFR12AC4_ManualReviewRows(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil)
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.do(MergeChecked{At: h.at(), Head: "h1", Pusher: PusherCoder, ContainsBase: true}).addressMode(contract.ModeManual)
	// A thread resolved by a loop identity arrives without HumanCleared: it
	// still blocks.
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	if len(h.s.UnclearedManual()) != 1 {
		t.Fatal("loop identity cleared a manual comment")
	}
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1", HumanCleared: true})
	if len(h.s.UnclearedManual()) != 0 {
		t.Fatal("human resolve didn't clear")
	}
}

func TestFR13AC7_LabelChangeByANonOwnerHasNoEffect(t *testing.T) {
	markers := []string{MarkerPaused, MarkerAborted, MarkerInProgress, MarkerNeedsOwner, MarkerBlocked, MarkerReady}
	for _, by := range []Role{RoleOther, RoleCoder, RoleReviewer, RoleLoop} {
		for _, c := range claimedStates() {
			for _, m := range markers {
				for _, added := range []bool{true, false} {
					h := c.build(t)
					before := h.s.Clone()
					h.do(MarkerChanged{At: h.at(), By: by, Marker: m, Added: added})
					after := h.s.Clone()
					after.Seq = before.Seq
					if !equalStates(before, after) || len(h.acts) != 1 {
						t.Fatalf("%s %s %s %v: had an effect: %v", by, c.name, m, added, h.record().Actions)
					}
				}
			}
		}
	}
}

func TestFR13AC7_RoleByAccountID(t *testing.T) {
	ids := Identities{Owners: []string{"1", "2"}, Coder: "10", Reviewer: "11", Loop: "12"}
	for id, want := range map[string]Role{"1": RoleOwner, "2": RoleOwner, "10": RoleCoder, "11": RoleReviewer, "12": RoleLoop, "3": RoleOther, "": RoleOther} {
		if got := ids.Role(id); got != want {
			t.Fatalf("Role(%q) = %s, want %s", id, got, want)
		}
	}
}

func TestFR10AC3_ResumeAfterPauseFromWaitingLimitAsksForTheLimitAgain(t *testing.T) {
	h := claimedStates()[6].build(t).cmd(CommandPause).cmd(CommandResume).state(StateInReview)
	h.do(DailyLimitReached{At: h.at(), RunID: h.runID()}).state(StateWaitingLimit)
	h.do(DayBoundary{At: h.at()}).reviewKind(contract.KindRound)
}

// FR-10 AC7: an abort while the first implement run is in flight marks the
// change that run opens (review R1).
func TestFR10AC7_AbortDuringImplementMarksTheNewChange(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil).state(StateImplementing).cmd(CommandAbort).state(StateAborted)
	if h.markerSet(MarkerAborted, true, true) {
		t.Fatal("change marked before there is a change")
	}
	h.implemented("h1").state(StateAborted)
	if h.s.Change == nil || h.s.Change.Ref != "57" {
		t.Fatalf("change = %+v", h.s.Change)
	}
	if !h.markerSet(MarkerAborted, true, true) {
		t.Fatal("the new change isn't marked aborted")
	}
	h.noRunStage()
}

// A change the item already had is marked by the abort itself, not again
// when the held result comes in (review R1).
func TestFR10AC7_AbortWithAChangeMarksItOnce(t *testing.T) {
	h := inReview(t, Policy{}).cmd(CommandAbort)
	if !h.markerSet(MarkerAborted, true, true) {
		t.Fatal("change not marked on abort")
	}
	h.reviewed(nil).state(StateAborted)
	if h.markerSet(MarkerAborted, true, true) {
		t.Fatal("change marked again by the held result")
	}
}

// A merge call that completes under a hold closes the item, so the hold's
// labels are removed from the item and the change (§5.1, review R6, D18).
func TestFR12AC4_MergeUnderAHoldClearsTheHoldLabels(t *testing.T) {
	for _, c := range []struct {
		cmd    Command
		marker string
	}{{CommandAbort, MarkerAborted}, {CommandPause, MarkerPaused}} {
		t.Run(c.marker, func(t *testing.T) {
			h := inReview(t, Policy{}).reviewed(nil)
			h.do(MergeChecked{At: h.at(), Head: "h1", Pusher: PusherCoder, ContainsBase: true})
			h.cmd(c.cmd)
			h.mergeResult(MergeMerged).state(StateMerged)
			if !h.markerSet(c.marker, false, false) {
				t.Fatalf("%s left on the item", c.marker)
			}
			if !h.markerSet(MarkerAborted, false, true) {
				t.Fatal("aborted not cleared on the change")
			}
		})
	}
}

// A merge call that fails under a hold leaves the hold's labels: the item
// stays held (review R6, negative case).
func TestFR12AC4_FailedMergeUnderAnAbortKeepsTheLabels(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil)
	h.do(MergeChecked{At: h.at(), Head: "h1", Pusher: PusherCoder, ContainsBase: true})
	h.cmd(CommandAbort).mergeResult(MergeFailed).state(StateAborted)
	if h.markerSet(MarkerAborted, false, false) || h.markerSet(MarkerAborted, false, true) {
		t.Fatal("aborted removed from an item that is still aborted")
	}
}
