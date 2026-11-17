package core

import (
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// The daily agent-run limit (FR-16 AC3, AC4) and the pause gate (FR-10 AC3).

func TestFR16AC3_AtTheLimitNoAgentStageStarts(t *testing.T) {
	loc := time.UTC
	var c DailyCount
	var a Admission
	var notify bool
	for i := range 3 {
		c, a, _ = Admit(c, AdmitRequest{Now: t0, Agent: true, Limit: 3, Loc: loc})
		if a != AdmitGranted {
			t.Fatalf("run %d: %s", i+1, a)
		}
	}
	c, a, notify = Admit(c, AdmitRequest{Now: t0, Agent: true, Limit: 3, Loc: loc})
	if a != AdmitWaitingLimit || !notify {
		t.Fatalf("at the limit: %s notify %v", a, notify)
	}
	c, a, notify = Admit(c, AdmitRequest{Now: t0, Agent: true, Limit: 3, Loc: loc})
	if a != AdmitWaitingLimit || notify {
		t.Fatalf("second refusal: %s notify %v (owner notified once)", a, notify)
	}
	if _, a, _ = Admit(c, AdmitRequest{Now: t0, Agent: false, Limit: 3, Loc: loc}); a != AdmitGranted {
		t.Fatalf("merge needs no unit: %s", a)
	}
	if !c.Reached(t0, loc, 3) {
		t.Fatal("Reached false at the limit")
	}
	if r := Pick("p", []Candidate{cand("1", "bug", 0)}, ProjectView{DailyLimitReached: c.Reached(t0, loc, 3)}, PickPolicy{}); len(r.Picks) != 0 {
		t.Fatal("picked at the daily limit")
	}
}

func TestFR16AC3_LimitCannotBeTurnedOff(t *testing.T) {
	c := DailyCount{Day: "2026-11-16", Runs: DefaultDailyAgentRuns}
	if _, a, _ := Admit(c, AdmitRequest{Now: t0, Agent: true, Limit: 0, Loc: time.UTC}); a != AdmitWaitingLimit {
		t.Fatalf("limit 0 = %s, want the default %d", a, DefaultDailyAgentRuns)
	}
}

func TestFR16AC3_ItemsWaitAtTheLimitIncludingAfterAWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    func(*testing.T) *harness
	}{
		{"next stage", func(t *testing.T) *harness { return inReview(t, Policy{}) }},
		{"leaving blocked", func(t *testing.T) *harness {
			h := claimedStates()[4].build(t)
			return h.do(DependencyClosed{At: h.at(), Dependency: "61", Done: true})
		}},
		{"leaving escalated", func(t *testing.T) *harness {
			return inReview(t, Policy{}).result(contract.OutcomeNeedsOwner).cmd(CommandResume)
		}},
		{"leaving paused", func(t *testing.T) *harness {
			h := inReview(t, Policy{}).cmd(CommandPause).reviewed([]Finding{open4})
			return h.cmd(CommandResume)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h(t)
			st, stage := h.s.State, h.s.Run.Stage
			h.do(DailyLimitReached{At: h.at(), RunID: h.runID()}).state(StateWaitingLimit)
			h.noRunStage()
			// FR-16 AC4: at the day boundary the item continues.
			h.do(DayBoundary{At: h.at()}).state(st)
			if h.runStage().Stage != stage {
				t.Fatalf("continued with %s, want %s", h.runStage().Stage, stage)
			}
		})
	}
}

func TestFR16AC4_DayBoundaryInTheProfileTimeZone(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	late := time.Date(2026, 11, 16, 18, 30, 0, 0, time.UTC) // 23:30 local
	if b := NextDayBoundary(late, loc); !b.Equal(time.Date(2026, 11, 16, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("boundary = %v", b)
	}
	c := DailyCount{}
	c, _, _ = Admit(c, AdmitRequest{Now: late, Agent: true, Limit: 1, Loc: loc})
	c, a, _ := Admit(c, AdmitRequest{Now: late, Agent: true, Limit: 1, Loc: loc})
	if a != AdmitWaitingLimit {
		t.Fatalf("admission = %s", a)
	}
	if _, wake, _ := DayReset(c, late.Add(10*time.Minute), loc); wake {
		t.Fatal("reset before local midnight")
	}
	next, wake, notify := DayReset(c, late.Add(30*time.Minute), loc)
	if !wake || !notify || next.Runs != 0 || next.Day != "2026-11-17" {
		t.Fatalf("reset = %+v wake %v notify %v", next, wake, notify)
	}
	if _, a, _ := Admit(next, AdmitRequest{Now: late.Add(31 * time.Minute), Agent: true, Limit: 1, Loc: loc}); a != AdmitGranted {
		t.Fatalf("after the reset: %s", a)
	}
	if _, wake, notify := DayReset(DailyCount{Day: "2026-11-16"}, late.Add(30*time.Minute), loc); !wake || notify {
		t.Fatal("quiet day: wake without notifying")
	}
}

func TestFR16AC4_LimitNeverStopsARunningStage(t *testing.T) {
	h := inReview(t, Policy{})
	// The project reaching its limit sends nothing to a running item; its
	// result is recorded and the next stage asks again.
	h.reviewed([]Finding{open4}).addressMode(contract.ModeFindings)
}

func TestFR10AC3_ProjectOrDeploymentPauseHoldsEveryStart(t *testing.T) {
	c := DailyCount{}
	for _, agent := range []bool{true, false} {
		next, a, notify := Admit(c, AdmitRequest{Now: t0, Agent: agent, Paused: true, Limit: 1, Loc: time.UTC})
		if a != AdmitPaused || notify || next != c {
			t.Fatalf("agent %v: %s %+v", agent, a, next)
		}
	}
	if r := Pick("p", []Candidate{cand("1", "bug", 0)}, ProjectView{Paused: true}, PickPolicy{}); len(r.Picks) != 0 {
		t.Fatal("picked in a paused project")
	}
}
