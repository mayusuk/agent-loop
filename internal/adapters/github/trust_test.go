package github_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// coderChange pushes the item's loop branch during a coder run and opens
// its change, as the worker does.
func (p *stubProject) coderChange(t *testing.T, item ports.ItemRef) (ports.ChangeRef, ports.Commit) {
	t.Helper()
	end := p.runs.Start(item)
	defer end()
	return p.open(t, item, "agent/"+string(item))
}

// coderPush pushes one commit to branch during a coder run for item.
func (p *stubProject) coderPush(t *testing.T, item ports.ItemRef, branch string) ports.Commit {
	t.Helper()
	end := p.runs.Start(item)
	defer end()
	c, err := p.driver.Push(actorOf(p.coder), branch)
	must(t, err)
	return c
}

func (p *stubProject) trust(t *testing.T, ch ports.ChangeRef) ports.TrustReport {
	t.Helper()
	r, err := p.host.Trust(ctx, ch)
	must(t, err)
	return r
}

func causes(r ports.TrustReport) string {
	var out []string
	for _, c := range r.Causes {
		out = append(out, string(c))
	}
	return strings.Join(out, ",")
}

func wantTrust(t *testing.T, what string, r ports.TrustReport, cs string, commits ...ports.Commit) {
	t.Helper()
	if got := causes(r); got != cs {
		t.Errorf("%s: causes %q, want %q", what, got, cs)
	}
	if !slices.Equal(r.Commits, commits) {
		t.Errorf("%s: commits %v, want %v", what, r.Commits, commits)
	}
}

// TestFR13AC5_TrustCauses: each cause of §9.8 step 3, the commits it
// names, and that base commits are never checked (work plan #16 D3–D7).
func TestFR13AC5_TrustCauses(t *testing.T) {
	t.Run("Trusted", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, it)
		wantTrust(t, "coder change", p.trust(t, ch), "")
		// The owner's push with no coder run live is trusted too.
		_, err := p.driver.Push(actorOf(p.owner), "agent/"+string(it))
		must(t, err)
		wantTrust(t, "owner push", p.trust(t, ch), "")
	})
	t.Run("BaseCommitsAreNotChecked", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		ch, head := p.coderChange(t, it)
		p.srv.Advance(2 * time.Minute)
		// A base commit pushed before the ruleset change is merged in by
		// the worker's base merge: only the loop branch's commits count.
		_, err := p.driver.Push(actorOf(p.owner), "main")
		must(t, err)
		end := p.runs.Start(it)
		merge, err := p.driver.MergeBaseInto(actorOf(p.coder), "agent/"+string(it))
		end()
		must(t, err)
		must(t, p.driver.SetRuleset(githubtest.RulesetTags, "change"))
		wantTrust(t, "after a tag ruleset change", p.trust(t, ch), "ruleset_changed", head, merge)
	})
	t.Run("Fork", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		alice := actorOf(p.human)
		head, err := p.driver.Push(alice, "alice:fix")
		must(t, err)
		ch, err := p.driver.OpenChange(alice, it, "alice:fix")
		must(t, err)
		c, err := p.host.Change(ctx, ch)
		must(t, err)
		if !c.Fork || c.Branch != "fix" {
			t.Errorf("fork change: %+v", c)
		}
		wantTrust(t, "fork", p.trust(t, ch), "fork", head)
	})
	t.Run("BranchOutsidePrefix", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		head, err := p.driver.Push(actorOf(p.owner), "feature")
		must(t, err)
		ch, err := p.driver.OpenChange(actorOf(p.owner), it, "feature")
		must(t, err)
		wantTrust(t, "outside the prefix", p.trust(t, ch), "branch_outside_prefix", head)
	})
	for _, c := range []struct {
		name, ruleset, action, causes string
	}{
		{"LoopRulesetRemoved", githubtest.RulesetLoopBranches, "remove", "ruleset_missing"},
		{"BaseRulesetDisabled", githubtest.RulesetBaseBranch, "disable", "ruleset_missing"},
		{"TagRulesetRemoved", githubtest.RulesetTags, "remove", "ruleset_missing"},
		{"LoopRulesetChanged", githubtest.RulesetLoopBranches, "change", "ruleset_changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newStub(t, 100)
			it := p.item(t, actorOf(p.owner))
			ch, head := p.coderChange(t, it)
			must(t, p.driver.SetRuleset(c.ruleset, c.action))
			wantTrust(t, c.name, p.trust(t, ch), c.causes, head)
		})
	}
	t.Run("LoopRulesetRestored", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		ch, head := p.coderChange(t, it)
		must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "remove"))
		must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "restore"))
		wantTrust(t, "restored", p.trust(t, ch), "ruleset_changed,pushed_before_ruleset", head)
	})
	t.Run("RulesetOfTheWrongShape", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		ch, head := p.coderChange(t, it)
		// The reviewer app may bypass the loop branches: not the required
		// ruleset any more (D3).
		rs := p.srv.Rulesets()[0]
		body := map[string]any{"bypass_actors": []any{
			map[string]any{"actor_type": "Integration", "actor_id": coderApp},
			map[string]any{"actor_type": "Integration", "actor_id": reviewerApp}}}
		must(t, p.driverCall("PUT", "/rulesets/"+itoa(rs.ID), body))
		wantTrust(t, "wrong bypass", p.trust(t, ch), "ruleset_missing", head)
	})
	t.Run("RulesetsRefused", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		ch, head := p.coderChange(t, it)
		p.srv.Fail("GET", `/rulesets$`, 403, nil, `{"message":"Upgrade to GitHub Pro"}`, 0)
		wantTrust(t, "refused", p.trust(t, ch), "ruleset_missing", head)
	})
	t.Run("AdminNotOwner", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		ch, head := p.coderChange(t, it)
		must(t, p.driver.AddAdmin(actorOf(p.human)))
		wantTrust(t, "new admin", p.trust(t, ch), "admin_not_owner", head)
		// After the admin is gone, the commits from before stay untrusted
		// (like a ruleset change), and a later push is trusted (D5).
		must(t, p.driver.RemoveAdmin(actorOf(p.human)))
		p.srv.Advance(2 * time.Minute)
		_, _ = p.feed(t, "")
		p.srv.Advance(time.Second)
		p.coderPush(t, it, "agent/"+string(it))
		wantTrust(t, "admin removed", p.trust(t, ch), "admin_not_owner", head)
	})
	t.Run("PusherUnknown", func(t *testing.T) {
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		_, cur := p.feed(t, "")
		end := p.runs.Start(b)
		pushed, err := p.driver.Push(actorOf(p.owner), "agent/"+string(a))
		must(t, err)
		evs, _ := p.feed(t, cur)
		end()
		if got := describe(evs); !slices.Contains(got, string(ch)+" ports.HeadMoved unknown") {
			t.Errorf("feed: %v", got)
		}
		// Decided at first sight: the run has ended, the push is still
		// unknown.
		wantTrust(t, "push during another item's run", p.trust(t, ch), "pusher_unknown", pushed)
	})
	t.Run("FirstSeenByTrust", func(t *testing.T) {
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		_, _ = p.feed(t, "")
		end := p.runs.Start(b)
		pushed, err := p.driver.Push(actorOf(p.owner), "agent/"+string(a))
		must(t, err)
		end()
		wantTrust(t, "unknown push seen by Trust", p.trust(t, ch), "pusher_unknown", pushed)
	})
	t.Run("MissingChange", func(t *testing.T) {
		p := newStub(t, 100)
		if _, err := p.host.Trust(ctx, "999"); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("missing change: %v", err)
		}
	})
}

// driverCall makes a raw API call as the owner, for settings the Driver has
// no action for.
func (p *stubProject) driverCall(method, path string, in any) error {
	return p.driver.Call(actorOf(p.owner), method, path, in, nil)
}

// feed reads the host's feed from cur until a call returns no events.
func (p *stubProject) feed(t *testing.T, cur ports.Cursor) ([]ports.ChangeEvent, ports.Cursor) {
	t.Helper()
	var all []ports.ChangeEvent
	for {
		evs, next, err := p.host.Changes(ctx, cur)
		must(t, err)
		cur = next
		if len(evs) == 0 {
			return all, cur
		}
		all = append(all, evs...)
	}
}

// TestTrustStateCachedForOnePollInterval: the rulesets and admins are read
// at most once per poll interval, and a failed read isn't cached (D6).
func TestTrustStateCachedForOnePollInterval(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	ch, head := p.coderChange(t, it)
	wantTrust(t, "first", p.trust(t, ch), "")
	must(t, p.driver.SetRuleset(githubtest.RulesetBaseBranch, "disable"))
	wantTrust(t, "cached", p.trust(t, ch), "")
	p.srv.Advance(time.Minute)
	wantTrust(t, "after the interval", p.trust(t, ch), "ruleset_missing", head)

	p.srv.Advance(time.Minute)
	p.srv.Fail("GET", `/collaborators$`, 500, nil, "", 1)
	if _, err := p.host.Trust(ctx, ch); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("failed admin read: %v", err)
	}
	wantTrust(t, "after a failed read", p.trust(t, ch), "ruleset_missing", head)
}

// TestC15_PusherWithoutRunLog: a host with no run records attributes every
// push to Unknown, so nothing it can't vouch for is trusted (D1).
func TestC15_PusherWithoutRunLog(t *testing.T) {
	p := newStub(t, 100)
	h, err := github.NewHost(p.client, github.HostConfig{
		Identities: github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)},
		Owners:     []string{itoa(p.owner.ID)},
		Apps:       github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp},
	})
	must(t, err)
	_, _, err = h.Changes(ctx, "")
	must(t, err)
	it := p.item(t, actorOf(p.owner))
	ch, head := p.coderChange(t, it)
	r, err := h.Trust(ctx, ch)
	must(t, err)
	wantTrust(t, "no run log", r, "pusher_unknown", head)
}

// TestFR13AC6_OwnerApprovalTrustsCommits: after the owner's resume writes
// an approved_commits record, the named commits are trusted; a later
// untrusted commit is untrusted again; a fork is never approved (core #11
// D3, D5).
func TestFR13AC6_OwnerApprovalTrustsCommits(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	ch, head := p.coderChange(t, it)
	must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "disable"))
	must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "enable"))
	r := p.trust(t, ch)
	v := coreReport(r).Check(nil)
	if !v.Untrusted || !slices.Equal(v.Commits, []string{string(head)}) {
		t.Fatalf("before approval: %+v", v)
	}

	// The resume: the loop writes approved_commits with the report's
	// commits, and the next check reads them back.
	approve := func(key ports.IdemKey, commits []string) {
		data, _ := json.Marshal(map[string]any{"type": ports.RecordApprovedCommits, "v": 1, "key": key, "commits": commits})
		rec := ports.Record{Type: ports.RecordApprovedCommits, Key: key, Data: data}
		_, err := p.tracker.Comment(ctx, it, rec, untrusted.New("Approved by the owner's resume."), key)
		must(t, err)
	}
	approved := func() []string {
		recs, err := p.tracker.FindRecords(ctx, it, ports.RecordApprovedCommits)
		must(t, err)
		var out []string
		for _, rec := range recs {
			var d struct {
				Commits []string `json:"commits"`
			}
			must(t, json.Unmarshal(rec.Data, &d))
			out = append(out, d.Commits...)
		}
		return out
	}
	approve("approve:1", v.Commits)
	if v := coreReport(p.trust(t, ch)).Check(approved()); v.Untrusted {
		t.Errorf("approved commits still untrusted: %+v", v)
	}

	// Another ruleset change: only the new commit isn't approved.
	p.srv.Advance(2 * time.Minute)
	later := p.coderPush(t, it, "agent/"+string(it))
	must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "change"))
	p.srv.Advance(2 * time.Minute)
	v = coreReport(p.trust(t, ch)).Check(approved())
	if !v.Untrusted || !slices.Equal(v.Commits, []string{string(later)}) {
		t.Errorf("after a second change: %+v", v)
	}

	// A fork is never approved, even with every commit approved.
	fork, err := p.driver.Push(actorOf(p.human), "alice:fix")
	must(t, err)
	fch, err := p.driver.OpenChange(actorOf(p.human), it, "alice:fix")
	must(t, err)
	approve("approve:2", []string{string(fork)})
	if v := coreReport(p.trust(t, fch)).Check(approved()); !v.Untrusted || !v.Fork {
		t.Errorf("approved fork: %+v", v)
	}
}

func coreReport(r ports.TrustReport) core.TrustReport {
	var c core.TrustReport
	for _, x := range r.Causes {
		c.Causes = append(c.Causes, string(x))
	}
	for _, x := range r.Commits {
		c.Commits = append(c.Commits, string(x))
	}
	return c
}

// restartHost returns a new Host on p's repository and run log, as after a
// restart: it has seen no poll. base is the profile's base branch.
func (p *stubProject) restartHost(t *testing.T, base string) *github.Host {
	t.Helper()
	h, err := github.NewHost(p.client, github.HostConfig{
		Identities:  github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)},
		BaseBranch:  base,
		Owners:      []string{itoa(p.owner.ID)},
		Apps:        github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp},
		Runs:        p.runs,
		Permissions: p.perms,
	})
	must(t, err)
	return h
}

// TestC15_PusherWindowEdges: the push window of §9.8 step 6 never comes
// from commit data, and a run of this item in the window doesn't vouch for
// the part of the window outside it while another item's token was live
// (work plan #16 D2).
func TestC15_PusherWindowEdges(t *testing.T) {
	t.Run("OtherRunAfterOwnRunInOneWindow", func(t *testing.T) {
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		_, cur := p.feed(t, "")
		p.srv.Advance(10 * time.Second)
		endA := p.runs.Start(a)
		p.srv.Advance(10 * time.Second)
		endA()
		p.srv.Advance(10 * time.Second)
		endB := p.runs.Start(b)
		pushed, err := p.driver.Push(actorOf(p.owner), "agent/"+string(a))
		must(t, err)
		evs, _ := p.feed(t, cur)
		endB()
		if got := describe(evs); !slices.Contains(got, string(ch)+" ports.HeadMoved unknown") {
			t.Errorf("feed: %v", got)
		}
		wantTrust(t, "push after own run, during another's", p.trust(t, ch), "pusher_unknown", pushed)
	})
	t.Run("WindowInsideOwnRun", func(t *testing.T) {
		// The push happened during this item's run: Coder, even though
		// another item's run was live too (§9.8 step 6's accepted gap).
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		endA := p.runs.Start(a)
		p.srv.Advance(time.Second)
		_, cur := p.feed(t, "")
		endB := p.runs.Start(b)
		pushed, err := p.driver.Push(actorOf(p.coder), "agent/"+string(a))
		must(t, err)
		evs, _ := p.feed(t, cur)
		endB()
		endA()
		if got := describe(evs); !slices.Contains(got, string(ch)+" ports.HeadMoved coder") {
			t.Errorf("feed: %v", got)
		}
		_ = pushed
		wantTrust(t, "push inside own run", p.trust(t, ch), "")
	})
	t.Run("RunHeadRecorded", func(t *testing.T) {
		// The window also covers another item's run, but the head is the
		// one this item's run recorded at its end: Coder.
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		_, cur := p.feed(t, "")
		p.srv.Advance(10 * time.Second)
		endB := p.runs.Start(b)
		p.srv.Advance(10 * time.Second)
		endA := p.runs.StartWithHead(a)
		pushed, err := p.driver.Push(actorOf(p.coder), "agent/"+string(a))
		must(t, err)
		p.srv.Advance(10 * time.Second)
		endA(pushed)
		p.srv.Advance(10 * time.Second)
		evs, _ := p.feed(t, cur)
		endB()
		if got := describe(evs); !slices.Contains(got, string(ch)+" ports.HeadMoved coder") {
			t.Errorf("feed: %v", got)
		}
		wantTrust(t, "the run's recorded head", p.trust(t, ch), "")
	})
	t.Run("AfterRestartOldCursor", func(t *testing.T) {
		// A new Host with no poll and a cursor two hours old: the window
		// starts at the cursor, this item's run lies inside it, and
		// another item's token was live at the push.
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		_, cur := p.feed(t, "")
		p.srv.Advance(time.Hour)
		p.coderPush(t, a, "agent/"+string(a))
		p.srv.Advance(time.Hour)
		endB := p.runs.Start(b)
		pushed, err := p.driver.Push(actorOf(p.owner), "agent/"+string(a))
		must(t, err)
		endB()
		h := p.restartHost(t, "")
		evs, _, err := h.Changes(ctx, cur)
		must(t, err)
		if got := describe(evs); !slices.Contains(got, string(ch)+" ports.HeadMoved unknown") {
			t.Errorf("feed: %v", got)
		}
		r, err := h.Trust(ctx, ch)
		must(t, err)
		if !slices.Contains(r.Causes, ports.TrustPusherUnknown) || !slices.Contains(r.Commits, pushed) {
			t.Errorf("after a restart: %+v, want pusher_unknown naming %s", r, pushed)
		}
	})
	t.Run("BackdatedCommitterDate", func(t *testing.T) {
		// Another item's run pushes to this item's branch with a committer
		// date inside this item's earlier run. Trust sees it before the
		// next poll: the window starts at the previous poll, not the date.
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		runStart := p.srv.Now()
		ch, _ := p.coderChange(t, a)
		p.srv.Advance(10 * time.Minute)
		_, _ = p.feed(t, "")
		p.srv.Advance(5 * time.Minute)
		endB := p.runs.Start(b)
		pushed, err := p.driver.Push(actorOf(p.coder), "agent/"+string(a))
		must(t, err)
		p.srv.SetCommitDate(string(pushed), runStart)
		wantTrust(t, "backdated", p.trust(t, ch), "pusher_unknown", pushed)
		endB()
	})
	t.Run("FutureCommitterDateAfterRestart", func(t *testing.T) {
		// After a restart, a commit pushed while the loop ruleset was
		// disabled claims a date in 2099. Trust still reports the ruleset
		// change: with no poll yet, the window starts at the earliest
		// possible time.
		p := newStub(t, 100)
		a := p.item(t, actorOf(p.owner))
		ch, _ := p.coderChange(t, a)
		p.srv.Advance(time.Minute)
		must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "disable"))
		pushed, err := p.driver.Push(actorOf(p.human), "agent/"+string(a))
		must(t, err)
		must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "enable"))
		p.srv.SetCommitDate(string(pushed), time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
		p.srv.Advance(time.Minute)
		r, err := p.restartHost(t, "").Trust(ctx, ch)
		must(t, err)
		if !slices.Contains(r.Causes, ports.TrustRulesetChanged) || !slices.Contains(r.Commits, pushed) {
			t.Errorf("future date after a restart: %+v, want ruleset_changed naming %s", r, pushed)
		}
	})
	t.Run("EarlierCommitsTakeTheHeadsWindow", func(t *testing.T) {
		// Two commits pushed together: the feed sees the head, and Trust
		// gives the earlier commit the head's window, whatever its date.
		p := newStub(t, 100)
		a, b := p.item(t, actorOf(p.owner)), p.item(t, actorOf(p.owner))
		runStart := p.srv.Now()
		ch, _ := p.coderChange(t, a)
		p.srv.Advance(10 * time.Minute)
		_, cur := p.feed(t, "")
		p.srv.Advance(time.Minute)
		endB := p.runs.Start(b)
		first, err := p.driver.Push(actorOf(p.coder), "agent/"+string(a))
		must(t, err)
		second, err := p.driver.Push(actorOf(p.coder), "agent/"+string(a))
		must(t, err)
		p.srv.SetCommitDate(string(first), runStart)
		_, _ = p.feed(t, cur)
		endB()
		wantTrust(t, "pushed together", p.trust(t, ch), "pusher_unknown", first, second)
	})
}

// TestFR13AC5_DefaultBranchRuleset: a base-branch ruleset on
// ~DEFAULT_BRANCH counts only when the base is the default branch (§9.8
// step 1, I10, work plan #16 D3).
func TestFR13AC5_DefaultBranchRuleset(t *testing.T) {
	onDefault := func(p *stubProject) {
		rs := p.srv.Rulesets()[1] // base branch
		must(t, p.driverCall("PUT", "/rulesets/"+itoa(rs.ID), map[string]any{
			"conditions": map[string]any{"ref_name": map[string]any{"include": []any{"~DEFAULT_BRANCH"}, "exclude": []any{}}}}))
		p.srv.Advance(time.Minute)
		_, _ = p.feed(t, "") // a poll after the change, before the push
	}
	t.Run("BaseIsDefault", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		onDefault(p)
		ch, _ := p.coderChange(t, it)
		wantTrust(t, "default base", p.trust(t, ch), "")
		p.cleanRepo()
		if got := problems(t, p.host.Validate(ctx, goodHostProfile())); got != "" {
			t.Errorf("validate: %q", got)
		}
	})
	t.Run("BaseIsNotDefault", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		onDefault(p)
		_, err := p.driver.Push(actorOf(p.owner), "develop")
		must(t, err)
		ch, head := p.coderChange(t, it)
		h := p.restartHost(t, "develop")
		r, err := h.Trust(ctx, ch)
		must(t, err)
		if !slices.Contains(r.Causes, ports.TrustRulesetMissing) || !slices.Contains(r.Commits, head) {
			t.Errorf("trust: %+v, want ruleset_missing naming %s", r, head)
		}
		p.cleanRepo()
		prof := goodHostProfile()
		prof.BaseBranch = "develop"
		got := problems(t, h.Validate(ctx, prof))
		if !strings.Contains(got, "error:ruleset_missing") {
			t.Errorf("validate: %q, want error:ruleset_missing", got)
		}
	})
	t.Run("RepositoryUnreadable", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		onDefault(p)
		ch, _ := p.coderChange(t, it)
		p.srv.Fail("GET", `^/repos/acme/widgets$`, 500, nil, "", 0)
		if _, err := p.host.Trust(ctx, ch); !errors.Is(err, ports.ErrTransient) {
			t.Errorf("trust with the repository unreadable: %v", err)
		}
		p.cleanRepo()
		if got := problems(t, p.host.Validate(ctx, goodHostProfile())); !strings.Contains(got, "error:rulesets_unreadable") {
			t.Errorf("validate: %q", got)
		}
	})
}

// TestC13_RuleViolationIsPermissionAtAnyStatus: the conformance driver
// reads a ruleset refusal as ErrPermission whether GitHub answers it with
// 403, 405, 409 or 422, so C13 doesn't hang on the stub's guess of the
// status (work plan #16 D11). Other refusals keep their status.
func TestC13_RuleViolationIsPermissionAtAnyStatus(t *testing.T) {
	for _, code := range []int{403, 405, 409, 422} {
		p := newStub(t, 100)
		p.srv.Fail("PUT", `/pulls/1/merge$`, code, nil, `{"message":"Repository rule violations found\n\n- Cannot update this protected ref."}`, 1)
		if err := p.driver.Call(actorOf(p.coder), "PUT", "/pulls/1/merge", map[string]any{}, nil); !errors.Is(err, ports.ErrPermission) {
			t.Errorf("%d rule violation: %v, want ErrPermission", code, err)
		}
	}
	p := newStub(t, 100)
	p.srv.Fail("PUT", `/pulls/1/merge$`, 405, nil, `{"message":"Pull Request is not mergeable"}`, 1)
	if err := p.driver.Call(actorOf(p.coder), "PUT", "/pulls/1/merge", map[string]any{}, nil); err == nil || errors.Is(err, ports.ErrPermission) {
		t.Errorf("405 not mergeable: %v, want a plain error", err)
	}
}
