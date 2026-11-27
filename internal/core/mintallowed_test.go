package core

import (
	"testing"

	"github.com/mayusuk/agent-loop/contract"
)

func plans(states ...State) []PlanState {
	var ps []PlanState
	for _, st := range states {
		ps = append(ps, PlanState{State: st})
	}
	return ps
}

func TestFR9AC1_MintAllowedRefusesWithADistinctReasonPerCondition(t *testing.T) {
	seen := map[Reason]string{}
	for _, tc := range []struct {
		name string
		pp   ProjectPlanning
		want Reason
	}{
		{"project paused", ProjectPlanning{Paused: true}, ReasonProjectPaused},
		{"minter running", ProjectPlanning{MinterRunning: true}, ReasonMinterRunning},
		{"plan minting", ProjectPlanning{Plans: plans(StateMinting)}, ReasonMinterRunning},
		{"open proposal", ProjectPlanning{Plans: plans(StatePlanProposed)}, ReasonOpenProposal},
		{"materialising", ProjectPlanning{Plans: plans(StateMaterialising)}, ReasonMaterialiseUnfinished},
		{"materialise_escalated", ProjectPlanning{Plans: plans(StateMaterialiseEscalated)}, ReasonMaterialiseUnfinished},
		{"daily limit", ProjectPlanning{DailyLimitReached: true}, ReasonDailyLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, r := MintAllowed(tc.pp)
			if ok || r != tc.want {
				t.Fatalf("MintAllowed = %v, %q, want false, %q", ok, r, tc.want)
			}
		})
		seen[tc.want] = tc.name
	}
	if len(seen) != 5 {
		t.Fatalf("reasons = %v, want 5 distinct", seen)
	}
}

func TestFR9AC1_MintAllowedReportsTheFirstReasonInOrder(t *testing.T) {
	pp := ProjectPlanning{Paused: true, MinterRunning: true, DailyLimitReached: true,
		Plans: plans(StatePlanProposed, StateMaterialiseEscalated)}
	for _, want := range []Reason{ReasonProjectPaused, ReasonMinterRunning, ReasonOpenProposal, ReasonMaterialiseUnfinished, ReasonDailyLimit} {
		if ok, r := MintAllowed(pp); ok || r != want {
			t.Fatalf("MintAllowed = %v, %q, want %q", ok, r, want)
		}
		switch want {
		case ReasonProjectPaused:
			pp.Paused = false
		case ReasonMinterRunning:
			pp.MinterRunning = false
		case ReasonOpenProposal:
			pp.Plans = plans(StateMaterialiseEscalated)
		case ReasonMaterialiseUnfinished:
			pp.Plans = nil
		}
	}
}

// Items play no part in MintAllowed, so items that are blocked, escalated
// or in review never refuse a mint; finished plans don't either.
func TestFR9AC1_MintAllowedWhileItemsAreBusyAndPlansFinished(t *testing.T) {
	ok, r := MintAllowed(ProjectPlanning{Plans: plans(StateMintEscalated, StateComplete, StatePlanRejected,
		StatePlanAbandoned, StateMaterialised, StateNone)})
	if !ok || r != "" {
		t.Fatalf("MintAllowed = %v, %q", ok, r)
	}
}

func TestFR9AC5_MintAllowedAfterComplete(t *testing.T) {
	h := newPlan(t).mint().mintResult(contract.OutcomeComplete).state(StateComplete)
	if ok, r := MintAllowed(ProjectPlanning{Plans: []PlanState{h.s}}); !ok {
		t.Fatalf("MintAllowed after complete = %q", r)
	}
	h.mint()
	if ok, r := MintAllowed(ProjectPlanning{Plans: []PlanState{h.s}}); ok || r != ReasonMinterRunning {
		t.Fatalf("MintAllowed while minting = %v, %q", ok, r)
	}
}

func TestFR17AC9_MintAllowedAgainAfterAbandon(t *testing.T) {
	h := newPlan(t).matEscalated()
	if ok, _ := MintAllowed(ProjectPlanning{Plans: []PlanState{h.s}}); ok {
		t.Fatal("mint allowed while the plan is in materialise_escalated")
	}
	h.do(PlanAbandoned{At: h.at()}).state(StatePlanAbandoned)
	if ok, r := MintAllowed(ProjectPlanning{Plans: []PlanState{h.s}}); !ok {
		t.Fatalf("MintAllowed after abandon = %q", r)
	}
}
