package fake

import (
	"context"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// The rulesets INTEGRATION-SPEC §9.8 step 1 requires.
const (
	RulesetLoopBranches = "loop_branches"
	RulesetBaseBranch   = "base_branch"
	RulesetTags         = "tags"
)

// Ruleset actions a test can take.
const (
	RulesetChange  = "change"  // edit it, still active
	RulesetDisable = "disable" // keep it, not enforced
	RulesetEnable  = "enable"
	RulesetRemove  = "remove"
	RulesetRestore = "restore" // create it again, with a new creation time
)

// Trust checks the change as INTEGRATION-SPEC §9.8 step 3 says, with the
// causes and commits of D5.
func (h *Host) Trust(ctx context.Context, ref ports.ChangeRef) (ports.TrustReport, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return ports.TrustReport{}, err
	}
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return ports.TrustReport{}, err
	}
	own := f.ownCommits(f.head(c))
	var rep ports.TrustReport
	seen := map[ports.Commit]bool{}
	add := func(cause ports.TrustCause, cs []*commit) {
		if len(cs) == 0 {
			return
		}
		rep.Causes = append(rep.Causes, cause)
		for _, cm := range cs {
			seen[cm.sha] = true
		}
	}
	where := func(pred func(*commit) bool) []*commit {
		var out []*commit
		for _, cm := range own {
			if pred(cm) {
				out = append(out, cm)
			}
		}
		return out
	}
	all := func(*commit) bool { return true }

	if c.fork() {
		add(ports.TrustFork, own)
	}
	if !c.fork() && !strings.HasPrefix(c.branch, f.cfg.BranchPrefix) {
		add(ports.TrustBranchOutsidePrefix, own)
	}
	missing := false
	for _, rs := range f.rulesets {
		if !rs.exists || !rs.active {
			missing = true
		}
	}
	if missing {
		add(ports.TrustRulesetMissing, own)
	}
	add(ports.TrustRulesetChanged, where(func(cm *commit) bool {
		for _, at := range f.rulesetEvents {
			if cm.at.Before(at) {
				return true
			}
		}
		return false
	}))
	add(ports.TrustPushedBeforeRuleset, where(func(cm *commit) bool {
		for _, iv := range f.loopInactive {
			if iv.contains(cm.at) {
				return true
			}
		}
		return false
	}))
	nonOwnerNow := false
	for _, a := range f.admins {
		if n := len(a.iv); n > 0 && a.iv[n-1].to.IsZero() {
			nonOwnerNow = true
		}
	}
	if nonOwnerNow {
		add(ports.TrustAdminNotOwner, where(all))
	} else {
		add(ports.TrustAdminNotOwner, where(func(cm *commit) bool {
			for _, a := range f.admins {
				for _, iv := range a.iv {
					if iv.contains(cm.at) {
						return true
					}
				}
			}
			return false
		}))
	}
	add(ports.TrustPusherUnknown, where(func(cm *commit) bool {
		return f.attribute(cm) == ports.PusherUnknown
	}))
	for _, cm := range own {
		if seen[cm.sha] {
			rep.Commits = append(rep.Commits, cm.sha)
		}
	}
	return rep, nil
}

// attribute decides who pushed cm, once, at its first observation (D4).
// The previous poll is the last Changes call that returned no events.
func (f *Fake) attribute(cm *commit) ports.Pusher {
	if cm == nil {
		return ports.PusherUnknown
	}
	if cm.attributed {
		return cm.attr
	}
	item := f.itemOfBranch(cm.branch)
	// The window is [previous poll, now], and never starts after the push,
	// so a run live at the push always counts.
	from := f.lastHostPoll
	if cm.at.Before(from) {
		from = cm.at
	}
	cm.attr = ports.PusherOwner
	for _, r := range f.runs {
		if r.item == item && item != "" && r.iv.contains(cm.at) {
			cm.attr = ports.PusherCoder
			break
		}
		if r.iv.overlaps(from, f.now) {
			cm.attr = ports.PusherUnknown
		}
	}
	cm.attributed = true
	return cm.attr
}

// itemOfBranch returns the item a loop branch belongs to.
func (f *Fake) itemOfBranch(branch string) ports.ItemRef {
	for _, c := range f.changes {
		if c.branch == branch && c.item != "" {
			return c.item
		}
	}
	rest, ok := strings.CutPrefix(branch, f.cfg.BranchPrefix)
	if !ok {
		return ""
	}
	for ref := range f.items {
		if f.branchFor(branch, ref) {
			return ref
		}
	}
	return ports.ItemRef(rest)
}

func (f *Fake) liveRun() bool {
	for _, r := range f.runs {
		if r.iv.to.IsZero() {
			return true
		}
	}
	return false
}

func (f *Fake) isAdmin(a ports.Actor) bool {
	if f.isOwner(a) {
		return true
	}
	ad, ok := f.admins[a.ID]
	return ok && len(ad.iv) > 0 && ad.iv[len(ad.iv)-1].to.IsZero()
}

func (f *Fake) enforced(rs string) bool {
	r := f.rulesets[rs]
	return r.exists && r.active
}

// canWrite: the account has contents write on the repository. The reviewer
// has contents read, and bots that aren't identities have none. A coder
// token exists only during a coder run (TECHNICAL-SPEC §7.3).
func (f *Fake) canWrite(a ports.Actor) bool {
	switch {
	case a.ID == f.cfg.Coder.ID:
		return f.liveRun()
	case a.ID == f.cfg.Reviewer.ID:
		return false
	case a.ID == f.cfg.Loop.ID:
		return true
	}
	return a.Kind == ports.ActorHuman
}

// canPush applies the rulesets of §9.8 step 1 to a push to branch.
func (f *Fake) canPush(a ports.Actor, branch string) bool {
	if strings.Contains(branch, ":") { // a fork: the fork owner's repository
		return a.Kind == ports.ActorHuman && strings.HasPrefix(branch, a.Login+":")
	}
	if !f.canWrite(a) {
		return false
	}
	switch {
	case branch == f.cfg.BaseBranch:
		return !f.enforced(RulesetBaseBranch) || f.isAdmin(a) || a.ID == f.cfg.Loop.ID
	case strings.HasPrefix(branch, f.cfg.BranchPrefix):
		return !f.enforced(RulesetLoopBranches) || f.isAdmin(a) || a.ID == f.cfg.Coder.ID
	}
	return true
}

func (f *Fake) setRuleset(name, action string) bool {
	rs, ok := f.rulesets[name]
	if !ok {
		return false
	}
	now := f.tick()
	wasLoopActive := name == RulesetLoopBranches && rs.exists && rs.active
	switch action {
	case RulesetChange:
		if !rs.exists {
			return false
		}
	case RulesetDisable:
		rs.active = false
	case RulesetEnable:
		rs.active = true
	case RulesetRemove:
		rs.exists = false
	case RulesetRestore:
		rs.exists, rs.active, rs.createdAt = true, true, now
	default:
		return false
	}
	f.rulesetEvents = append(f.rulesetEvents, now)
	if name == RulesetLoopBranches {
		active := rs.exists && rs.active
		switch {
		case wasLoopActive && !active:
			f.loopInactive = append(f.loopInactive, interval{from: now})
		case !wasLoopActive && active && len(f.loopInactive) > 0:
			f.loopInactive[len(f.loopInactive)-1].to = now.Add(-time.Nanosecond)
		}
	}
	return true
}
