package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// CoderRun is one coder run from the worker's run records: the item it was
// for, and when its push token was live (INTEGRATION-SPEC §9.8 step 6). The
// worker's conflict-free base merge is one too.
type CoderRun struct {
	Item  ports.ItemRef
	Start time.Time
	End   time.Time // zero while the run is live
	// Head is the run's head at its end: its done.head, or the branch head
	// after failed. Empty while the run is live, or when none was recorded.
	Head ports.Commit
}

// RunLog is the worker's coder-run records (work plan #16 D1).
type RunLog interface {
	// CoderRuns returns the project's coder runs whose token was live at
	// any time in [from, to].
	CoderRuns(ctx context.Context, from, to time.Time) ([]CoderRun, error)
}

// AppPermissions reads the permissions each identity's GitHub App
// installation has on a repository (work plan #16 D8).
type AppPermissions interface {
	InstalledPermissions(ctx context.Context, id ports.Identity, repo string) (map[string]string, error)
}

// AppIDs are the GitHub App IDs of the identities, as ruleset bypass lists
// name them (an app's ID, not its bot user's).
type AppIDs struct {
	Coder, Reviewer, Loop int64
}

// pushObs is what was decided about one commit at its first observation:
// who pushed it, and the start of the window its push lies in (work plan
// #16 D2).
type pushObs struct {
	pusher ports.Pusher
	from   time.Time
}

// overlaps reports whether the run's token was live at some time in
// [from, to].
func (r CoderRun) overlaps(from, to time.Time) bool {
	return !r.Start.After(to) && (r.End.IsZero() || !r.End.Before(from))
}

// attribute decides who pushed sha, a commit on item's change, once (work
// plan #16 D2). The push lies in [from, to]: from is the earliest it can
// have happened, never taken from commit data, and to is when it was first
// seen (now when zero).
func (h *Host) attribute(ctx context.Context, sha string, item ports.ItemRef, from, to time.Time) (pushObs, error) {
	h.mu.Lock()
	o, ok := h.pushers[sha]
	h.mu.Unlock()
	if ok {
		return o, nil
	}
	now := to
	if now.IsZero() {
		now = h.c.now()
	}
	if from.After(now) {
		from = now
	}
	o = pushObs{pusher: ports.PusherUnknown, from: from}
	if h.runs != nil {
		// Every run up to now: a run that recorded sha as its head vouches
		// for it wherever the window lies (review R6).
		runs, err := h.runs.CoderRuns(ctx, time.Time{}, now)
		if err != nil {
			return pushObs{}, err
		}
		o.pusher = pusherIn(runs, sha, item, from, now)
	}
	return h.keep(sha, o), nil
}

// keep records o as sha's observation unless one was recorded first, and
// returns the one recorded.
func (h *Host) keep(sha string, o pushObs) pushObs {
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev, ok := h.pushers[sha]; ok {
		return prev
	}
	h.pushers[sha] = o
	return o
}

// pusherIn applies §9.8 step 6 to a push of sha in [from, to] (work plan
// #16 D2). Coder when any run of item recorded sha as its head, in the
// window or not, or when every part of the window outside item's runs had
// no other run's token live; Owner when no run's token was live in the
// window; Unknown otherwise.
func pusherIn(runs []CoderRun, sha string, item ports.ItemRef, from, to time.Time) ports.Pusher {
	for _, r := range runs {
		if item != "" && r.Item == item && r.Head != "" && string(r.Head) == sha {
			return ports.PusherCoder
		}
	}
	var own, other []CoderRun
	for _, r := range runs {
		if !r.overlaps(from, to) {
			continue
		}
		if item != "" && r.Item == item {
			own = append(own, r)
		} else {
			other = append(other, r)
		}
	}
	switch {
	case len(own) == 0 && len(other) == 0:
		return ports.PusherOwner
	case len(own) == 0:
		return ports.PusherUnknown
	}
	// The parts of [from, to] that no run of item covers.
	slices.SortFunc(own, func(a, b CoderRun) int { return a.Start.Compare(b.Start) })
	var gaps [][2]time.Time
	cur, covered := from, false
	for _, r := range own {
		if r.Start.After(cur) {
			gaps = append(gaps, [2]time.Time{cur, r.Start.Add(-time.Nanosecond)})
		}
		if r.End.IsZero() {
			covered = true
			break
		}
		if e := r.End.Add(time.Nanosecond); e.After(cur) {
			cur = e
		}
	}
	if !covered && !cur.After(to) {
		gaps = append(gaps, [2]time.Time{cur, to})
	}
	for _, o := range other {
		for _, g := range gaps {
			if o.overlaps(g[0], g[1]) {
				return ports.PusherUnknown
			}
		}
	}
	return ports.PusherCoder
}

// afterPoll is the start of the push window of a commit that wasn't there
// at the previous poll: just after it (work plan #16 D2).
func afterPoll(poll time.Time) time.Time { return poll.Add(time.Nanosecond) }

// The rulesets INTEGRATION-SPEC §9.8 step 1 requires.
type rulesetKind int

const (
	rulesetLoop rulesetKind = iota
	rulesetBase
	rulesetTags
)

func (k rulesetKind) String() string {
	return [...]string{"loop branches", "base branch", "tags"}[k]
}

// adminRole is the RepositoryRole ID of GitHub's admin role.
const adminRole = 5

type rulesetSummary struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
	SourceType  string `json:"source_type"`
}

type rulesetDetail struct {
	rulesetSummary
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Conditions struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type string `json:"type"`
	} `json:"rules"`
	BypassActors []struct {
		ActorID   *int64 `json:"actor_id"`
		ActorType string `json:"actor_type"`
	} `json:"bypass_actors"`
}

func (d rulesetDetail) hasRule(t string) bool {
	for _, r := range d.Rules {
		if r.Type == t {
			return true
		}
	}
	return false
}

func (d rulesetDetail) bypasses(typ string, id int64) bool {
	for _, b := range d.BypassActors {
		if b.ActorType == typ && b.ActorID != nil && *b.ActorID == id {
			return true
		}
	}
	return false
}

// includes reports the ref_name include patterns that select kind k.
func (h *Host) includes(k rulesetKind, base string) []string {
	switch k {
	case rulesetLoop:
		return []string{"refs/heads/" + h.prefix + "**"}
	case rulesetBase:
		// ~DEFAULT_BRANCH was resolved by rulesets (work plan #16 D3).
		return []string{"refs/heads/" + base}
	}
	return []string{"refs/tags/**", "~ALL"}
}

// aimsAt reports whether d is meant to be the ruleset of kind k: its target
// and an include pattern are k's.
func (h *Host) aimsAt(d rulesetDetail, k rulesetKind, base string) bool {
	target := "branch"
	if k == rulesetTags {
		target = "tag"
	}
	if d.Target != target {
		return false
	}
	for _, p := range d.Conditions.RefName.Include {
		if slices.Contains(h.includes(k, base), p) {
			return true
		}
	}
	return false
}

// basePushRules are the only rules the base-branch ruleset may hold: it
// restricts pushes and nothing else, because the loop app bypasses it.
var basePushRules = []string{"creation", "update", "deletion", "non_fast_forward"}

// misfit says why d isn't the ruleset of kind k (work plan #16 D3), or ""
// when it is.
func (h *Host) misfit(d rulesetDetail, k rulesetKind, base string) string {
	if d.SourceType != "" && d.SourceType != "Repository" {
		return fmt.Sprintf("ruleset %q is an %s ruleset, not a repository one", d.Name, strings.ToLower(d.SourceType))
	}
	if !h.aimsAt(d, k, base) {
		return fmt.Sprintf("ruleset %q doesn't target %s", d.Name, strings.Join(h.includes(k, base), " or "))
	}
	if d.Enforcement != "active" {
		return fmt.Sprintf("ruleset %q is %s, not active", d.Name, d.Enforcement)
	}
	if len(d.Conditions.RefName.Exclude) > 0 {
		return fmt.Sprintf("ruleset %q excludes %s", d.Name, strings.Join(d.Conditions.RefName.Exclude, ", "))
	}
	var want []string
	switch k {
	case rulesetLoop:
		want = []string{"creation", "update", "deletion", "non_fast_forward"}
	case rulesetBase:
		want = []string{"update", "deletion", "non_fast_forward"}
	default:
		want = []string{"creation", "update", "deletion"}
	}
	for _, r := range want {
		if !d.hasRule(r) {
			return fmt.Sprintf("ruleset %q lacks the rule %s", d.Name, r)
		}
	}
	if k == rulesetBase {
		// The loop app bypasses this ruleset, so it may hold only the push
		// rules: the project's own base rules (required checks, pull
		// requests) must still apply to its merges (§9.8 step 1).
		for _, r := range d.Rules {
			if !slices.Contains(basePushRules, r.Type) {
				return fmt.Sprintf("ruleset %q also holds the rule %s, which the loop app would bypass", d.Name, r.Type)
			}
		}
	}
	for _, b := range d.BypassActors {
		id := int64(-1)
		if b.ActorID != nil {
			id = *b.ActorID
		}
		ok := b.ActorType == "RepositoryRole" && id == adminRole
		if b.ActorType == "Integration" {
			switch k {
			case rulesetLoop:
				ok = h.apps.Coder != 0 && id == h.apps.Coder
			case rulesetBase:
				ok = h.apps.Loop != 0 && id == h.apps.Loop
			default:
				ok = slices.Contains(h.releaseApps, id)
			}
		}
		if !ok {
			return fmt.Sprintf("ruleset %q lets %s %d bypass it", d.Name, b.ActorType, id)
		}
	}
	return ""
}

// rulesets reads the repository's rulesets in full. refused is true when
// GitHub refuses to list them (403 or 404: not supported on the plan, or no
// administration: read).
func (h *Host) rulesets(ctx context.Context) (ds []rulesetDetail, refused bool, err error) {
	list, err := List[rulesetSummary](ctx, h.c, asLoop, h.c.RepoPath("/rulesets"), nil)
	if isStatus(err, http.StatusForbidden, http.StatusNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, s := range list {
		var d rulesetDetail
		if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/rulesets/%d", s.ID), nil, &d); err != nil {
			return nil, false, err
		}
		if d.SourceType == "" {
			d.SourceType = s.SourceType
		}
		ds = append(ds, d)
	}
	if err := h.resolveDefaultBranch(ctx, ds); err != nil {
		return nil, false, err
	}
	return ds, false, nil
}

// resolveDefaultBranch replaces ~DEFAULT_BRANCH in the rulesets' ref_name
// conditions with the repository's default branch, so a ruleset on it
// counts for the base branch only when the base is the default branch
// (work plan #16 D3). The repository is read only when a ruleset names it.
func (h *Host) resolveDefaultBranch(ctx context.Context, ds []rulesetDetail) error {
	def := ""
	resolve := func(ps []string) error {
		for i, p := range ps {
			if p != "~DEFAULT_BRANCH" {
				continue
			}
			if def == "" {
				var repo struct {
					DefaultBranch string `json:"default_branch"`
				}
				if err := h.c.Get(ctx, asLoop, h.c.RepoPath(""), nil, &repo); err != nil {
					return err
				}
				if repo.DefaultBranch == "" {
					return fmt.Errorf("github: %s has no default branch", h.c.Repo())
				}
				def = repo.DefaultBranch
			}
			ps[i] = "refs/heads/" + def
		}
		return nil
	}
	for i := range ds {
		if err := resolve(ds[i].Conditions.RefName.Include); err != nil {
			return err
		}
		if err := resolve(ds[i].Conditions.RefName.Exclude); err != nil {
			return err
		}
	}
	return nil
}

type collaborator struct {
	user
	Permissions struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

// admins lists the accounts with admin permission on the repository,
// organisation owners included (§9.8 step 2).
func (h *Host) admins(ctx context.Context) ([]collaborator, error) {
	q := url.Values{"permission": {"admin"}, "affiliation": {"all"}}
	cs, err := List[collaborator](ctx, h.c, asLoop, h.c.RepoPath("/collaborators"), q)
	if err != nil {
		return nil, err
	}
	var out []collaborator
	for _, c := range cs {
		if c.Permissions.Admin {
			out = append(out, c)
		}
	}
	return out, nil
}

// trustState is the repository-wide part of the trust check, read at most
// once per poll interval (work plan #16 D6).
type trustState struct {
	at       time.Time
	found    [3]*rulesetDetail // by rulesetKind; nil when missing
	nonOwner []collaborator
}

func (h *Host) trustState(ctx context.Context) (*trustState, error) {
	now := h.c.now()
	h.mu.Lock()
	st := h.trust
	h.mu.Unlock()
	if st != nil && now.Sub(st.at) < h.poll && !now.Before(st.at) {
		return st, nil
	}
	ds, _, err := h.rulesets(ctx)
	if err != nil {
		return nil, err
	}
	st = &trustState{at: now}
	for k := rulesetLoop; k <= rulesetTags; k++ {
		for i := range ds {
			if h.misfit(ds[i], k, h.base) == "" {
				st.found[k] = &ds[i]
				break
			}
		}
	}
	admins, err := h.admins(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range admins {
		if !h.owners[strconv.FormatInt(a.ID, 10)] {
			st.nonOwner = append(st.nonOwner, a)
		}
	}
	h.mu.Lock()
	h.trust = st
	if len(st.nonOwner) > 0 && now.After(h.adminSeen) {
		h.adminSeen = now
	}
	h.mu.Unlock()
	return st, nil
}

// compareCommit is a commit the compare lists. Its dates are set by the
// pusher, so trust never reads them (work plan #16 D2).
type compareCommit struct {
	SHA string `json:"sha"`
}

// Trust checks the change as INTEGRATION-SPEC §9.8 step 3 says, over the
// head's commits not reachable from the base tip, with the causes and
// commits of work plan #16 D7.
func (h *Host) Trust(ctx context.Context, ref ports.ChangeRef) (ports.TrustReport, error) {
	p, err := h.pull(ctx, ref)
	if err != nil {
		return ports.TrustReport{}, err
	}
	cmp, err := h.compare(ctx, p.Base.Ref, p.Head.SHA)
	if err != nil {
		return ports.TrustReport{}, err
	}
	own := cmp.Commits
	var rep ports.TrustReport
	if len(own) == 0 {
		return rep, nil
	}
	named := map[string]bool{}
	add := func(cause ports.TrustCause, pred func(i int) bool) {
		hit := false
		for i, c := range own {
			if pred(i) {
				hit, named[c.SHA] = true, true
			}
		}
		if hit {
			rep.Causes = append(rep.Causes, cause)
		}
	}
	all := func(int) bool { return true }
	finish := func() ports.TrustReport {
		for _, c := range own {
			if named[c.SHA] {
				rep.Commits = append(rep.Commits, ports.Commit(c.SHA))
			}
		}
		return rep
	}
	v, err := h.view(ctx, p)
	if err != nil {
		return ports.TrustReport{}, err
	}
	if v.Fork {
		add(ports.TrustFork, all)
		return finish(), nil
	}
	st, err := h.trustState(ctx)
	if err != nil {
		return ports.TrustReport{}, err
	}
	h.mu.Lock()
	adminSeen := h.adminSeen
	from, read := h.windowStart(), h.hasRead()
	h.mu.Unlock()
	if !read {
		// No push window yet: every commit would count as pushed at the
		// zero time. Retry once this host has read the feed (work plan #16
		// D2, review R6).
		return ports.TrustReport{}, fmt.Errorf("%w: trust: the host has not read the change feed yet", ports.ErrTransient)
	}
	// Newest first: a head a feed read saw has the window of that read; a
	// commit no read saw takes the window of the nearest later commit one
	// did, the push it came with; failing that, the window since the
	// previous poll (work plan #16 D2).
	obs := make([]pushObs, len(own))
	var later *pushObs
	for i := len(own) - 1; i >= 0; i-- {
		sha := own[i].SHA
		h.mu.Lock()
		o, seen := h.pushers[sha]
		w, old := h.seen[sha]
		h.mu.Unlock()
		switch {
		case seen:
		case old:
			if o, err = h.attribute(ctx, sha, v.Item, w[0], w[1]); err != nil {
				return ports.TrustReport{}, err
			}
		case later != nil:
			o = h.keep(sha, *later)
		default:
			if o, err = h.attribute(ctx, sha, v.Item, from, time.Time{}); err != nil {
				return ports.TrustReport{}, err
			}
		}
		obs[i] = o
		later = &obs[i]
	}

	if !strings.HasPrefix(p.Head.Ref, h.prefix) {
		add(ports.TrustBranchOutsidePrefix, all)
	}
	missing := st.found[rulesetLoop] == nil || st.found[rulesetBase] == nil || st.found[rulesetTags] == nil
	if missing {
		add(ports.TrustRulesetMissing, all)
	}
	var changed time.Time
	for _, d := range st.found {
		if d != nil && d.UpdatedAt.After(changed) {
			changed = d.UpdatedAt
		}
	}
	add(ports.TrustRulesetChanged, func(i int) bool { return obs[i].from.Before(changed) })
	if loop := st.found[rulesetLoop]; loop != nil {
		add(ports.TrustPushedBeforeRuleset, func(i int) bool { return obs[i].from.Before(loop.CreatedAt) })
	}
	if len(st.nonOwner) > 0 {
		add(ports.TrustAdminNotOwner, all)
	} else {
		add(ports.TrustAdminNotOwner, func(i int) bool { return obs[i].from.Before(adminSeen) })
	}
	add(ports.TrustPusherUnknown, func(i int) bool { return obs[i].pusher == ports.PusherUnknown })
	return finish(), nil
}
