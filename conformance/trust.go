package conformance

import (
	"github.com/mayusuk/agent-loop/internal/ports"
)

func (s *suite) trust(ch ports.ChangeRef) ports.TrustReport {
	s.t.Helper()
	r, err := s.env().Host.Trust(s.ctx(), ch)
	s.must(err)
	return r
}

// untrusted checks that the report names cause and the commit c.
func (s *suite) untrusted(r ports.TrustReport, cause ports.TrustCause, c ports.Commit) {
	s.t.Helper()
	found := false
	for _, x := range r.Causes {
		found = found || x == cause
	}
	if !found {
		s.t.Errorf("Trust causes %v, want %s", r.Causes, cause)
	}
	named := false
	for _, x := range r.Commits {
		named = named || x == c
	}
	if !named {
		s.t.Errorf("Trust commits %v don't name %s", r.Commits, c)
	}
}

// c10: Trust reports a fork, a removed or changed ruleset, a new admin who
// isn't an owner, a branch outside the prefix, and refuses a push by an
// account outside the bypass list.
func c10(s *suite) {
	s.fresh("Trusted", func(s *suite) {
		ch, _ := s.loopChange(s.item())
		if r := s.trust(ch); len(r.Causes) != 0 || len(r.Commits) != 0 {
			s.t.Errorf("a coder change on a loop branch is untrusted: %+v", r)
		}
	})
	s.fresh("Fork", func(s *suite) {
		e := s.env()
		branch := e.Human.Login + ":conformance-fork"
		head, err := e.Driver.Push(e.Human, branch)
		s.must(err)
		ch, err := e.Driver.OpenChange(e.Human, s.item(), branch)
		s.must(err)
		if !s.getChange(ch).Fork {
			s.t.Error("a change from a fork has Fork false")
		}
		s.untrusted(s.trust(ch), ports.TrustFork, head)
	})
	for _, c := range []struct {
		name   string
		action RulesetAction
		cause  ports.TrustCause
	}{
		{"RulesetRemoved", RulesetRemove, ports.TrustRulesetMissing},
		{"RulesetDisabled", RulesetDisable, ports.TrustRulesetMissing},
		{"RulesetChanged", RulesetChange, ports.TrustRulesetChanged},
	} {
		s.fresh(c.name, func(s *suite) {
			ch, head := s.loopChange(s.item())
			s.must(s.env().Driver.SetRuleset(RulesetLoopBranches, c.action))
			s.untrusted(s.trust(ch), c.cause, head)
		})
	}
	s.fresh("RulesetDisabledAndEnabled", func(s *suite) { // I9: strict
		ch, head := s.loopChange(s.item())
		s.must(s.env().Driver.SetRuleset(RulesetBaseBranch, RulesetDisable))
		s.must(s.env().Driver.SetRuleset(RulesetBaseBranch, RulesetEnable))
		s.untrusted(s.trust(ch), ports.TrustRulesetChanged, head)
	})
	s.fresh("AdminNotOwner", func(s *suite) {
		ch, head := s.loopChange(s.item())
		s.must(s.env().Driver.AddAdmin(s.env().Human))
		s.untrusted(s.trust(ch), ports.TrustAdminNotOwner, head)
	})
	s.fresh("BranchOutsidePrefix", func(s *suite) {
		e := s.env()
		head, err := e.Driver.Push(e.Owner, "conformance-feature")
		s.must(err)
		ch, err := e.Driver.OpenChange(e.Owner, s.item(), "conformance-feature")
		s.must(err)
		s.untrusted(s.trust(ch), ports.TrustBranchOutsidePrefix, head)
	})
	s.fresh("PushOutsideBypassList", func(s *suite) {
		e := s.env()
		ch, head := s.loopChange(s.item())
		_, err := e.Driver.Push(e.Human, s.getChange(ch).Branch)
		s.refused(err, "a push to a loop branch by a human who isn't an owner")
		if got := s.getChange(ch).Head; got != head {
			s.t.Errorf("the refused push moved the head to %s", got)
		}
		if r := s.trust(ch); len(r.Causes) != 0 {
			s.t.Errorf("a refused push made the change untrusted: %+v", r)
		}
	})
}

// c13: with the coder handler token, a push to the base branch, a merge of a
// change, and creating a tag are all refused (I10).
func c13(s *suite) {
	e := s.env()
	it := s.item()
	ch, _ := s.loopChange(it)
	s.coderRun(it, func() {
		_, err := e.Driver.Push(e.Coder, s.getChange(ch).Branch)
		s.must(err) // the loop branch accepts the coder
		_, err = e.Driver.Push(e.Coder, e.BaseBranch)
		s.refused(err, "coder push to the base branch")
		s.refused(e.Driver.MergeChange(e.Coder, ch), "coder merge of a change")
		s.refused(e.Driver.CreateTag(e.Coder, "conformance-tag"), "coder tag")
	})
	if c := s.getChange(ch); c.State != ports.ChangeStateOpen {
		s.t.Errorf("the change is %s after a refused coder merge", c.State)
	}
}

// c15: HeadMoved.Pusher is Coder for a push during the item's coder run or
// the worker's base merge, Owner for a push with no live coder token, and
// Unknown for a push during another item's coder run (I11).
func c15(s *suite) {
	e := s.env()
	a, b := s.item(), s.item()
	ch, _ := s.loopChange(a)
	branch := s.getChange(ch).Branch
	_, cur := s.hostFeed("")
	pusher := func(want ports.Commit) ports.Pusher {
		s.t.Helper()
		evs, next := s.hostFeed(cur)
		cur = next
		for _, ev := range evs {
			if hm, ok := ev.Detail.(ports.HeadMoved); ok && ev.Change == ch && hm.Commit == want {
				return hm.Pusher
			}
		}
		s.t.Fatalf("no HeadMoved for %s", want)
		return ""
	}

	coder := s.coderPush(a, ch)
	if p := pusher(coder); p != ports.PusherCoder {
		s.t.Errorf("push during the item's coder run: %s, want coder", p)
	}

	_, err := e.Driver.Push(e.Owner, e.BaseBranch)
	s.must(err)
	var merge ports.Commit
	s.coderRun(a, func() {
		merge, err = e.Driver.MergeBaseInto(e.Coder, branch)
		s.must(err)
	})
	if p := pusher(merge); p != ports.PusherCoder {
		s.t.Errorf("the worker's base merge: %s, want coder", p)
	}

	owner, err := e.Driver.Push(e.Owner, branch)
	s.must(err)
	if p := pusher(owner); p != ports.PusherOwner {
		s.t.Errorf("owner push with no live coder token: %s, want owner", p)
	}

	var unknown ports.Commit
	end, err := e.Driver.StartCoderRun(b)
	s.must(err)
	unknown, err = e.Driver.Push(e.Owner, branch)
	s.must(err)
	if p := pusher(unknown); p != ports.PusherUnknown {
		s.t.Errorf("push during another item's coder run: %s, want unknown", p)
	}
	end()
	s.untrusted(s.trust(ch), ports.TrustPusherUnknown, unknown)
}
