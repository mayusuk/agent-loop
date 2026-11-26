package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// planBranch reports whether branch is a plan branch, <prefix>plan/<key>.
func (f *Fake) planBranch(branch string) bool {
	rest, ok := strings.CutPrefix(branch, f.cfg.BranchPrefix+"plan/")
	return ok && rest != ""
}

// recordPlan returns the plan key in a record's data.
func recordPlan(rec *ports.Record) string {
	if rec == nil {
		return ""
	}
	var d struct {
		Plan string `json:"plan"`
	}
	if json.Unmarshal(rec.Data, &d) != nil {
		return ""
	}
	return d.Plan
}

// checkProposal applies the proposal rules of OpenChange (§8.3, #156 D5).
func (f *Fake) checkProposal(n ports.NewChange, as ports.Identity) error {
	switch {
	case as != ports.IdentityCoder:
		return fmt.Errorf("fake: proposals are opened as the coder, not %s: %w", as, ports.ErrPermission)
	case n.Item != "":
		return fmt.Errorf("fake: a proposal has no item, got %s: %w", n.Item, ports.ErrConflict)
	case !f.planBranch(n.Branch):
		return fmt.Errorf("fake: proposal branch %q is not under %splan/: %w", n.Branch, f.cfg.BranchPrefix, ports.ErrConflict)
	case recordPlan(&n.Record) == "":
		return fmt.Errorf("fake: the proposal record has no plan key: %w", ports.ErrConflict)
	}
	return nil
}

// proposalPlan is the plan key of a proposal: the coder's proposal record
// on a plan branch (#156 D6).
func (f *Fake) proposalPlan(c *change) string {
	if c.rec == nil || c.rec.Type != ports.RecordProposal || !f.counts(c.rec, c.author) || !f.planBranch(c.branch) {
		return ""
	}
	return recordPlan(c.rec)
}

// subtree returns the files under dir, by their path relative to dir.
func subtree(files map[string]string, dir string) map[string]string {
	out := map[string]string{}
	for p, c := range files {
		if rest, ok := strings.CutPrefix(p, dir+"/"); ok {
			out[rest] = c
		}
	}
	return out
}

// changesDir reports whether c changed dir, with the default history
// simplification: a merge counts only if dir differs from every parent
// (#156 D2).
func (f *Fake) changesDir(c *commit, dir string) bool {
	mine := subtree(c.files, dir)
	if len(c.parents) == 0 {
		return len(mine) > 0
	}
	for _, p := range c.parents {
		if maps.Equal(mine, subtree(f.commits[p].files, dir)) {
			return false
		}
	}
	return true
}

// PlanAt reads the plan directory dir as of the latest base commit that
// changed it (§8.3, #156 D1–D4).
func (h *Host) PlanAt(ctx context.Context, dir string) (ports.PlanVersion, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return ports.PlanVersion{}, err
	}
	defer f.mu.Unlock()
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return ports.PlanVersion{}, fmt.Errorf("fake: empty plan directory: %w", ports.ErrConflict)
	}
	var latest *commit
	for sha := range f.reachable(f.branches[f.cfg.BaseBranch]) {
		if c := f.commits[sha]; f.changesDir(c, dir) && (latest == nil || c.seq > latest.seq) {
			latest = c
		}
	}
	if latest == nil {
		return ports.PlanVersion{}, fmt.Errorf("fake: no commit on %s changed %s: %w", f.cfg.BaseBranch, dir, ports.ErrNotFound)
	}
	v := ports.PlanVersion{Commit: latest.sha}
	if c := f.broughtIn(latest.sha); c != nil {
		a := *c.mergedBy
		v.Change, v.MergedBy, v.MergedAt = c.ref, &a, c.mergedAt
	}
	for p, content := range subtree(latest.files, dir) {
		pf := ports.PlanFile{Path: p, Size: int64(len(content))}
		if pf.Size <= ports.MaxPlanFile {
			pf.Content = untrusted.New(content)
		}
		v.Files = append(v.Files, pf)
	}
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
	return v, nil
}

// broughtIn returns the merged change that brought sha into the base: the
// one whose merge commit is sha, else the earliest merged that contains it
// (#156 D3).
func (f *Fake) broughtIn(sha ports.Commit) *change {
	var best *change
	for _, ref := range f.changeOrder {
		c := f.changes[ref]
		if c.state != ports.ChangeStateMerged || c.mergeCommit == "" {
			continue
		}
		if c.mergeCommit == sha {
			return c
		}
		mc := f.commits[c.mergeCommit]
		if !f.reachable(c.mergedHead)[sha] || f.reachable(mc.parents[0])[sha] {
			continue
		}
		if best == nil || c.mergedAt.Before(best.mergedAt) {
			best = c
		}
	}
	return best
}
