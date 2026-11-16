package profile

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// Policy is the core's view of a profile: the limits and names it decides
// with (TECHNICAL-SPEC §4). It holds only standard-library types, because
// internal/core can't import this package (D1).
type Policy struct {
	Types    []string
	Priority []string

	WIP               int
	MaxRounds         int
	BlockingThreshold int
	ResumeRounds      int
	MaxBaseUpdates    int
	MergeMethod       string

	FollowUpType  string
	FollowUpReady bool

	DailyAgentRuns int
	// DailySpend is 0 when there is no spend limit.
	DailySpend float64
	AgentRuns  int

	Location           *time.Location
	SupervisorInterval time.Duration
	PollInterval       time.Duration

	Stages map[string]StageLimits

	Markers map[string]MarkerMapping

	BaseBranch   string
	BranchPrefix string
	PlansDir     string

	// OwnerIDs are the owner accounts' stable IDs (FR-13 AC7, I13).
	OwnerIDs          []string
	ManualCommentBots []string

	// Identities holds the three identities' account IDs. It is empty until
	// WithIdentityIDs sets it.
	Identities IdentityIDs
}

// StageLimits are a stage's time limit and attempt limit (FR-16 AC1).
type StageLimits struct {
	TimeLimit time.Duration
	Attempts  int
}

// IdentityIDs are the stable account IDs of the three identities.
type IdentityIDs struct {
	Coder    string
	Reviewer string
	Loop     string
}

// Policy returns the core's view of the profile. The result shares no
// memory with the profile.
func (p *Profile) Policy() Policy {
	st := make(map[string]StageLimits, len(p.Stages))
	for n, s := range p.Stages {
		st[n] = StageLimits{TimeLimit: s.TimeLimit, Attempts: s.Attempts}
	}
	owners := make([]string, len(p.Identities.Owners))
	for i, o := range p.Identities.Owners {
		owners[i] = o.ID
	}
	return Policy{
		Types:              slices.Clone(p.Types),
		Priority:           slices.Clone(p.Priority),
		WIP:                p.WIP,
		MaxRounds:          p.MaxRounds,
		BlockingThreshold:  p.BlockingThreshold,
		ResumeRounds:       p.ResumeRounds,
		MaxBaseUpdates:     p.MaxBaseUpdates,
		MergeMethod:        p.MergeMethod,
		FollowUpType:       p.FollowUp.Type,
		FollowUpReady:      p.FollowUp.Ready,
		DailyAgentRuns:     p.Limits.DailyAgentRuns,
		DailySpend:         p.Limits.DailySpend,
		AgentRuns:          p.Limits.AgentRuns,
		Location:           p.Location,
		SupervisorInterval: p.Intervals.Supervisor,
		PollInterval:       p.Intervals.Poll,
		Stages:             st,
		Markers:            maps.Clone(p.Markers),
		BaseBranch:         p.BaseBranch,
		BranchPrefix:       p.BranchPrefix,
		PlansDir:           p.PlansDir,
		OwnerIDs:           owners,
		ManualCommentBots:  slices.Clone(p.ManualCommentBots),
	}
}

// WithIdentityIDs returns a copy of the policy with the identities' account
// IDs, once they are known. It fails when an ID is empty, when two identities
// share an account, or when an owner account is one of the identities
// (FR-13 AC1, D2).
func (p Policy) WithIdentityIDs(ids IdentityIDs) (Policy, error) {
	named := []struct{ name, id string }{{"coder", ids.Coder}, {"reviewer", ids.Reviewer}, {"loop", ids.Loop}}
	seen := map[string]string{}
	for _, n := range named {
		if n.id == "" {
			return p, fmt.Errorf("identities.%s: account ID is empty", n.name)
		}
		if other, ok := seen[n.id]; ok {
			return p, fmt.Errorf("identities.%s: account %s is also identities.%s; the three identities must be different (FR-13 AC1)", n.name, n.id, other)
		}
		seen[n.id] = n.name
		if slices.Contains(p.OwnerIDs, n.id) {
			return p, fmt.Errorf("identities.owners: account %s is the %s identity; an owner account can't be one of the three identities (FR-13 AC1)", n.id, n.name)
		}
	}
	p.Identities = ids
	return p, nil
}
