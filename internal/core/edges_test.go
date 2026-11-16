package core

import (
	"slices"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
)

// Edges of the PRODUCT-SPEC §6 diagram that touch implementing, in_review,
// addressing, escalated (from a stage) and next_stage.

func TestFR3AC1_ClaimWithNoChangeImplements(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil).state(StateImplementing)
	r := h.runStage()
	if r.Stage != contract.StageImplement || r.Attempt != 1 || r.RunID != "item/p/42#1:implement:1:1" {
		t.Fatalf("run = %+v", r)
	}
	if !r.Deadline.Equal(h.record().At.Add(DefaultTimeLimit)) {
		t.Fatalf("deadline = %v, want 2h after the event (FR-16 AC1)", r.Deadline)
	}
}

func TestFR3AC1_ImplementDoneGoesToRoundOneReview(t *testing.T) {
	h := inReview(t, Policy{})
	r := h.runStage()
	if r.Change != "57" || r.Review.Commit != "h1" || r.Review.Round != 1 {
		t.Fatalf("run = %+v", r)
	}
}

func TestFR3AC3_ExistingChangeIsAdoptedNotImplemented(t *testing.T) {
	h := newHarness(t, Policy{}).start(&Change{Ref: "57", Head: "h1", Pusher: PusherOwner})
	h.reviewKind(contract.KindRound) // first review is round (D4)
}

func TestFR3AC3_AdoptedChangeRebuildsRoundsFromRecords(t *testing.T) {
	h := newHarness(t, Policy{}).start(&Change{Ref: "57", Head: "h2"},
		ReviewRecord{Commit: "h1", Kind: contract.KindRound, Findings: []Finding{open4}},
		ReviewRecord{Commit: "h1", Kind: contract.KindRound, Stale: true},
		ReviewRecord{Commit: "h2", Kind: contract.KindRound, Findings: []Finding{open4}},
	)
	if h.s.RoundsUsed != 2 {
		t.Fatalf("rounds = %d, want 2 (stale not counted)", h.s.RoundsUsed)
	}
	h.addressMode(contract.ModeFindings) // head reviewed, blocking: rule 5
}

func TestFR3AC3_AdoptedChangeWithCleanReviewMerges(t *testing.T) {
	h := newHarness(t, Policy{}).start(&Change{Ref: "57", Head: "h1"},
		ReviewRecord{Commit: "h1", Kind: contract.KindRound, Findings: []Finding{open2}})
	h.state(StateMerging)
}

func TestFR18AC1_FailedRetriesSameStageWithNextAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    func(*testing.T) *harness
	}{
		{"implement", func(t *testing.T) *harness { return newHarness(t, Policy{}).start(nil) }},
		{"review", func(t *testing.T) *harness { return inReview(t, Policy{}) }},
		{"address", func(t *testing.T) *harness {
			return inReview(t, Policy{}).reviewed([]Finding{open4}).addressMode(contract.ModeFindings)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h(t)
			st, stage := h.s.State, h.s.Run.Stage
			h.result(contract.OutcomeFailed).state(st)
			if r := h.runStage(); r.Stage != stage || r.Attempt != 2 || !strings.Contains(r.RunID, ":"+string(stage)+":2:") {
				t.Fatalf("retry = %+v", r)
			}
			h.result(contract.OutcomeFailed)
			h.escalation(ReasonRetriesExhausted)
		})
	}
}

func TestFR18AC3_ResultNotAllowedForStageIsFailed(t *testing.T) {
	h := inReview(t, Policy{})
	h.result(contract.OutcomeBlocked).reviewKind(contract.KindRound) // review can't be blocked
	if h.runStage().Attempt != 2 {
		t.Fatal("disallowed result should count as a failed attempt")
	}
	h = inReview(t, Policy{})
	h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageImplement, Outcome: contract.OutcomeDone})
	if h.runStage().Attempt != 2 {
		t.Fatal("a result for another stage should be failed")
	}
	h.result(contract.OutcomeDone) // done with no review object
	h.escalation(ReasonRetriesExhausted)
}

func TestFR18AC1_ResultForAnotherRunIsIgnored(t *testing.T) {
	h := inReview(t, Policy{})
	before := h.s
	h.do(StageResult{At: h.at(), RunID: "item/p/42#1:review:1:b", Stage: contract.StageReview, Outcome: contract.OutcomeFailed})
	if h.record().Rule != "ignored" || h.s.Run.RunID != before.Run.RunID {
		t.Fatalf("late result applied: %+v", h.record())
	}
}

func TestFR10AC4_NeedsOwnerEscalatesFromEachStage(t *testing.T) {
	for _, h := range []*harness{
		newHarness(t, Policy{}).start(nil),
		inReview(t, Policy{}),
		inReview(t, Policy{}).reviewed([]Finding{open4}),
	} {
		st := h.s.Run.Stage
		h.result(contract.OutcomeNeedsOwner)
		if ps := h.escalation(ReasonNeedsOwner); ps.Stage != st {
			t.Fatalf("stage = %s, want %s", ps.Stage, st)
		}
	}
}

func TestFR18AC4_TimeLimitEscalatesFromEachStage(t *testing.T) {
	for _, h := range []*harness{
		newHarness(t, Policy{}).start(nil),
		inReview(t, Policy{}),
		inReview(t, Policy{}).reviewed([]Finding{open4}),
	} {
		h.do(StageTimedOut{At: h.at(), RunID: h.runID()})
		h.escalation(ReasonTimeLimit)
	}
}

func blockedResult(h *harness, dep string, st DependencyState) *harness {
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: h.s.Run.Stage, Outcome: contract.OutcomeBlocked,
		Blocked: &BlockedOutcome{Dependency: dep, State: st}})
}

func TestFR18AC1_BlockedOnOpenDependency(t *testing.T) {
	for _, h := range []*harness{
		newHarness(t, Policy{}).start(nil),
		inReview(t, Policy{}).reviewed([]Finding{open4}),
	} {
		blockedResult(h, "61", DependencyOpen).state(StateBlocked)
		if h.s.BlockedOn != "61" || !slices.Contains(h.record().Actions, "set_marker") {
			t.Fatalf("blocked state = %+v", h.s)
		}
	}
}

func TestFR18AC1_BlockedWithNoDependencyEscalates(t *testing.T) {
	for _, h := range []*harness{
		newHarness(t, Policy{}).start(nil),
		inReview(t, Policy{}).reviewed([]Finding{open4}),
	} {
		blockedResult(h, "", "")
		h.escalation(ReasonBlockedNoDependency)
	}
}

func TestFR18AC1_BlockedOnClosedDependency(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil)
	blockedResult(h, "61", DependencyDone).state(StateImplementing) // next stage at once (D14)
	h = newHarness(t, Policy{}).start(nil)
	blockedResult(h, "61", DependencyNotDone)
	if ps := h.escalation(ReasonDependencyNotDone); ps.Dependency != "61" {
		t.Fatalf("status = %+v", ps)
	}
}

func TestFR18AC1_BlockedExits(t *testing.T) {
	// blocked --> next_stage : dependency done
	h := newHarness(t, Policy{}).start(nil)
	blockedResult(h, "61", DependencyOpen)
	h.do(DependencyClosed{At: h.at(), Dependency: "60", Done: true}).state(StateBlocked) // not ours
	h.do(DependencyClosed{At: h.at(), Dependency: "61", Done: true}).state(StateImplementing)
	if h.runStage().Attempt != 1 {
		t.Fatal("next stage after a block starts a new run")
	}
	// blocked --> next_stage : owner removes blocked
	h = inReview(t, Policy{}).reviewed([]Finding{open4})
	blockedResult(h, "61", DependencyOpen)
	h.do(BlockRemoved{At: h.at()}).addressMode(contract.ModeFindings)
	// blocked --> escalated : dependency closed without being done
	h = newHarness(t, Policy{}).start(nil)
	blockedResult(h, "61", DependencyOpen)
	h.do(DependencyClosed{At: h.at(), Dependency: "61", Done: false})
	h.escalation(ReasonDependencyNotDone)
}

func TestFR6AC2_ReviewWithNoBlockingFindingMerges(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open2, fixed4}).state(StateMerging)
	var cm *CheckMerge
	for _, a := range h.acts {
		if c, ok := a.(CheckMerge); ok {
			cm = &c
		}
	}
	if cm == nil || cm.Head != "h1" || cm.Change != "57" {
		t.Fatalf("check merge = %+v", cm)
	}
}

func TestFR6AC2_ThresholdFromPolicy(t *testing.T) {
	inReview(t, Policy{BlockingThreshold: 2}).reviewed([]Finding{open2}).addressMode(contract.ModeFindings)
	inReview(t, Policy{BlockingThreshold: 5}).reviewed([]Finding{open4}).state(StateMerging)
	partial := f("F1", 4, contract.StatusPartiallyAddressed)
	inReview(t, Policy{}).reviewed([]Finding{partial}).addressMode(contract.ModeFindings)
	withdrawn := f("F1", 5, contract.StatusWithdrawn)
	inReview(t, Policy{}).reviewed([]Finding{withdrawn}).state(StateMerging)
}

func TestFR5AC1_BlockingReviewAddressesEveryUnresolvedFindingAndManualComment(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-2", Empty: true})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-3", HumanCleared: true})
	h.reviewed([]Finding{open4, open2, fixed4}).addressMode(contract.ModeFindings)
	a := h.runStage().Address
	if !slices.Equal(a.AnswerFindings, []string{"F1", "F2"}) || !slices.Equal(a.AnswerManual, []string{"MC-1"}) {
		t.Fatalf("address input = %+v", a)
	}
}

func TestFR6AC3_LastRoundStillBlockingEscalatesListingManualComments(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.reviewed([]Finding{open4, open2})
	ps := h.escalation(ReasonRoundLimit)
	if !slices.Equal(ps.FindingIDs, []string{"F1"}) || !slices.Equal(ps.CommentIDs, []string{"MC-1"}) {
		t.Fatalf("escalation = %+v", ps)
	}
	if !h.s.Escalation.AfterBlockingRound {
		t.Fatal("FR-6 AC3 escalation must remember the blocking round")
	}
}

func TestFR6AC3_UncountedReviewWithAllowanceUsedEscalates(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1}).reviewed([]Finding{open2}) // round 1, clean
	h.s.State, h.s.Run = StateEscalated, nil                          // a wait, so rules choose
	h.s.Escalation = &Escalation{Reason: ReasonNeedsOwner}
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindOwnerPush)
	h.reviewed([]Finding{open2, open5})
	h.escalation(ReasonRoundLimit)
}

func TestFR6AC6_AddressDoneGoesToReviewOfItsKind(t *testing.T) {
	// findings mode -> round
	h := inReview(t, Policy{}).reviewed([]Finding{open4}).addressed("h2").reviewKind(contract.KindRound)
	if h.runStage().Review.Round != 2 || h.runStage().Review.Commit != "h2" {
		t.Fatalf("review = %+v", h.runStage().Review)
	}
	// manual mode -> manual_reply, even when the head didn't move (FR-7 AC10)
	h = inReview(t, Policy{}).reviewed([]Finding{open2})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.startAddress(contract.ModeManual)
	h.addressed("h1").reviewKind(contract.KindManualReply)
	if rv := h.runStage().Review; rv.Round != 0 || !slices.Equal(rv.RuleManual, []string{"MC-1"}) {
		t.Fatalf("manual_reply review = %+v", rv)
	}
}

// startAddress drives the T6 entry point from a test.
func (h *harness) startAddress(mode contract.AddressMode) *harness {
	h.t.Helper()
	return h.call(func(d *decision) { d.startAddress(mode) })
}

func (h *harness) baseUpdate() *harness {
	h.t.Helper()
	return h.call(func(d *decision) { d.beginBaseUpdate() })
}

// call runs one decision step directly, the way T6's merge rules will.
func (h *harness) call(fn func(*decision)) *harness {
	h.t.Helper()
	d := &decision{s: h.s.Clone(), p: h.p, at: h.at().Time}
	d.s.Seq++
	fn(d)
	h.s, h.acts = d.s, append(d.actions, RecordDecision{Record: DecisionRecord{Rule: d.rule, To: d.s.State}})
	return h
}

func TestFR6AC8_AddressDoneWithAllowanceUsedUpRefusesTheRound(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	h.s.RoundsUsed = h.s.Allowance(h.p) // allowance used up while addressing
	h.addressed("h2")
	ps := h.escalation(ReasonRoundLimit)
	h.noRunStage()
	if h.s.Escalation.AfterBlockingRound || len(ps.FindingIDs) != 0 {
		t.Fatalf("FR-6 AC8 escalation = %+v", h.s.Escalation)
	}
	// FR-10 AC5: the resume starts with the review that was refused.
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound)
	if r := h.runStage().Review; r.Commit != "h2" || r.Round != h.s.RoundsUsed+1 {
		t.Fatalf("refused review = %+v", r)
	}
}

func TestFR6AC8_UncountedReviewsIgnoreTheAllowance(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1}).reviewed([]Finding{open2})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.startAddress(contract.ModeManual).addressed("h1").reviewKind(contract.KindManualReply) // FR-6 AC9
	h.reviewed([]Finding{open2}, ManualRuling{Comment: "MC-1", Ruling: contract.RulingOpen}).state(StateMerging)
	h.baseUpdate()
	h.do(BaseUpdated{At: h.at(), Head: "h3"}).reviewKind(contract.KindBaseUpdate)
}

func TestFR4AC3_StaleReviewIsReviewedAgainWithTheSameKind(t *testing.T) {
	for _, kind := range []contract.ReviewKind{contract.KindRound, contract.KindManualReply, contract.KindBaseUpdate, contract.KindOwnerPush} {
		t.Run(string(kind), func(t *testing.T) {
			h := inReview(t, Policy{})
			h.s.Run.Review.Kind = kind // as if this kind was chosen
			h.s.NextKind = kind
			h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
			used, bu := h.s.RoundsUsed, h.s.BaseUpdates
			h.reviewed([]Finding{open2}) // on h1, now stale
			h.reviewKind(kind)
			if h.s.RoundsUsed != used || h.s.BaseUpdates != bu || h.s.LatestReview != nil {
				t.Fatal("a stale review must not count or be used (FR-6 AC5)")
			}
			if r := h.runStage(); r.Review.Commit != "h2" || r.Attempt != 1 {
				t.Fatalf("replacement = %+v", r)
			}
		})
	}
}

func TestFR4AC3_WorkerMarkedStaleReviewIsNotUsed(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageReview, Outcome: contract.OutcomeDone,
		Review: &ReviewOutcome{Commit: "h1", Kind: contract.KindRound, Stale: true, Findings: []Finding{open2}}})
	h.reviewKind(contract.KindRound)
	if h.s.RoundsUsed != 0 {
		t.Fatal("stale review counted")
	}
}

func TestFR4AC8_ManualRulingAddressedClearsTheComment(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open2})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-2"})
	h.startAddress(contract.ModeManual).addressed("h2")
	h.reviewed([]Finding{open2},
		ManualRuling{Comment: "MC-1", Ruling: contract.RulingAddressed},
		ManualRuling{Comment: "MC-2", Ruling: contract.RulingOpen})
	if got := h.s.UnclearedManual(); !slices.Equal(got, []string{"MC-2"}) || len(h.s.PendingRulings) != 0 {
		t.Fatalf("uncleared = %v, pending = %v", got, h.s.PendingRulings)
	}
	// A human reopening the thread drops the ruling (FR-7 AC9).
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1", Reopened: true})
	if got := h.s.UnclearedManual(); !slices.Equal(got, []string{"MC-1", "MC-2"}) {
		t.Fatalf("after reopen uncleared = %v", got)
	}
}

func TestFR4AC8_StaleReviewRulingsAreIgnored(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open2})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.startAddress(contract.ModeManual).addressed("h1")
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
	h.reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingAddressed})
	if len(h.s.UnclearedManual()) != 1 || !slices.Equal(h.s.PendingRulings, []string{"MC-1"}) {
		t.Fatalf("stale ruling applied: %+v", h.s.ManualComments)
	}
	h.reviewKind(contract.KindManualReply)
}

func TestFR5AC2_AddressWithoutRequiredRepliesFails(t *testing.T) {
	setup := func() *harness {
		h := inReview(t, Policy{})
		h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
		return h.reviewed([]Finding{open4, open2})
	}
	cases := []struct {
		name string
		o    AddressOutcome
		ok   bool
	}{
		{"all replied, no push", AddressOutcome{Head: "h1",
			Replies:       []ReplyOutcome{{To: "F1", Counts: true}, {To: "F2", Counts: true}},
			ManualReplies: []ReplyOutcome{{To: "MC-1", Counts: true}}}, true},
		{"head moved, manual replied", AddressOutcome{Head: "h2", ManualReplies: []ReplyOutcome{{To: "MC-1", Counts: true}}}, true},
		{"finding missing, no push", AddressOutcome{Head: "h1",
			Replies: []ReplyOutcome{{To: "F1", Counts: true}}, ManualReplies: []ReplyOutcome{{To: "MC-1", Counts: true}}}, false},
		{"deferred to a closed item, no push", AddressOutcome{Head: "h1",
			Replies:       []ReplyOutcome{{To: "F1", Counts: true}, {To: "F2", Kind: contract.ReplyDeferred, Counts: false}},
			ManualReplies: []ReplyOutcome{{To: "MC-1", Counts: true}}}, false},
		{"manual missing even though head moved", AddressOutcome{Head: "h2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup()
			o := tc.o
			h.addressedWith(&o)
			if tc.ok {
				h.reviewKind(contract.KindRound)
				return
			}
			h.addressMode(contract.ModeFindings)
			if h.runStage().Attempt != 2 {
				t.Fatal("FR-5 AC2 violation should be a failed attempt")
			}
		})
	}
}

func TestFR5AC6_BaseUpdateWithoutConflictsRunsNoAgent(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil).state(StateMerging)
	h.baseUpdate().state(StateAddressing)
	h.noRunStage()
	if _, ok := h.acts[0].(UpdateBase); !ok {
		t.Fatalf("actions = %v", h.acts)
	}
	h.do(BaseUpdated{At: h.at(), Head: "h2"}).reviewKind(contract.KindBaseUpdate)
	h.reviewed(nil).state(StateMerging)
	if h.s.BaseUpdates != 1 || h.s.RoundsUsed != 1 {
		t.Fatalf("base updates = %d, rounds = %d", h.s.BaseUpdates, h.s.RoundsUsed)
	}
}

func TestFR5AC6_BaseUpdateWithConflictsIsAnAddressRun(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open2}).baseUpdate()
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.do(BaseConflicts{At: h.at()}).addressMode(contract.ModeBaseUpdate)
	if a := h.runStage().Address; len(a.AnswerFindings)+len(a.AnswerManual) != 0 {
		t.Fatalf("base_update mode answers nothing: %+v", a)
	}
	h.addressedWith(&AddressOutcome{Head: "h1"}).addressMode(contract.ModeBaseUpdate) // head must move
	h.addressedWith(&AddressOutcome{Head: "h3"}).reviewKind(contract.KindBaseUpdate)
}

func TestFR5AC6_BaseUpdateRefusedChecksMergeAgain(t *testing.T) {
	h := inReview(t, Policy{}).reviewed(nil).baseUpdate()
	h.do(BaseUpdateRefused{At: h.at()}).state(StateMerging)
	h.do(BaseUpdated{At: h.at(), Head: "h9"}).state(StateMerging) // no longer updating: ignored
}

func TestFR6AC7_BaseUpdateLimitEscalates(t *testing.T) {
	h := inReview(t, Policy{MaxBaseUpdates: 1}).reviewed(nil).baseUpdate()
	h.do(BaseUpdated{At: h.at(), Head: "h2"}).reviewed(nil).state(StateMerging)
	h.baseUpdate()
	h.escalation(ReasonBaseUpdateLimit)
}

func TestFR16AC3_DailyLimitWaitsBeforeEachAgentStage(t *testing.T) {
	for _, h := range []*harness{
		newHarness(t, Policy{}).start(nil),
		inReview(t, Policy{}),
		inReview(t, Policy{}).reviewed([]Finding{open4}),
	} {
		st := h.s.Run.Stage
		h.do(DailyLimitReached{At: h.at(), RunID: h.runID()}).state(StateWaitingLimit)
		h.noRunStage()
		h.do(DayBoundary{At: h.at()}).state(stateFor(st))
		if h.runStage().Stage != st || h.runStage().Attempt != 1 {
			t.Fatalf("after the day boundary: %+v", h.runStage())
		}
	}
}

func TestFR16AC3_WaitingLimitKeepsAttempt(t *testing.T) {
	h := newHarness(t, Policy{Stages: map[contract.Stage]StageLimits{contract.StageImplement: {Attempts: 3}}}).start(nil)
	h.result(contract.OutcomeFailed)
	h.do(DailyLimitReached{At: h.at(), RunID: h.runID()})
	h.do(DayBoundary{At: h.at()}).state(StateImplementing)
	if h.runStage().Attempt != 2 {
		t.Fatalf("attempt = %d, want 2 (D12)", h.runStage().Attempt)
	}
}

func TestFR10AC2_EventsOutsideTheirStateAreIgnored(t *testing.T) {
	h := inReview(t, Policy{})
	for _, e := range []Event{
		DayBoundary{At: h.at()}, Resumed{At: h.at()}, BlockRemoved{At: h.at()},
		DependencyClosed{At: h.at(), Dependency: "1", Done: true}, BaseConflicts{At: h.at()},
		Started{At: h.at(), WorkflowID: "x"},
	} {
		before := h.s
		h.do(e)
		if h.record().Rule != "ignored" || h.s.State != before.State || h.s.Run.RunID != before.Run.RunID {
			t.Fatalf("%s changed the state: %+v", e.Name(), h.record())
		}
	}
}

func TestFR18AC2_DoneWithoutItsContentIsFailed(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil)
	h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageImplement, Outcome: contract.OutcomeDone,
		Implement: &ImplementOutcome{Change: "57"}}).state(StateImplementing)
	if h.runStage().Attempt != 2 {
		t.Fatal("implement done without a head should be failed")
	}
	h = inReview(t, Policy{}).reviewed([]Finding{open4})
	h.addressedWith(nil).addressMode(contract.ModeFindings)
	if h.runStage().Attempt != 2 {
		t.Fatal("address done without replies should be failed")
	}
}

func TestTECH9_WritesInOneDecisionHaveDistinctKeys(t *testing.T) {
	// Review R2: clearing blocked and setting needs_owner in one decision.
	h := newHarness(t, Policy{}).start(nil)
	blockedResult(h, "61", DependencyOpen)
	h.do(DependencyClosed{At: h.at(), Dependency: "61", Done: false})
	h.escalation(ReasonDependencyNotDone)
	var keys []string
	for _, a := range h.acts {
		if m, ok := a.(SetMarker); ok {
			keys = append(keys, m.Key)
		}
	}
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("marker keys = %v, want two distinct keys", keys)
	}
}

func TestFR5AC6_BaseUpdateOutcomesOutsideABaseUpdateAreIgnored(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(BaseUpdateRefused{At: h.at()}).reviewKind(contract.KindRound)
	if h.record().Rule != "ignored" {
		t.Fatalf("rule = %q", h.record().Rule)
	}
	if stateFor(StageMerge) != StateMerging || stateFor(contract.StageMint) != StateMerging {
		t.Fatal("stateFor: a non-agent stage maps to merging")
	}
}

func TestFR10AC2_Rule3_DefaultsToManualReplyWhenNoKindIsWaiting(t *testing.T) {
	// Replies waiting for a ruling with no kind recorded (D22): rule 3 picks
	// a manual_reply review.
	h := inReview(t, Policy{}).reviewed([]Finding{open2})
	h.s.State, h.s.Escalation = StateEscalated, &Escalation{Reason: ReasonNeedsOwner}
	h.s.PendingRulings, h.s.NextKind = []string{"MC-1"}, ""
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindManualReply)
	wantRule(t, h, "3")
}
