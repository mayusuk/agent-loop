package core

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// planHarness drives DecidePlan one event at a time.
type planHarness struct {
	t    *testing.T
	p    Policy
	s    PlanState
	acts []Action
	n    int
}

func newPlan(t *testing.T) *planHarness { t.Helper(); return &planHarness{t: t} }

func (h *planHarness) at() At { h.n++; return At{t0.Add(time.Duration(h.n) * time.Minute)} }

func (h *planHarness) do(e Event) *planHarness {
	h.t.Helper()
	before := h.s.Clone()
	in := h.s.Clone()
	h.s, h.acts = DecidePlan(in, e, h.p)
	if !reflect.DeepEqual(in, before) {
		h.t.Fatalf("%s: DecidePlan changed its input", e.Name())
	}
	rec, ok := h.acts[len(h.acts)-1].(RecordDecision)
	if !ok {
		h.t.Fatalf("%s: last action is %T, want RecordDecision", e.Name(), h.acts[len(h.acts)-1])
	}
	r := rec.Record
	if r.From != before.State || r.To != h.s.State || r.Event != e.Name() || r.Plan == nil || r.Seq != before.Seq+1 {
		h.t.Fatalf("%s: record %+v doesn't match %s -> %s", e.Name(), r, before.State, h.s.State)
	}
	keys := map[string]bool{}
	for _, a := range h.acts {
		k := planKey(a)
		if k == "" {
			continue
		}
		if keys[k] {
			h.t.Fatalf("%s: two writes share the key %s", e.Name(), k)
		}
		keys[k] = true
	}
	return h
}

func planKey(a Action) string {
	switch a := a.(type) {
	case Notify:
		return a.Key
	case Materialise:
		return a.Key
	case MarkProposal:
		return a.Key
	case RecordPlan:
		return a.Key
	}
	return ""
}

func (h *planHarness) record() DecisionRecord {
	return h.acts[len(h.acts)-1].(RecordDecision).Record
}

// kinds lists the actions' kinds without the decision record.
func (h *planHarness) kinds() []string {
	var k []string
	for _, a := range h.acts[:len(h.acts)-1] {
		k = append(k, a.Kind())
	}
	return k
}

func (h *planHarness) state(want State) *planHarness {
	h.t.Helper()
	if h.s.State != want {
		h.t.Fatalf("state = %s, want %s (rule %q)", h.s.State, want, h.record().Rule)
	}
	return h
}

func (h *planHarness) runID() string {
	h.t.Helper()
	if h.s.Run == nil {
		h.t.Fatalf("no run in state %s", h.s.State)
	}
	return h.s.Run.RunID
}

func (h *planHarness) mint() *planHarness {
	h.t.Helper()
	return h.do(MintRequested{At: h.at(), WorkflowID: "mint/p", MintID: "mint-1"}).state(StateMinting)
}

func (h *planHarness) mintResult(o contract.Outcome) *planHarness {
	h.t.Helper()
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: o, Reason: "x"})
}

func (h *planHarness) proposed() *planHarness {
	h.t.Helper()
	h.mint()
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeDone,
		Mint: &MintOutcome{Proposal: "90", PlanKey: "003-auth"}}).state(StatePlanProposed)
}

func (h *planHarness) materialising() *planHarness {
	h.t.Helper()
	return h.proposed().do(ChangeMerged{At: h.at(), By: RoleOwner, Change: "90"}).state(StateMaterialising)
}

func (h *planHarness) matResult(r MaterialiseResult) *planHarness {
	h.t.Helper()
	r.At, r.RunID = h.at(), h.runID()
	return h.do(r)
}

func (h *planHarness) matEscalated() *planHarness {
	h.t.Helper()
	return h.materialising().matResult(MaterialiseResult{Invalid: true}).state(StateMaterialiseEscalated)
}

func (h *planHarness) find(kind string) Action {
	h.t.Helper()
	for _, a := range h.acts {
		if a.Kind() == kind {
			return a
		}
	}
	h.t.Fatalf("no %s action in %v", kind, h.kinds())
	return nil
}

// TestPlanSection6_EveryPlanningEdge has one row per planning edge of the
// PRODUCT-SPEC §6 diagram.
func TestPlanSection6_EveryPlanningEdge(t *testing.T) {
	failed := func(h *planHarness) Event {
		return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeFailed}
	}
	matFailed := func(h *planHarness) Event {
		return MaterialiseResult{At: h.at(), RunID: h.runID(), Outcome: contract.OutcomeFailed}
	}
	retried := func(h *planHarness) *planHarness { h.mint(); return h.do(failed(h)) }
	matRetried := func(h *planHarness) *planHarness { h.materialising(); return h.do(matFailed(h)) }
	for _, tc := range []struct {
		edge string
		from func(*planHarness) *planHarness
		ev   func(*planHarness) Event
		to   State
		acts []string
	}{
		{"[*] --> minting", func(h *planHarness) *planHarness { return h },
			func(h *planHarness) Event { return MintRequested{At: h.at(), WorkflowID: "mint/p", MintID: "mint-1"} },
			StateMinting, []string{"run_stage"}},
		{"minting --> minting: failed, attempts left", (*planHarness).mint, failed, StateMinting, []string{"run_stage"}},
		{"minting --> plan_proposed", (*planHarness).mint, func(h *planHarness) Event {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeDone,
				Mint: &MintOutcome{Proposal: "90", PlanKey: "003-auth"}}
		}, StatePlanProposed, []string{"wait"}},
		{"minting --> complete", (*planHarness).mint, func(h *planHarness) Event {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeComplete}
		}, StateComplete, []string{"notify", "end_plan"}},
		{"minting --> mint_escalated: needs_owner", (*planHarness).mint, func(h *planHarness) Event {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeNeedsOwner}
		}, StateMintEscalated, []string{"notify", "wait"}},
		{"minting --> mint_escalated: retries used up", retried, failed, StateMintEscalated, []string{"notify", "wait"}},
		{"minting --> mint_escalated: time limit", (*planHarness).mint, func(h *planHarness) Event {
			return StageTimedOut{At: h.at(), RunID: h.runID()}
		}, StateMintEscalated, []string{"notify", "wait"}},
		{"plan_proposed --> materialising", (*planHarness).proposed, func(h *planHarness) Event {
			return ChangeMerged{At: h.at(), By: RoleOwner, Change: "90"}
		}, StateMaterialising, []string{"materialise"}},
		{"plan_proposed --> plan_rejected", (*planHarness).proposed, func(h *planHarness) Event {
			return ChangeClosed{At: h.at(), By: RoleOwner, Change: "90"}
		}, StatePlanRejected, []string{"record_plan", "end_plan"}},
		{"plan_proposed --> materialise_escalated: non-owner merge", (*planHarness).proposed, func(h *planHarness) Event {
			return ChangeMerged{At: h.at(), By: RoleOther, Change: "90"}
		}, StateMaterialiseEscalated, []string{"mark_proposal", "notify", "wait"}},
		{"materialising --> materialising: failed, attempts left", (*planHarness).materialising, matFailed,
			StateMaterialising, []string{"materialise"}},
		{"materialising --> materialised (items created)", (*planHarness).materialising, func(h *planHarness) Event {
			return MaterialiseResult{At: h.at(), RunID: h.runID(), Outcome: contract.OutcomeDone, Items: []string{"101", "102"}, Tracking: "100"}
		}, StateMaterialised, []string{"end_plan"}},
		{"materialising --> materialise_escalated: invalid task list", (*planHarness).materialising, func(h *planHarness) Event {
			return MaterialiseResult{At: h.at(), RunID: h.runID(), Outcome: contract.OutcomeFailed, Invalid: true}
		}, StateMaterialiseEscalated, []string{"mark_proposal", "notify", "wait"}},
		{"materialising --> materialise_escalated: plan not approved", (*planHarness).materialising, func(h *planHarness) Event {
			return MaterialiseResult{At: h.at(), RunID: h.runID(), Outcome: contract.OutcomeFailed, NotApproved: "95"}
		}, StateMaterialiseEscalated, []string{"mark_proposal", "notify", "wait"}},
		{"materialising --> materialise_escalated: retries used up", matRetried, matFailed,
			StateMaterialiseEscalated, []string{"mark_proposal", "notify", "wait"}},
		{"mint_escalated --> minting", func(h *planHarness) *planHarness {
			h.mint()
			return h.do(StageTimedOut{At: h.at(), RunID: h.runID()})
		}, func(h *planHarness) Event { return MintRequested{At: h.at(), WorkflowID: "mint/p", MintID: "mint-1"} },
			StateMinting, []string{"run_stage"}},
		{"materialise_escalated --> materialising: owner merges a fix", (*planHarness).matEscalated, func(h *planHarness) Event {
			return PlanFixMerged{At: h.at(), By: RoleOwner, Change: "95"}
		}, StateMaterialising, []string{"mark_proposal", "materialise"}},
		{"materialise_escalated --> plan_abandoned", (*planHarness).matEscalated, func(h *planHarness) Event {
			return PlanAbandoned{At: h.at()}
		}, StatePlanAbandoned, []string{"mark_proposal", "record_plan", "end_plan"}},
		{"complete --> minting", func(h *planHarness) *planHarness {
			return h.mint().mintResult(contract.OutcomeComplete)
		}, func(h *planHarness) Event { return MintRequested{At: h.at(), WorkflowID: "mint/p", MintID: "mint-1"} },
			StateMinting, []string{"run_stage"}},
	} {
		t.Run(tc.edge, func(t *testing.T) {
			h := tc.from(newPlan(t))
			h.do(tc.ev(h)).state(tc.to)
			if got := h.kinds(); !slices.Equal(got, tc.acts) {
				t.Fatalf("actions = %v, want %v (rule %q)", got, tc.acts, h.record().Rule)
			}
			if h.record().Rule == "ignored" {
				t.Fatal("the edge's decision has no rule")
			}
		})
	}
}

func TestFR9AC1_MintRunIDAndDeadline(t *testing.T) {
	h := newPlan(t).mint()
	r := h.find("run_stage").(RunStage)
	if r.Stage != contract.StageMint || r.Attempt != 1 || r.RunID != "mint/p#mint-1:mint:1:1" {
		t.Fatalf("run = %+v", r)
	}
	if !r.Deadline.Equal(h.record().At.Add(DefaultTimeLimit)) {
		t.Fatalf("deadline = %v", r.Deadline)
	}
}

func TestFR16AC1_PlanRetryUsesNextAttemptAndPolicyLimits(t *testing.T) {
	h := newPlan(t)
	h.p = Policy{Stages: map[contract.Stage]StageLimits{contract.StageMint: {Attempts: 3, TimeLimit: time.Hour}}}
	h.mint().mintResult(contract.OutcomeFailed).state(StateMinting)
	if r := h.find("run_stage").(RunStage); r.Attempt != 2 || !strings.Contains(r.RunID, ":mint:2:") ||
		!r.Deadline.Equal(h.record().At.Add(time.Hour)) {
		t.Fatalf("retry = %+v", r)
	}
	h.mintResult(contract.OutcomeFailed).state(StateMinting)
	h.mintResult(contract.OutcomeFailed).state(StateMintEscalated)
	if h.s.Escalation.Reason != ReasonRetriesExhausted || h.s.Escalation.Stage != contract.StageMint {
		t.Fatalf("escalation = %+v", h.s.Escalation)
	}
}

func TestFR16AC1_PlanCrashRerunsSameAttemptThenEscalates(t *testing.T) {
	for _, tc := range []struct {
		name string
		from func(*planHarness) *planHarness
		to   State
	}{
		{"mint", (*planHarness).mint, StateMintEscalated},
		{"materialise", (*planHarness).materialising, StateMaterialiseEscalated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.from(newPlan(t))
			st := h.s.State
			for i := 1; i <= MaxCrashReruns; i++ {
				old := h.runID()
				h.do(StageCrashed{At: h.at(), RunID: old}).state(st)
				if h.s.Run.Attempt != 1 || h.s.Run.Crashes != i || h.s.Run.RunID == old {
					t.Fatalf("rerun %d = %+v", i, h.s.Run)
				}
			}
			h.do(StageCrashed{At: h.at(), RunID: h.runID()}).state(tc.to)
			if h.s.Escalation.Reason != ReasonWorkerLost {
				t.Fatalf("escalation = %+v", h.s.Escalation)
			}
		})
	}
}

func TestFR18AC3_MintDoneWithoutProposalIsFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    func(*planHarness) StageResult
	}{
		{"no outcome", func(h *planHarness) StageResult {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeDone}
		}},
		{"no plan key", func(h *planHarness) StageResult {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeDone,
				Mint: &MintOutcome{Proposal: "90"}}
		}},
		{"not allowed for mint", func(h *planHarness) StageResult {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageMint, Outcome: contract.OutcomeBlocked}
		}},
		{"wrong stage", func(h *planHarness) StageResult {
			return StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageReview, Outcome: contract.OutcomeDone}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlan(t).mint()
			h.do(tc.e(h)).state(StateMinting)
			if h.s.Run.Attempt != 2 {
				t.Fatalf("run = %+v, want a retry", h.s.Run)
			}
		})
	}
}

func TestFR18AC1_PlanEventsForAnotherRunAreIgnored(t *testing.T) {
	h := newPlan(t).mint()
	before := h.s.Clone()
	for _, e := range []Event{
		StageResult{At: h.at(), RunID: "old", Stage: contract.StageMint, Outcome: contract.OutcomeDone},
		StageCrashed{At: h.at(), RunID: "old"},
		StageTimedOut{At: h.at(), RunID: "old"},
		DailyLimitReached{At: h.at(), RunID: "old"},
		MaterialiseResult{At: h.at(), RunID: h.runID(), Outcome: contract.OutcomeDone},
	} {
		h.do(e).state(StateMinting)
		if h.record().Rule != "ignored" || len(h.kinds()) != 0 || !reflect.DeepEqual(h.s.Run, before.Run) {
			t.Fatalf("%s: rule %q, actions %v", e.Name(), h.record().Rule, h.kinds())
		}
	}
}

func TestFR16AC3_MintRefusedByDailyLimitWaitsForTheDayBoundary(t *testing.T) {
	h := newPlan(t).mint()
	old := h.runID()
	h.do(DailyLimitReached{At: h.at(), RunID: old}).state(StateMinting)
	if !h.s.Waiting || !slices.Equal(h.kinds(), []string{"wait"}) {
		t.Fatalf("waiting = %v, actions %v", h.s.Waiting, h.kinds())
	}
	h.do(DayBoundary{At: h.at()}).state(StateMinting)
	r := h.find("run_stage").(RunStage)
	if h.s.Waiting || r.Attempt != 1 || r.RunID == old {
		t.Fatalf("restart = %+v, waiting %v", r, h.s.Waiting)
	}
	h.do(DayBoundary{At: h.at()})
	if len(h.kinds()) != 0 {
		t.Fatalf("a second day boundary started %v", h.kinds())
	}
}

func TestFR9AC5_CompleteThenMintAgainStartsANewMint(t *testing.T) {
	h := newPlan(t).mint().mintResult(contract.OutcomeComplete).state(StateComplete)
	if n := h.find("notify").(Notify); n.Event != EventComplete {
		t.Fatalf("notify = %+v", n)
	}
	h.mint()
	if r := h.find("run_stage").(RunStage); r.Attempt != 1 || r.RunID != "mint/p#mint-1:mint:1:2" {
		t.Fatalf("run = %+v", r)
	}
}

func TestFR9AC1_MintRequestedIgnoredWhileAPlanIsUnderway(t *testing.T) {
	for _, tc := range []struct {
		name string
		from func(*planHarness) *planHarness
	}{
		{"minting", (*planHarness).mint},
		{"plan_proposed", (*planHarness).proposed},
		{"materialising", (*planHarness).materialising},
		{"materialise_escalated", (*planHarness).matEscalated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.from(newPlan(t))
			st := h.s.State
			h.do(MintRequested{At: h.at(), WorkflowID: "mint/p", MintID: "mint-2"}).state(st)
			if len(h.kinds()) != 0 || h.s.MintID != "mint-1" {
				t.Fatalf("actions %v, mint id %s", h.kinds(), h.s.MintID)
			}
		})
	}
}

func TestFR12AC4_PlanNonOwnerMergeIsRecordedAndStartsNothing(t *testing.T) {
	for _, by := range []Role{RoleOther, RoleCoder, RoleReviewer, RoleLoop} {
		t.Run(string(by), func(t *testing.T) {
			h := newPlan(t).proposed()
			h.do(ChangeMerged{At: h.at(), By: by, Change: "90"}).state(StateMaterialiseEscalated)
			if slices.Contains(h.kinds(), "materialise") {
				t.Fatal("a non-owner merge started materialise")
			}
			if e := h.s.Escalation; e.Reason != ReasonMergedByNonOwner || e.Merge != "90" {
				t.Fatalf("escalation = %+v", e)
			}
		})
	}
}

func TestFR12AC4_PlanOwnerCloseRecordsRejected(t *testing.T) {
	for _, by := range []Role{RoleOwner, RoleOther} {
		t.Run(string(by), func(t *testing.T) {
			h := newPlan(t).proposed()
			h.do(ChangeClosed{At: h.at(), By: by, Change: "90"}).state(StatePlanRejected)
			if r := h.find("record_plan").(RecordPlan); r.Event != PlanRecordRejected || r.Proposal != "90" {
				t.Fatalf("record = %+v", r)
			}
			if slices.Contains(h.kinds(), "materialise") {
				t.Fatal("a rejected plan materialised")
			}
		})
	}
}

func TestFR12AC4_PlanOtherChangeEventsAreIgnored(t *testing.T) {
	h := newPlan(t).proposed()
	h.do(ChangeMerged{At: h.at(), By: RoleOwner, Change: "77"}).state(StatePlanProposed)
	h.do(ChangeClosed{At: h.at(), By: RoleOwner, Change: "77"}).state(StatePlanProposed)
	if len(h.kinds()) != 0 {
		t.Fatalf("actions %v", h.kinds())
	}
}

func TestFR12AC4_PlanNeedsOwnerRemovedFromProposalIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		from func(*planHarness) *planHarness
	}{
		{"plan_proposed", (*planHarness).proposed},
		{"materialise_escalated", (*planHarness).matEscalated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.from(newPlan(t))
			before := h.s.Clone()
			h.do(MarkerChanged{At: h.at(), By: RoleOwner, Marker: MarkerNeedsOwner, Added: false, OnChange: true})
			if len(h.kinds()) != 0 || !strings.Contains(h.record().Rule, "ignored") || h.record().Rule == "ignored" {
				t.Fatalf("actions %v, rule %q", h.kinds(), h.record().Rule)
			}
			before.Seq++
			if !reflect.DeepEqual(h.s, before) {
				t.Fatalf("state changed: %+v", h.s)
			}
		})
	}
}

func TestFR17AC8_OwnerFixRestartsMaterialiseWithANewAttemptCount(t *testing.T) {
	h := newPlan(t).materialising()
	h.matResult(MaterialiseResult{Outcome: contract.OutcomeFailed}).
		matResult(MaterialiseResult{Outcome: contract.OutcomeFailed}).state(StateMaterialiseEscalated)
	if h.s.Escalation.Reason != ReasonRetriesExhausted {
		t.Fatalf("escalation = %+v", h.s.Escalation)
	}
	h.do(PlanFixMerged{At: h.at(), By: RoleOwner, Change: "95"}).state(StateMaterialising)
	m := h.find("materialise").(Materialise)
	if m.Attempt != 1 || m.PlanKey != "003-auth" || !m.Deadline.Equal(h.record().At.Add(DefaultTimeLimit)) {
		t.Fatalf("materialise = %+v", m)
	}
	if mk := h.find("mark_proposal").(MarkProposal); mk.On || mk.Proposal != "90" || mk.Marker != MarkerNeedsOwner {
		t.Fatalf("marker = %+v", mk)
	}
	if h.s.Escalation != nil {
		t.Fatalf("escalation kept: %+v", h.s.Escalation)
	}
	h.matResult(MaterialiseResult{Outcome: contract.OutcomeFailed}).state(StateMaterialising)
}

func TestFR17AC8_NonOwnerFixEscalatesAgain(t *testing.T) {
	h := newPlan(t).matEscalated()
	h.do(PlanFixMerged{At: h.at(), By: RoleOther, Change: "96"}).state(StateMaterialiseEscalated)
	if slices.Contains(h.kinds(), "materialise") {
		t.Fatal("a non-owner fix started materialise")
	}
	if e := h.s.Escalation; e.Reason != ReasonMergedByNonOwner || e.Merge != "96" {
		t.Fatalf("escalation = %+v", e)
	}
	h.find("notify")
	h.find("mark_proposal")
}

func TestFR17AC8_LatestChangeNotOwnerMergedEscalatesNamingIt(t *testing.T) {
	h := newPlan(t).materialising().matResult(MaterialiseResult{Outcome: contract.OutcomeFailed, NotApproved: "96"})
	h.state(StateMaterialiseEscalated)
	if e := h.s.Escalation; e.Reason != ReasonPlanNotApproved || e.Merge != "96" {
		t.Fatalf("escalation = %+v", e)
	}
}

func TestFR17AC8_MergedChangeToAPlanNotEscalatedStartsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		from func(*planHarness) *planHarness
	}{
		{"none", func(h *planHarness) *planHarness { return h }},
		{"minting", (*planHarness).mint},
		{"plan_proposed", (*planHarness).proposed},
		{"materialising", (*planHarness).materialising},
		{"materialised", func(h *planHarness) *planHarness {
			return h.materialising().matResult(MaterialiseResult{Outcome: contract.OutcomeDone})
		}},
		{"plan_abandoned", func(h *planHarness) *planHarness { return h.matEscalated().do(PlanAbandoned{At: h.at()}) }},
		{"plan_rejected", func(h *planHarness) *planHarness {
			return h.proposed().do(ChangeClosed{At: h.at(), By: RoleOwner, Change: "90"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.from(newPlan(t))
			before := h.s.Clone()
			h.do(PlanFixMerged{At: h.at(), By: RoleOwner, Change: "97"}).state(before.State)
			if len(h.kinds()) != 0 || !strings.Contains(h.record().Rule, "D33") {
				t.Fatalf("actions %v, rule %q", h.kinds(), h.record().Rule)
			}
		})
	}
}

func TestFR17AC6_InvalidTaskListEscalatesWithoutRetry(t *testing.T) {
	h := newPlan(t).materialising().matResult(MaterialiseResult{Outcome: contract.OutcomeFailed, Invalid: true})
	h.state(StateMaterialiseEscalated)
	if h.s.Escalation.Reason != ReasonInvalidTaskList {
		t.Fatalf("escalation = %+v", h.s.Escalation)
	}
}

func TestFR17AC2_MaterialiseDoneRecordsCreatedItems(t *testing.T) {
	h := newPlan(t).materialising().matResult(MaterialiseResult{Outcome: contract.OutcomeDone, Items: []string{"101"}, Tracking: "100"})
	if !slices.Equal(h.s.Items, []string{"101"}) || h.s.Tracking != "100" || h.s.Run != nil {
		t.Fatalf("state = %+v", h.s)
	}
}

func TestFR17AC9_AbandonOnlyFromMaterialiseEscalated(t *testing.T) {
	h := newPlan(t).matEscalated().do(PlanAbandoned{At: h0(t)})
	h.state(StatePlanAbandoned)
	if r := h.find("record_plan").(RecordPlan); r.Event != PlanRecordAbandoned || r.Proposal != "90" {
		t.Fatalf("record = %+v", r)
	}
	if mk := h.find("mark_proposal").(MarkProposal); mk.On {
		t.Fatalf("marker = %+v", mk)
	}
	for _, from := range []func(*planHarness) *planHarness{(*planHarness).mint, (*planHarness).proposed, (*planHarness).materialising} {
		h := from(newPlan(t))
		st := h.s.State
		h.do(PlanAbandoned{At: h.at()}).state(st)
		if len(h.kinds()) != 0 {
			t.Fatalf("%s: abandon acted: %v", st, h.kinds())
		}
	}
}

func h0(t *testing.T) At { t.Helper(); return At{t0.Add(time.Hour)} }

func TestFR10AC1_PlanMintEscalationWithoutProposalOnlyNotifies(t *testing.T) {
	h := newPlan(t).mint()
	h.mintResult(contract.OutcomeNeedsOwner).state(StateMintEscalated)
	if slices.Contains(h.kinds(), "mark_proposal") {
		t.Fatal("marked a proposal that doesn't exist")
	}
	if n := h.find("notify").(Notify); n.Event != contract.EventEscalate || n.Reason != ReasonNeedsOwner {
		t.Fatalf("notify = %+v", n)
	}
}

func TestFR10AC1_PlanEscalationMarksTheProposalAndNotifies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reach  func(*planHarness) *planHarness
		reason string
	}{
		{"non-owner merge", func(h *planHarness) *planHarness {
			return h.proposed().do(ChangeMerged{At: h.at(), By: RoleOther, Change: "90"})
		}, ReasonMergedByNonOwner},
		{"invalid task list", (*planHarness).matEscalated, ReasonInvalidTaskList},
		{"time limit", func(h *planHarness) *planHarness {
			h.materialising()
			return h.do(StageTimedOut{At: h.at(), RunID: h.runID()})
		}, ReasonTimeLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.reach(newPlan(t)).state(StateMaterialiseEscalated)
			mk := h.find("mark_proposal").(MarkProposal)
			if !mk.On || mk.Proposal != "90" || mk.Marker != MarkerNeedsOwner {
				t.Fatalf("marker = %+v", mk)
			}
			if n := h.find("notify").(Notify); n.Event != contract.EventEscalate || n.Reason != tc.reason {
				t.Fatalf("notify = %+v", n)
			}
		})
	}
}

func TestNFR5AC1_PlanDecisionRecordNamesTheBasis(t *testing.T) {
	h := newPlan(t).materialising()
	h.matResult(MaterialiseResult{Outcome: contract.OutcomeFailed})
	r := h.record()
	if r.Plan.Proposal != "90" || r.Plan.PlanKey != "003-auth" || r.Plan.Attempt != 1 ||
		r.Result == nil || r.Result.Stage != StageMaterialise || r.Result.Outcome != contract.OutcomeFailed {
		t.Fatalf("record = %+v, plan %+v", r, r.Plan)
	}
	if !slices.Equal(r.Actions, []string{"materialise"}) {
		t.Fatalf("record actions = %v", r.Actions)
	}
}

func TestNFR5AC1_DecidePlanIsDeterministic(t *testing.T) {
	h := newPlan(t).matEscalated()
	e := PlanFixMerged{At: h.at(), By: RoleOwner, Change: "95"}
	s1, a1 := DecidePlan(h.s, e, h.p)
	s2, a2 := DecidePlan(h.s, e, h.p)
	if !reflect.DeepEqual(s1, s2) || !reflect.DeepEqual(a1, a2) {
		t.Fatal("two calls with the same input differ")
	}
}
