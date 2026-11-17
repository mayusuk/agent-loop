package core

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Candidate is an item as the supervisor reads it for picking (FR-2 AC1).
// Its markers are the effective ones: a label change by an account that
// isn't an owner is left out by whoever builds it (FR-13 AC7, ApplyMarker).
type Candidate struct {
	Key     string
	Types   []string // the item's type markers without "type:", e.g. "bug"
	Created time.Time

	Ready, Claimed, Closed, HumanAssignee bool
	Aborted, Paused, NeedsOwner, Plan     bool

	Dependencies []Dependency
	// DependencyRecords counts the FR-2 AC6 status records already on the
	// item; it makes the next one's idempotency key (work plan #12 D15).
	DependencyRecords int
}

// Dependency is one blocking dependency and its state (INTEGRATION-SPEC
// §9.5): open, done, or not_done (closed any other way).
type Dependency struct {
	Key   string
	State DependencyState
}

// ApplyMarker applies a label change to an unclaimed candidate. Only an
// owner's change counts (FR-13 AC7).
func (c Candidate) ApplyMarker(e MarkerChanged) Candidate {
	if e.By != RoleOwner || e.OnChange {
		return c
	}
	switch e.Marker {
	case MarkerReady:
		c.Ready = e.Added
	case MarkerAborted:
		c.Aborted = e.Added
	case MarkerPaused:
		c.Paused = e.Added
	case MarkerNeedsOwner:
		c.NeedsOwner = e.Added
	case MarkerPlan:
		c.Plan = e.Added
	default:
		if t, ok := strings.CutPrefix(e.Marker, "type:"); ok {
			c.Types = slices.DeleteFunc(slices.Clone(c.Types), func(x string) bool { return x == t })
			if e.Added {
				c.Types = append(c.Types, t)
			}
		}
	}
	return c
}

// PickPolicy is the profile's picking settings (TECHNICAL-SPEC §13). Zero
// values use the defaults.
type PickPolicy struct {
	Types    []string // default feature, bug
	Priority []string // default type:bug, type:feature, oldest
	WIP      int      // default 2
}

// DefaultWIP is the default WIP limit (FR-2 AC3).
const DefaultWIP = 2

func (p PickPolicy) types() []string {
	if len(p.Types) == 0 {
		return []string{"feature", "bug"}
	}
	return p.Types
}

func (p PickPolicy) priority() []string {
	if len(p.Priority) == 0 {
		return []string{"type:bug", "type:feature", "oldest"}
	}
	return p.Priority
}

// ProjectView is the project's state that limits picking (FR-2 AC1, AC3).
type ProjectView struct {
	Paused, DeploymentPaused bool
	DailyLimitReached        bool
	// Claimed counts claimed items that aren't closed or released, in every
	// claimed state (FR-2 AC3).
	Claimed int
}

// Exclusion is why a candidate isn't picked (INTEGRATION-SPEC §12.3).
type Exclusion string

// The exclusions, in the order Pick checks them.
const (
	ExcludeClosed            Exclusion = "closed"
	ExcludeClaimed           Exclusion = "claimed"
	ExcludePlan              Exclusion = "plan"
	ExcludeNotReady          Exclusion = "not_ready"
	ExcludeType              Exclusion = "type_not_configured"
	ExcludeHumanAssignee     Exclusion = "human_assignee"
	ExcludeAborted           Exclusion = "aborted"
	ExcludePaused            Exclusion = "paused"
	ExcludeNeedsOwner        Exclusion = "needs_owner"
	ExcludeDependencyNotDone Exclusion = "dependency_not_done"
	ExcludeDependencyOpen    Exclusion = "dependency_open"
	ExcludeDeploymentPaused  Exclusion = "deployment_paused"
	ExcludeProjectPaused     Exclusion = "project_paused"
	ExcludeDailyLimit        Exclusion = "daily_limit"
	ExcludeWIP               Exclusion = "wip_full"
)

// Verdict is one candidate's outcome. Pick is true, or Reason says why not
// (with the dependency it names, if any).
type Verdict struct {
	Key        string
	Pick       bool
	Reason     Exclusion
	Dependency string
}

// DependencyEscalation is the FR-2 AC6 escalation of an unclaimed item: add
// needs-owner and comment naming the dependency.
type DependencyEscalation struct {
	Item       string
	Dependency string
	Key        string
}

// PickResult is a supervisor pass's picking decision.
type PickResult struct {
	// Verdicts has every candidate, in priority order (FR-1 AC6).
	Verdicts []Verdict
	// Picks are the items to claim, in order; never more than the free WIP.
	Picks    []string
	Escalate []DependencyEscalation
}

// Pick decides which candidates to claim (FR-2 AC1–AC3, AC6). It is pure:
// the same input gives the same result.
func Pick(project string, cands []Candidate, v ProjectView, p PickPolicy) PickResult {
	sorted := slices.Clone(cands)
	prio := p.priority()
	newest := slices.Contains(prio, "newest")
	slices.SortStableFunc(sorted, func(a, b Candidate) int {
		if ra, rb := rank(a, prio), rank(b, prio); ra != rb {
			return ra - rb
		}
		if !a.Created.Equal(b.Created) {
			if a.Created.Before(b.Created) != newest {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	wip := p.WIP
	if wip <= 0 {
		wip = DefaultWIP
	}
	free := wip - v.Claimed
	var res PickResult
	for _, c := range sorted {
		vd := Verdict{Key: c.Key}
		vd.Reason, vd.Dependency = exclusion(c, p.types())
		if vd.Reason == ExcludeDependencyNotDone && !c.NeedsOwner {
			res.Escalate = append(res.Escalate, DependencyEscalation{Item: c.Key, Dependency: vd.Dependency,
				Key: fmt.Sprintf("item/%s/%s#0:dependency_not_done:%d", project, c.Key, c.DependencyRecords+1)})
		}
		if vd.Reason == "" {
			switch {
			case v.DeploymentPaused:
				vd.Reason = ExcludeDeploymentPaused
			case v.Paused:
				vd.Reason = ExcludeProjectPaused
			case v.DailyLimitReached:
				vd.Reason = ExcludeDailyLimit
			case len(res.Picks) >= free:
				vd.Reason = ExcludeWIP
			default:
				vd.Pick = true
				res.Picks = append(res.Picks, c.Key)
			}
		}
		res.Verdicts = append(res.Verdicts, vd)
	}
	return res
}

// rank is the index of the first priority entry naming one of the item's
// types; items with none come after every listed type.
func rank(c Candidate, prio []string) int {
	for i, e := range prio {
		if t, ok := strings.CutPrefix(e, "type:"); ok && slices.Contains(c.Types, t) {
			return i
		}
	}
	return len(prio)
}

// exclusion is the first FR-2 AC1 rule that excludes the candidate. A
// not-done dependency comes before an open one (FR-2 AC6).
func exclusion(c Candidate, types []string) (Exclusion, string) {
	switch {
	case c.Closed:
		return ExcludeClosed, ""
	case c.Claimed:
		return ExcludeClaimed, ""
	case c.Plan:
		return ExcludePlan, ""
	case !c.Ready:
		return ExcludeNotReady, ""
	case !slices.ContainsFunc(c.Types, func(t string) bool { return slices.Contains(types, t) }):
		return ExcludeType, ""
	case c.HumanAssignee:
		return ExcludeHumanAssignee, ""
	case c.Aborted:
		return ExcludeAborted, ""
	case c.Paused:
		return ExcludePaused, ""
	}
	for _, dep := range c.Dependencies {
		if dep.State == DependencyNotDone {
			if c.NeedsOwner {
				return ExcludeNeedsOwner, dep.Key
			}
			return ExcludeDependencyNotDone, dep.Key
		}
	}
	if c.NeedsOwner {
		return ExcludeNeedsOwner, ""
	}
	for _, dep := range c.Dependencies {
		if dep.State != DependencyDone {
			return ExcludeDependencyOpen, dep.Key
		}
	}
	return "", ""
}

// ClaimPlan is what claiming one item does (FR-2 AC5, TECHNICAL-SPEC §6.1,
// INTEGRATION-SPEC §9.3): add in-progress, remove ready, assign the coder
// where the tracker allows, post the claim record, then start the item's
// workflow. If the claim fails, nothing starts.
type ClaimPlan struct {
	WorkflowID string
	Item       string
	Claim      int
	StatusKey  string
	Add        string
	Remove     string
}

// ClaimFor plans the claim of an item whose earlier claims left
// claimRecords claim records (TECHNICAL-SPEC §6.1).
func ClaimFor(project, item string, claimRecords int) ClaimPlan {
	wf := fmt.Sprintf("item/%s/%s", project, item)
	n := claimRecords + 1
	return ClaimPlan{WorkflowID: wf, Item: item, Claim: n, StatusKey: fmt.Sprintf("%s#%d:claim", wf, n),
		Add: MarkerInProgress, Remove: MarkerReady}
}
