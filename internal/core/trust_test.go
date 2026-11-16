package core

import (
	"slices"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
)

// The pre-stage trust check and owner approvals (FR-13 AC5, AC6,
// TECHNICAL-SPEC §8.3, INTEGRATION-SPEC §9.8).

func untrusted(commits ...string) TrustReport {
	return TrustReport{Causes: []string{CauseRulesetChanged}, Commits: commits}
}

func (h *harness) refuse(r TrustReport) *harness {
	h.t.Helper()
	return h.do(TrustRefused{At: h.at(), RunID: h.runID(), Report: r})
}

func (h *harness) approvals() []ApproveCommits {
	var out []ApproveCommits
	for _, a := range h.acts {
		if x, ok := a.(ApproveCommits); ok {
			out = append(out, x)
		}
	}
	return out
}

func TestFR13AC5_TrustVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		r        TrustReport
		approved []string
		want     TrustVerdict
	}{
		{"no causes", TrustReport{Commits: []string{"c1"}}, nil, TrustVerdict{}},
		{"ruleset changed", untrusted("c1", "c2"), []string{"c1"},
			TrustVerdict{Untrusted: true, Commits: []string{"c2"}, Reason: ReasonUntrustedCommit}},
		{"every commit approved", untrusted("c1"), []string{"c1"}, TrustVerdict{}},
		{"fork, even approved", TrustReport{Causes: []string{CausePusherUnknown, CauseFork}, Commits: []string{"c1"}}, []string{"c1"},
			TrustVerdict{Untrusted: true, Fork: true, Reason: ReasonForkChange}},
		{"outside prefix", TrustReport{Causes: []string{CausePusherUnknown, CauseBranchOutsidePrefix}, Commits: []string{"c1"}}, nil,
			TrustVerdict{Untrusted: true, Commits: []string{"c1"}, Reason: ReasonBranchOutsidePrefix}},
		{"admin and unknown pusher", TrustReport{Causes: []string{CausePusherUnknown, CauseAdminNotOwner}, Commits: []string{"c1", "c1"}}, nil,
			TrustVerdict{Untrusted: true, Commits: []string{"c1"}, Reason: ReasonUntrustedCommit}},
		{"unknown pusher only", TrustReport{Causes: []string{CausePusherUnknown}, Commits: []string{"c1"}}, nil,
			TrustVerdict{Untrusted: true, Commits: []string{"c1"}, Reason: ReasonPusherUnknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.r.Check(tc.approved)
			if got.Untrusted != tc.want.Untrusted || got.Fork != tc.want.Fork || got.Reason != tc.want.Reason || !slices.Equal(got.Commits, tc.want.Commits) {
				t.Fatalf("Check = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFR13AC5_TrustIsCheckedBeforeAddressNotReview(t *testing.T) {
	h := inReview(t, Policy{})
	if h.runStage().Trust != nil {
		t.Fatal("review asks for no trust check")
	}
	h.refuse(untrusted("h1")).state(StateInReview) // not a stage that is checked
	if h.record().Rule != "ignored" {
		t.Fatalf("rule = %q", h.record().Rule)
	}
	h.reviewed([]Finding{open4}).addressMode(contract.ModeFindings)
	if h.runStage().Trust == nil {
		t.Fatal("address asks for the trust check")
	}
	h.refuse(untrusted("h1"))
	ps := h.escalation(ReasonUntrustedCommit)
	if ps.Stage != contract.StageAddress || !slices.Equal(ps.Commits, []string{"h1"}) {
		t.Fatalf("status = %+v", ps)
	}
}

func TestFR13AC5_TrustIsCheckedBeforeImplementOnAChange(t *testing.T) {
	h := newHarness(t, Policy{}).start(nil)
	if h.runStage().Trust != nil {
		t.Fatal("implement with no change has nothing to check")
	}
	h = newHarness(t, Policy{}).start(&Change{Ref: "57", Head: "h1"})
	h.call(func(d *decision) { d.start(plan{stage: contract.StageImplement}, 1, 0) }).state(StateImplementing)
	if h.runStage().Trust == nil {
		t.Fatal("implement on a change asks for the trust check")
	}
	h.refuse(untrusted("h1"))
	if ps := h.escalation(ReasonUntrustedCommit); ps.Stage != contract.StageImplement {
		t.Fatalf("status = %+v", ps)
	}
}

func TestFR13AC5_RefusalForAnotherRunIsIgnored(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	h.do(TrustRefused{At: h.at(), RunID: "other", Report: untrusted("h1")}).state(StateAddressing)
	if h.record().Rule != "ignored" {
		t.Fatalf("rule = %q", h.record().Rule)
	}
}

func TestFR13AC5_UntrustedPushDuringReviewEscalatesWhenItEnds(t *testing.T) {
	for _, o := range []contract.Outcome{contract.OutcomeDone, contract.OutcomeFailed} {
		t.Run(string(o), func(t *testing.T) {
			h := inReview(t, Policy{})
			h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherUnknown}).state(StateInReview) // the review runs on
			if o == contract.OutcomeDone {
				h.reviewed(nil)
			} else {
				h.result(o)
			}
			ps := h.escalation(ReasonPusherUnknown)
			if ps.Stage != contract.StageReview || !slices.Equal(ps.Commits, []string{"h2"}) || len(h.s.UnknownPushes) != 0 {
				t.Fatalf("status = %+v, unknown = %v", ps, h.s.UnknownPushes)
			}
			if h.s.RoundsUsed != 0 {
				t.Fatal("the review's result must not be applied")
			}
			// The owner's resume approves the push, and the new head is reviewed.
			h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound)
			if a := h.approvals(); len(a) != 1 || !slices.Equal(a[0].Commits, []string{"h2"}) {
				t.Fatalf("approvals = %+v", a)
			}
		})
	}
}

func TestFR13AC5_UnknownPushOutsideReviewIsLeftToTheTrustCheck(t *testing.T) {
	h := merging(t, Policy{})
	h.do(HeadMoved{At: h.at(), Commit: "h2", Pusher: PusherUnknown})
	if len(h.s.UnknownPushes) != 0 {
		t.Fatal("only a running review collects unknown pushes")
	}
}

func TestFR13AC6_ResumeApprovesExactlyTheNamedCommits(t *testing.T) {
	h := merging(t, Policy{})
	h.check(MergeChecked{Head: "h2", Pusher: PusherUnknown, ContainsBase: true,
		Trust: TrustReport{Causes: []string{CausePusherUnknown}, Commits: []string{"h2"}}})
	h.escalation(ReasonPusherUnknown)

	h.do(Resumed{At: h.at()}).reviewKind(contract.KindRound) // §8.2 rule 4: h2 not reviewed
	a := h.approvals()
	if len(a) != 1 || !slices.Equal(a[0].Commits, []string{"h2"}) || !slices.Equal(h.s.ApprovedCommits, []string{"h2"}) {
		t.Fatalf("approvals = %+v, approved = %v", a, h.s.ApprovedCommits)
	}
	if !hasAction[PostStatus](h.acts) {
		t.Fatal("resume status missing")
	}

	// The approved commit is trusted from then on.
	h.reviewed(nil).state(StateMerging)
	h.check(MergeChecked{Head: "h2", ContainsBase: true,
		Trust: TrustReport{Causes: []string{CausePusherUnknown}, Commits: []string{"h2"}}}).mergeAction()

	// A later untrusted commit escalates again, naming only it.
	h.mergeResult(MergeHeadMoved)
	h.check(MergeChecked{Head: "h3", Pusher: PusherUnknown, ContainsBase: true,
		Trust: TrustReport{Causes: []string{CausePusherUnknown}, Commits: []string{"h2", "h3"}}})
	if ps := h.escalation(ReasonPusherUnknown); !slices.Equal(ps.Commits, []string{"h3"}) {
		t.Fatalf("status = %+v", ps)
	}
	h.do(Resumed{At: h.at()})
	if !slices.Equal(h.s.ApprovedCommits, []string{"h2", "h3"}) {
		t.Fatalf("approved = %v", h.s.ApprovedCommits)
	}
}

func TestFR13AC6_ApprovedCommitsLetARefusedRunStart(t *testing.T) {
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	h.s.ApprovedCommits = []string{"h1"}
	id := h.runID()
	h.refuse(untrusted("h1")).addressMode(contract.ModeFindings)
	if r := h.runStage(); r.RunID != id || !slices.Equal(r.Trust.Approved, []string{"h1"}) {
		t.Fatalf("run = %+v", r)
	}
}

func TestFR13AC6_ForkIsNeverApproved(t *testing.T) {
	fork := TrustReport{Causes: []string{CauseFork}, Commits: []string{"h1"}}
	h := inReview(t, Policy{}).reviewed([]Finding{open4})
	h.refuse(fork)
	if ps := h.escalation(ReasonForkChange); ps.Stage != contract.StageAddress || !h.s.Escalation.Fork {
		t.Fatalf("status = %+v", ps)
	}
	h.do(Resumed{At: h.at()}).addressMode(contract.ModeFindings)
	if len(h.approvals()) != 0 || len(h.s.ApprovedCommits) != 0 {
		t.Fatalf("a fork was approved: %v", h.s.ApprovedCommits)
	}
	if got := h.runStage().Trust.Approved; len(got) != 0 {
		t.Fatalf("approved = %v", got)
	}
	h.refuse(fork)
	h.escalation(ReasonForkChange) // escalated again on every check
}

func TestFR13AC6_OtherResumesApproveNothing(t *testing.T) {
	h := escalate(inReview(t, Policy{}))
	h.do(Resumed{At: h.at()})
	if len(h.approvals()) != 0 {
		t.Fatal("approved commits on a needs_owner resume")
	}
}

func TestFR13AC6_StartedRebuildsApprovedCommits(t *testing.T) {
	h := newHarness(t, Policy{})
	h.do(Started{At: h.at(), WorkflowID: "item/p/42", Item: "42", Claim: 2,
		Change:          &Change{Ref: "57", Head: "h1"},
		Reviews:         []ReviewRecord{{Commit: "h1", Kind: contract.KindRound}},
		ApprovedCommits: []string{"h0"}}).state(StateMerging)
	h.check(MergeChecked{Head: "h1", ContainsBase: true, Trust: untrusted("h0")}).mergeAction()
}
