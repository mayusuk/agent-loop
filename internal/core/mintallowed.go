package core

// Reason is why MintAllowed refuses a mint (FR-9 AC1). It is empty when the
// mint is allowed.
type Reason string

// The refusal reasons, in the order MintAllowed checks them (work plan
// #155 D7).
const (
	ReasonProjectPaused         Reason = "project_paused"
	ReasonMinterRunning         Reason = "minter_running"
	ReasonOpenProposal          Reason = "open_proposal"
	ReasonMaterialiseUnfinished Reason = "materialise_unfinished"
	ReasonDailyLimit            Reason = "daily_limit"
)

// ProjectPlanning is what MintAllowed needs to know about one project
// (FR-9 AC1). Items play no part: minting may run while items are in any
// state, including blocked, escalated or in review.
type ProjectPlanning struct {
	// Plans are the project's plans in any state.
	Plans []PlanState
	// MinterRunning: a mint workflow is running for the project
	// (FR-9 AC6), whether or not its plan is listed.
	MinterRunning bool
	// Paused: the project or the deployment is paused (FR-10 AC3).
	Paused bool
	// DailyLimitReached: the project's daily agent-run limit is reached
	// (FR-16 AC3, DailyCount.Reached).
	DailyLimitReached bool
}

// MintAllowed reports whether the owner's `agent-loop mint` may start, or
// the first reason it may not (FR-9 AC1, work plan #155 D7). A plan in
// mint_escalated, complete, plan_rejected, plan_abandoned or materialised
// never refuses (FR-9 AC5, FR-17 AC9).
func MintAllowed(pp ProjectPlanning) (bool, Reason) {
	has := func(states ...State) bool {
		for _, p := range pp.Plans {
			for _, st := range states {
				if p.State == st {
					return true
				}
			}
		}
		return false
	}
	switch {
	case pp.Paused:
		return false, ReasonProjectPaused
	case pp.MinterRunning || has(StateMinting):
		return false, ReasonMinterRunning
	case has(StatePlanProposed):
		return false, ReasonOpenProposal
	case has(StateMaterialising, StateMaterialiseEscalated):
		return false, ReasonMaterialiseUnfinished
	case pp.DailyLimitReached:
		return false, ReasonDailyLimit
	}
	return true, ""
}
