package core

import (
	"slices"
	"testing"
	"time"
)

// Picking and claiming (FR-2), and the unclaimed-item rows of FR-12 AC4.

func cand(key, typ string, age int) Candidate {
	return Candidate{Key: key, Types: []string{typ}, Ready: true, Created: t0.Add(time.Duration(age) * time.Hour)}
}

func verdict(t *testing.T, r PickResult, key string) Verdict {
	t.Helper()
	i := slices.IndexFunc(r.Verdicts, func(v Verdict) bool { return v.Key == key })
	if i < 0 {
		t.Fatalf("no verdict for %s", key)
	}
	return r.Verdicts[i]
}

func TestFR2AC1_EachExclusion(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Candidate)
		want Exclusion
	}{
		{"closed", func(c *Candidate) { c.Closed = true }, ExcludeClosed},
		{"claimed", func(c *Candidate) { c.Claimed = true }, ExcludeClaimed},
		{"plan", func(c *Candidate) { c.Plan = true }, ExcludePlan},
		{"not ready (owner removed ready)", func(c *Candidate) { c.Ready = false }, ExcludeNotReady},
		{"type not configured", func(c *Candidate) { c.Types = []string{"chore"} }, ExcludeType},
		{"no type", func(c *Candidate) { c.Types = nil }, ExcludeType},
		{"human assignee", func(c *Candidate) { c.HumanAssignee = true }, ExcludeHumanAssignee},
		{"aborted", func(c *Candidate) { c.Aborted = true }, ExcludeAborted},
		{"paused", func(c *Candidate) { c.Paused = true }, ExcludePaused},
		{"needs-owner", func(c *Candidate) { c.NeedsOwner = true }, ExcludeNeedsOwner},
		{"dependency open", func(c *Candidate) { c.Dependencies = []Dependency{{"17", DependencyOpen}} }, ExcludeDependencyOpen},
		{"dependency not done", func(c *Candidate) { c.Dependencies = []Dependency{{"17", DependencyNotDone}} }, ExcludeDependencyNotDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cand("42", "feature", 0)
			tc.mod(&c)
			r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{})
			if v := verdict(t, r, "42"); v.Pick || v.Reason != tc.want {
				t.Fatalf("verdict = %+v, want %s", v, tc.want)
			}
			if len(r.Picks) != 0 {
				t.Fatalf("picked %v", r.Picks)
			}
		})
	}
	t.Run("dependencies done", func(t *testing.T) {
		c := cand("42", "bug", 0)
		c.Dependencies = []Dependency{{"17", DependencyDone}}
		if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); !slices.Equal(r.Picks, []string{"42"}) {
			t.Fatalf("picks = %v", r.Picks)
		}
	})
	for _, tc := range []struct {
		name string
		v    ProjectView
		want Exclusion
	}{
		{"project paused", ProjectView{Paused: true}, ExcludeProjectPaused},
		{"deployment paused", ProjectView{DeploymentPaused: true}, ExcludeDeploymentPaused},
		{"daily limit", ProjectView{DailyLimitReached: true}, ExcludeDailyLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Pick("p", []Candidate{cand("42", "bug", 0)}, tc.v, PickPolicy{})
			if len(r.Picks) != 0 || verdict(t, r, "42").Reason != tc.want {
				t.Fatalf("result = %+v", r)
			}
		})
	}
}

func TestFR2AC2_PriorityOrder(t *testing.T) {
	cs := []Candidate{cand("f-old", "feature", 0), cand("b-new", "bug", 5), cand("f-new", "feature", 3), cand("b-old", "bug", 1)}
	r := Pick("p", cs, ProjectView{}, PickPolicy{WIP: 20})
	if want := []string{"b-old", "b-new", "f-old", "f-new"}; !slices.Equal(r.Picks, want) {
		t.Fatalf("default order = %v, want %v", r.Picks, want)
	}
	r = Pick("p", cs, ProjectView{}, PickPolicy{WIP: 20, Priority: []string{"type:feature", "type:bug", "newest"}})
	if want := []string{"f-new", "f-old", "b-new", "b-old"}; !slices.Equal(r.Picks, want) {
		t.Fatalf("profile order = %v, want %v", r.Picks, want)
	}
}

func TestFR2AC3_WIPNeverGoesOverTheLimit(t *testing.T) {
	cs := []Candidate{cand("1", "bug", 0), cand("2", "bug", 1), cand("3", "bug", 2)}
	for claimed := range 4 {
		for wip := 1; wip <= 3; wip++ {
			r := Pick("p", cs, ProjectView{Claimed: claimed}, PickPolicy{WIP: wip})
			if claimed+len(r.Picks) > max(wip, claimed) {
				t.Fatalf("wip %d claimed %d: picks %v", wip, claimed, r.Picks)
			}
			if want := max(0, min(3, wip-claimed)); len(r.Picks) != want {
				t.Fatalf("wip %d claimed %d: %d picks, want %d", wip, claimed, len(r.Picks), want)
			}
		}
	}
	r := Pick("p", cs, ProjectView{Claimed: 2}, PickPolicy{})
	if len(r.Picks) != 0 || verdict(t, r, "1").Reason != ExcludeWIP {
		t.Fatalf("default WIP 2: %+v", r)
	}
}

func TestFR2AC5_ClaimPlan(t *testing.T) {
	c := ClaimFor("eagain", "42", 1)
	if c.WorkflowID != "item/eagain/42" || c.Claim != 2 || c.StatusKey != "item/eagain/42#2:claim" ||
		c.Add != MarkerInProgress || c.Remove != MarkerReady {
		t.Fatalf("claim = %+v", c)
	}
}

func TestFR2AC6_DependencyClosedWithoutBeingDone(t *testing.T) {
	c := cand("42", "bug", 0)
	c.Dependencies = []Dependency{{"17", DependencyNotDone}}
	r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{})
	if len(r.Escalate) != 1 || r.Escalate[0].Dependency != "17" || r.Escalate[0].Key != "item/p/42#0:dependency_not_done:1" {
		t.Fatalf("escalate = %+v", r.Escalate)
	}
	// With needs-owner it is skipped and not escalated again.
	c.NeedsOwner = true
	c.DependencyRecords = 1
	if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); len(r.Escalate) != 0 || len(r.Picks) != 0 {
		t.Fatalf("with needs-owner: %+v", r)
	}
	// The owner removes needs-owner, the dependency still isn't done: once more.
	c = c.ApplyMarker(MarkerChanged{By: RoleOwner, Marker: MarkerNeedsOwner, Added: false})
	if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); len(r.Escalate) != 1 || r.Escalate[0].Key != "item/p/42#0:dependency_not_done:2" {
		t.Fatalf("again: %+v", r.Escalate)
	}
	// The owner changed the dependency: a candidate again.
	c.Dependencies = []Dependency{{"18", DependencyDone}}
	if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); !slices.Equal(r.Picks, []string{"42"}) {
		t.Fatalf("after fix: %+v", r)
	}
}

func TestFR12AC4_UnclaimedItemRows(t *testing.T) {
	t.Run("reopened item is a new candidate", func(t *testing.T) {
		c := cand("42", "bug", 0) // reopened: open, ready, not claimed
		if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); !slices.Equal(r.Picks, []string{"42"}) {
			t.Fatalf("picks = %v", r.Picks)
		}
		c.Ready = false
		if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); len(r.Picks) != 0 {
			t.Fatal("reopened item without ready picked")
		}
	})
	t.Run("owner removes ready", func(t *testing.T) {
		c := cand("42", "bug", 0).ApplyMarker(MarkerChanged{By: RoleOwner, Marker: MarkerReady})
		if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); verdict(t, r, "42").Reason != ExcludeNotReady {
			t.Fatalf("verdict = %+v", r.Verdicts)
		}
	})
	t.Run("owner changes the type label: used for priority", func(t *testing.T) {
		a := cand("1", "feature", 0).ApplyMarker(MarkerChanged{By: RoleOwner, Marker: "type:feature"})
		a = a.ApplyMarker(MarkerChanged{By: RoleOwner, Marker: "type:bug", Added: true})
		b := cand("2", "bug", -1)
		if r := Pick("p", []Candidate{a, b}, ProjectView{}, PickPolicy{}); !slices.Equal(r.Picks, []string{"2", "1"}) {
			t.Fatalf("picks = %v", r.Picks)
		}
	})
	t.Run("owner assigns a human", func(t *testing.T) {
		c := cand("42", "bug", 0)
		c.HumanAssignee = true
		if r := Pick("p", []Candidate{c}, ProjectView{}, PickPolicy{}); len(r.Picks) != 0 {
			t.Fatal("picked an item with a human assignee")
		}
	})
	t.Run("owner opens a change: adopted when picked", func(t *testing.T) {
		h := newHarness(t, Policy{}).start(&Change{Ref: "58", Head: "o1", Pusher: PusherOwner})
		h.reviewKind("round")
	})
}

func TestFR13AC7_NonOwnerLabelOnAnUnclaimedItemHasNoEffect(t *testing.T) {
	c := cand("42", "bug", 0)
	for _, m := range []string{MarkerReady, MarkerPaused, MarkerAborted, MarkerNeedsOwner, MarkerPlan, "type:bug"} {
		for _, added := range []bool{true, false} {
			for _, by := range []Role{RoleOther, RoleCoder, RoleLoop} {
				got := c.ApplyMarker(MarkerChanged{By: by, Marker: m, Added: added})
				if !slices.Equal(got.Types, c.Types) || got.Ready != c.Ready || got.Paused || got.Aborted || got.NeedsOwner || got.Plan {
					t.Fatalf("%s %s %v changed the candidate: %+v", by, m, added, got)
				}
			}
		}
	}
}
