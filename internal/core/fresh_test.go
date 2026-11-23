package core

import (
	"testing"

	"github.com/mayusuk/agent-loop/contract"
)

// Work plan #25 D3: the merge check and a review's result carry the manual
// comments as read at that moment, so a comment the feed hasn't reported
// yet still counts.

// TestFR7AC9_MergeCheckSeesACommentTheFeedHasNotReported: a comment in the
// merge check's facts holds the merge and starts a manual address run.
func TestFR7AC9_MergeCheckSeesACommentTheFeedHasNotReported(t *testing.T) {
	h := merging(t, Policy{})
	c := clean()
	c.Manual = []ManualCommentChanged{{Comment: "MC-1"}}
	h.check(c).addressMode(contract.ModeManual)
	if got := h.runStage().Address.AnswerManual; len(got) != 1 || got[0] != "MC-1" {
		t.Fatalf("answer %v, want MC-1", got)
	}
}

// TestFR7AC9_MergeCheckSeesAClearingTheFeedHasNotReported: a comment the
// state holds as open, cleared by a human by the time of the check, no
// longer holds the merge; one a human had cleared and no longer has
// blocks again.
func TestFR7AC9_MergeCheckSeesAClearingTheFeedHasNotReported(t *testing.T) {
	h := merging(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	c := clean()
	c.Manual = []ManualCommentChanged{{Comment: "MC-1", HumanCleared: true}}
	h.check(c)
	h.mergeAction()

	h = merging(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1", HumanCleared: true})
	c = clean()
	c.Manual = []ManualCommentChanged{{Comment: "MC-1"}}
	h.check(c).addressMode(contract.ModeManual)
}

// TestFR6AC3_RoundLimitEscalationNamesACommentLeftDuringTheReview: §11.1
// step 3. A comment left while the last allowed round ran, reported with
// the review's result, is named by the escalation.
func TestFR6AC3_RoundLimitEscalationNamesACommentLeftDuringTheReview(t *testing.T) {
	h := inReview(t, Policy{MaxRounds: 1})
	rv := h.s.Run.Review
	h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageReview, Outcome: contract.OutcomeDone,
		Review: &ReviewOutcome{Commit: rv.Commit, Kind: rv.Kind, Findings: []Finding{open5}},
		Manual: []ManualCommentChanged{{Comment: "MC-7"}}})
	ps := h.escalation(ReasonRoundLimit)
	if len(ps.CommentIDs) != 1 || ps.CommentIDs[0] != "MC-7" {
		t.Fatalf("escalation names %v, want MC-7", ps.CommentIDs)
	}
}

// TestNFR2AC1_UncountedReviewsOfOneCommitAreToldApart: work plan #25 D4.
// Two manual-reply reviews of one commit, after two address runs that
// left the head, get different sequence numbers; a rerun of one keeps its
// number.
func TestNFR2AC1_UncountedReviewsOfOneCommitAreToldApart(t *testing.T) {
	h := merging(t, Policy{})
	h.do(ManualCommentChanged{At: h.at(), Comment: "MC-1"})
	h.check(clean()).addressMode(contract.ModeManual)
	h.addressed("h1").reviewKind(contract.KindManualReply)
	first := h.runStage().Review
	// The worker is lost: the rerun asks for the same review.
	h.do(StageCrashed{At: h.at(), RunID: h.runID()})
	if again := h.runStage().Review; again.Seq != first.Seq || again.Commit != first.Commit {
		t.Fatalf("rerun review %+v, want the same as %+v", again, first)
	}
	h.reviewed(nil, ManualRuling{Comment: "MC-1", Ruling: contract.RulingOpen}).state(StateMerging)
	h.check(clean()).addressMode(contract.ModeManual)
	h.addressed("h1").reviewKind(contract.KindManualReply)
	second := h.runStage().Review
	if second.Commit != first.Commit || second.Seq == first.Seq || second.Round != 0 {
		t.Fatalf("second manual-reply review %+v, first %+v: want the same commit and another sequence number", second, first)
	}
}
