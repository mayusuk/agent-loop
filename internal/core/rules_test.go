package core

import (
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
)

// The six TECHNICAL-SPEC §8.2 rules, both pre-stage checks, round counting
// (FR-6 AC5, AC6, D36), resumes (FR-10 AC5, AC8) and crash reruns
// (FR-16 AC1, D42).

// escalated returns h after an owner-visible escalation, ready to resume.
func escalate(h *harness) *harness {
	h.result(contract.OutcomeNeedsOwner)
	h.escalation(ReasonNeedsOwner)
	return h
}

func wantRule(t *testing.T, h *harness, n string) {
	t.Helper()
	if !strings.Contains(h.record().Rule, "rule "+n) {
		t.Fatalf("rule = %q, want rule %s", h.record().Rule, n)
	}
}

func TestFR10AC2_Rule1_RoundLimitResumeStartsWithAddressEvenAfterOwnerPush(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1}).reviewed([]Finding{open5})
	h.escalation(ReasonRoundLimit)
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
	h.do(Resumed{At: h.at()}).addressMode(contract.ModeFindings)
	wantRule(t, h, "1")
	// The address run is followed by a counted round in the added allowance.
	h.addressed("h3").reviewKind(contract.KindRound)
	if h.runStage().Review.Round != 2 {
		t.Fatalf("round = %d", h.runStage().Review.Round)
	}
}

func TestFR10AC2_Rule2_NoChangeImplements(t *testing.T) {
	h := escalate(newHarness(t, Policy{}).start(nil))
	h.do(Resumed{At: h.at()}).state(StateImplementing)
	wantRule(t, h, "2")
}

func TestFR10AC2_Rule3_UnruledManualRepliesGetAManualReplyReview(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open2})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.startAddress(contract.ModeManual).addressed("h1") // head didn't move
	escalate(h)
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindManualReply)
	wantRule(t, h, "3")
}

func TestFR10AC2_Rule3_FindingsRunRepliesKeepTheRoundKind(t *testing.T) {
	// D5: a findings run that also replied to a manual comment is followed
	// by a round, also after a wait.
	h := inReview(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.reviewed([]Finding{open4}).addressed("h2").reviewKind(contract.KindRound)
	escalate(h)
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound)
	wantRule(t, h, "3")
}

func TestFR10AC2_Rule4_UnreviewedHeadIsReviewed(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	escalate(h) // escalated while addressing
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherOwner})
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindOwnerPush)
	wantRule(t, h, "4")
}

func TestFR10AC2_Rule4_AddressRunWaitingForItsReview(t *testing.T) {
	// An address run that replied without pushing, then a wait: its review
	// is still due, not another address run (work plan D26).
	h := inReview(t, Policy{}).reviewed([]Finding{open4}).addressed("h1").reviewKind(contract.KindRound)
	h.do(DailyLimitReached{At: h.at(), RunID: h.runID()})
	h.do(DayBoundary{At: h.at()}).reviewKind(contract.KindRound)
	wantRule(t, h, "4")
}

func TestFR10AC2_Rule5_BlockingReviewGetsAnAddressRun(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	escalate(h)
	h.do(Resumed{At: h.at()}).addressMode(contract.ModeFindings)
	wantRule(t, h, "5")
}

func TestFR10AC2_Rule6_CleanReviewOnHeadMerges(t *testing.T) {
	h := newHarness(t, Policy{}).start(&Change{Ref: "57", Head: "h1"},
		ReviewRecord{Commit: "h1", Kind: contract.KindRound})
	h.state(StateMerging)
	wantRule(t, h, "6")
}

func TestFR6AC8_PreStageCheck1_CountedRoundRefusedWhenAllowanceUsedUp(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1}).reviewed([]Finding{open4})
	h.escalation(ReasonRoundLimit)
	h.do(Resumed{At: h.at()}).addressMode(contract.ModeFindings) // allowance 2 now
	h.addressed("h2").reviewed([]Finding{open4})                 // round 2, still blocking
	h.escalation(ReasonRoundLimit)
	// Without the resume rule, a round would now be refused: force a wait
	// that comes back through rule 4.
	h.s.Escalation.AfterBlockingRound = false
	h.do(HeadMoved{At: h.at(), Commit: "h3", Pusher: PusherCoder})
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound) // allowance 3 after the resume
	h.reviewed([]Finding{open4})
	h.escalation(ReasonRoundLimit)
	if h.s.RoundsUsed != 3 || h.s.Allowance(h.p) != 3 {
		t.Fatalf("rounds = %d of %d", h.s.RoundsUsed, h.s.Allowance(h.p))
	}
}

func TestFR6AC8_PreStageCheck1_RunsOnEveryStartNotOnlyAfterAWait(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	h.s.RoundsUsed = h.p.maxRounds() // rounds 1 to 3 used while this address run ran
	h.addressed("h2")
	h.escalation(ReasonRoundLimit)
}

func TestFR16AC3_PreStageCheck2_DailyLimitAfterAWait(t *testing.T) {
	h := escalate(inReview(t, Policy{}).reviewed([]Finding{open4}))
	h.do(Resumed{At: h.at()}).addressMode(contract.ModeFindings)
	h.do(DailyLimitReached{At: h.at(), RunID: h.runID()}).state(StateWaitingLimit)
	h.do(DayBoundary{At: h.at()}).addressMode(contract.ModeFindings)
}

func TestFR16AC3_PreStageChecksInOrder_RoundLimitBeforeDailyLimit(t *testing.T) {
	// When the round is refused, no RunStage is emitted, so the workflow
	// never asks for a daily unit: the round check comes first.
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	h.s.RoundsUsed = h.s.Allowance(h.p)
	h.addressed("h2")
	h.noRunStage()
	h.escalation(ReasonRoundLimit)
}

func TestFR10AC5_ResumeAfterRoundLimitAddsResumeRounds(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1, ResumeRounds: 2}).reviewed([]Finding{open4})
	h.do(Resumed{At: h.at()})
	var ps *PostStatus
	for _, a := range h.acts {
		if p, ok := a.(PostStatus); ok && p.Event == contract.EventResume {
			ps = &p
		}
	}
	if ps == nil || ps.RoundsAdded == nil || *ps.RoundsAdded != 2 || h.s.Allowance(h.p) != 3 {
		t.Fatalf("resume status = %+v, allowance %d", ps, h.s.Allowance(h.p))
	}
	// Another escalation adds nothing.
	escalate(h)
	h.do(Resumed{At: h.at()})
	if h.s.RoundsAdded != 2 {
		t.Fatalf("rounds added = %d", h.s.RoundsAdded)
	}
}

func TestFR10AC8_ResumeGivesANewAttemptCount(t *testing.T) {
	h := inReview(t, Policy{})
	h.result(contract.OutcomeFailed).result(contract.OutcomeFailed)
	h.escalation(ReasonRetriesExhausted)
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound)
	if r := h.runStage(); r.Attempt != 1 || !strings.HasSuffix(r.RunID, ":1:a") {
		t.Fatalf("run = %+v", r)
	}
}

func TestFR6AC5_RoundCountingPerKind(t *testing.T) {
	type rec = ReviewRecord
	cases := []struct {
		name          string
		records       []rec
		rounds, bases int
	}{
		{"round", []rec{{Kind: contract.KindRound}}, 1, 0},
		{"base_update", []rec{{Kind: contract.KindBaseUpdate}}, 0, 1},
		{"manual_reply", []rec{{Kind: contract.KindManualReply}}, 0, 0},
		{"owner_push", []rec{{Kind: contract.KindOwnerPush}}, 0, 0},
		{"stale round", []rec{{Kind: contract.KindRound, Stale: true}}, 0, 0},
		{"stale base_update", []rec{{Kind: contract.KindBaseUpdate, Stale: true}}, 0, 0},
		{"mixed", []rec{{Kind: contract.KindRound}, {Kind: contract.KindRound, Stale: true}, {Kind: contract.KindRound},
			{Kind: contract.KindBaseUpdate}, {Kind: contract.KindOwnerPush}}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r, b := CountReviews(tc.records); r != tc.rounds || b != tc.bases {
				t.Fatalf("CountReviews = %d, %d; want %d, %d", r, b, tc.rounds, tc.bases)
			}
		})
	}
}

func TestFR6AC6_LiveRoundCountingPerKind(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4}) // round 1
	h.addressed("h2").reviewed([]Finding{fixed4})         // round 2, clean
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.startAddress(contract.ModeManual).addressed("h2").reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingAddressed})
	h.baseUpdate()
	h.do(BaseUpdated{At: h.at(), Head: "h3"}).reviewed(nil)
	h.s.State, h.s.Escalation = StateEscalated, &Escalation{Reason: ReasonNeedsOwner}
	h.do(HeadMoved{At: h.at(), Commit: "h4", Pusher: PusherOwner})
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindOwnerPush).reviewed(nil)
	if h.s.RoundsUsed != 2 || h.s.BaseUpdates != 1 {
		t.Fatalf("rounds = %d, base updates = %d", h.s.RoundsUsed, h.s.BaseUpdates)
	}
}

func TestFR6AC6_OwnerPushBeforeAPendingReviewKeepsItsKind(t *testing.T) {
	// D6: an owner push between an address run and its review doesn't make
	// that review owner_push.
	h := inReview(t, Policy{}).reviewed([]Finding{open4}).addressed("h2")
	h.do(DailyLimitReached{At: h.at(), RunID: h.runID()})
	h.do(HeadMoved{At: h.at(), Commit: "h3", Pusher: PusherOwner})
	h.do(DayBoundary{At: h.at()}).reviewKind(contract.KindRound)
}

func TestFR6AC6_CoderPushIsNotAnOwnerPush(t *testing.T) {
	h := escalate(inReview(t, Policy{}).reviewed([]Finding{open2}).baseUpdate().
		do(BaseUpdated{At: At{t0}, Head: "h2"}))
	h.s.NextKind = "" // as if the base_update review were lost; D24
	h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound)
}

func TestD36_OwnerPushDuringRoundReviewCostsNoRound(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4}).addressed("h2").reviewKind(contract.KindRound)
	h.do(HeadMoved{At: h.at(), Commit: "h3", Pusher: PusherOwner})
	h.reviewed([]Finding{fixed4}).reviewKind(contract.KindRound) // stale; replacement is still round
	if h.s.RoundsUsed != 1 || h.runStage().Review.Round != 2 {
		t.Fatalf("rounds = %d, next round %d", h.s.RoundsUsed, h.runStage().Review.Round)
	}
	h.reviewed([]Finding{fixed4}).state(StateMerging)
	if h.s.RoundsUsed != 2 {
		t.Fatalf("rounds = %d", h.s.RoundsUsed)
	}
}

func TestFR16AC1_CrashRerunsKeepTheAttemptAndFourthCrashEscalates(t *testing.T) {
	h := inReview(t, Policy{})
	for i, suffix := range []string{"b", "c", "d"} {
		h.do(StageCrashed{At: h.at(), RunID: h.runID()}).reviewKind(contract.KindRound)
		r := h.runStage()
		if r.Attempt != 1 || !strings.HasSuffix(r.RunID, ":review:1:"+suffix) || h.s.Run.Crashes != i+1 {
			t.Fatalf("rerun %d = %+v", i+1, r)
		}
		if !r.Deadline.Equal(h.record().At.Add(DefaultTimeLimit)) {
			t.Fatalf("rerun %d deadline not renewed", i+1)
		}
	}
	h.do(StageCrashed{At: h.at(), RunID: h.runID()})
	h.escalation(ReasonWorkerLost)
}

func TestNFR1AC2_CrashCountRestartsWithEachAttempt(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil)
	h.do(StageCrashed{At: h.at(), RunID: h.runID()}).do(StageCrashed{At: h.at(), RunID: h.runID()})
	h.result(contract.OutcomeFailed)
	if h.s.Run.Attempt != 2 || h.s.Run.Crashes != 0 {
		t.Fatalf("run = %+v", h.s.Run)
	}
	for range 3 {
		h.do(StageCrashed{At: h.at(), RunID: h.runID()}).state(StateImplementing)
	}
	if h.s.Run.Attempt != 2 {
		t.Fatal("crash used an attempt")
	}
	h.do(StageCrashed{At: h.at(), RunID: h.runID()})
	h.escalation(ReasonWorkerLost)
}

func TestNFR1AC2_CrashOfAnOldRunIsIgnored(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil)
	old := h.runID()
	h.do(StageCrashed{At: h.at(), RunID: old})
	h.do(StageCrashed{At: h.at(), RunID: old})
	if h.s.Run.Crashes != 1 || h.record().Rule != "ignored" {
		t.Fatalf("old crash applied: %+v", h.s.Run)
	}
}

func TestFR16AC1_StageLimitsFromPolicy(t *testing.T) {
	p := Policy{Stages: map[contract.Stage]StageLimits{contract.StageImplement: {TimeLimit: 30 * 60e9, Attempts: 1}}}
	h := newHarness(t, p).start(nil)
	if !h.runStage().Deadline.Equal(h.record().At.Add(30 * 60e9)) {
		t.Fatal("time limit not from policy")
	}
	h.result(contract.OutcomeFailed)
	h.escalation(ReasonRetriesExhausted)
}

func TestNFR5AC1_EveryDecisionNamesItsBasisAndResult(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	id := h.runID()
	h.reviewed([]Finding{open4})
	rec := h.record()
	if rec.Result == nil || rec.Result.RunID != id || rec.Result.Outcome != contract.OutcomeDone || rec.Result.Stage != contract.StageReview {
		t.Fatalf("result ref = %+v", rec.Result)
	}
	b := rec.Basis
	if b.Head != "h1" || b.RoundsUsed != 0 || b.RoundAllowance != 3 || len(b.UnclearedManual) != 1 || rec.Seq != h.s.Seq || rec.Rule == "" {
		t.Fatalf("record = %+v", rec)
	}
	if got := rec.Actions[len(rec.Actions)-1]; got != "run_stage" {
		t.Fatalf("actions = %v", rec.Actions)
	}
}

func TestDecideDoesNotChangeItsInput(t *testing.T) {
	h := inReview(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.reviewed([]Finding{open4, open2})
	in := h.s.Clone()
	snapshot := in.Clone()
	out, _ := Decide(in, StageResult{At: At{t0}, RunID: in.Run.RunID, Stage: contract.StageAddress, Outcome: contract.OutcomeDone,
		Address: &AddressOutcome{Head: "h2", ManualReplies: []ReplyOutcome{{To: "MC-1", Counts: true}}}}, Policy{})
	out.Findings[0].Status = contract.StatusWithdrawn
	out.ManualComments[0].Empty = true
	if !equalStates(in, snapshot) {
		t.Fatal("Decide changed its input")
	}
}

func TestPolicyDefaults(t *testing.T) {
	var p Policy
	if p.maxRounds() != 3 || p.resumeRounds() != 1 || p.blockingThreshold() != 4 || p.maxBaseUpdates() != 3 {
		t.Fatal("defaults")
	}
	if l := p.limits(contract.StageReview); l.TimeLimit != DefaultTimeLimit || l.Attempts != 2 {
		t.Fatalf("limits = %+v", l)
	}
}
