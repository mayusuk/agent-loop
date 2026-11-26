package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// planBranch reports whether branch is a plan branch, <prefix>plan/<key>.
func (h *Host) planBranch(branch string) bool {
	rest, ok := strings.CutPrefix(branch, h.prefix+"plan/")
	return ok && rest != ""
}

// recordPlan returns the plan key in a record's data.
func recordPlan(r ports.Record) string {
	var d struct {
		Plan string `json:"plan"`
	}
	if json.Unmarshal(r.Data, &d) != nil {
		return ""
	}
	return d.Plan
}

// proposalPlan is the plan key of a pull request that is a plan proposal:
// the coder's proposal record on a plan branch (#156 D6).
func (h *Host) proposalPlan(p pullRequest) string {
	if !h.planBranch(p.Head.Ref) {
		return ""
	}
	r, ok := h.ids.recordIn(p.Body, p.User, time.Time{}, "", ports.RecordProposal)
	if !ok {
		return ""
	}
	return recordPlan(r)
}

// openProposal opens a plan proposal from n.Branch as the coder. A pull
// request on the branch with the coder's proposal record for key is
// returned instead (§6.1, #156 D5).
func (h *Host) openProposal(ctx context.Context, n ports.NewChange, key ports.IdemKey) (ports.ChangeRef, error) {
	switch {
	case n.Item != "":
		return "", fmt.Errorf("github: a proposal has no item, got %s: %w", n.Item, ports.ErrConflict)
	case !h.planBranch(n.Branch):
		return "", fmt.Errorf("github: proposal branch %q is not under %splan/: %w", n.Branch, h.prefix, ports.ErrConflict)
	case recordPlan(n.Record) == "":
		return "", fmt.Errorf("github: the proposal record has no plan key: %w", ports.ErrConflict)
	}
	base := n.Base
	if base == "" {
		base = h.base
	}
	ps, err := h.pullsOn(ctx, n.Branch)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if r, ok := h.ids.recordIn(p.Body, p.User, time.Time{}, "", ports.RecordProposal); ok && r.Key == key {
			return ports.ChangeRef(strconv.Itoa(p.Number)), nil
		}
	}
	marker, err := encodeMarker(n.Record, key)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(untrusted.Read(n.Title))
	if title == "" {
		title = "agent-loop: plan " + recordPlan(n.Record)
	}
	in := map[string]any{"title": title, "head": n.Branch, "base": base, "body": withMarker(untrusted.Read(n.Body), marker)}
	var p pullRequest
	if err := h.c.Send(ctx, asCoder, http.MethodPost, h.c.RepoPath("/pulls"), in, &p); err != nil {
		return "", err
	}
	return ports.ChangeRef(strconv.Itoa(p.Number)), nil
}

// escapePath escapes each segment of a repository path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// contentEntry is one entry of the contents API.
type contentEntry struct {
	Type     string `json:"type"` // file, dir, symlink, submodule
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// PlanAt reads the plan directory dir as of the latest base commit that
// changed it: the commits API filtered by path, the commit's pull requests,
// and the contents API pinned to the commit (§8.3, #156 D1–D4).
func (h *Host) PlanAt(ctx context.Context, dir string) (ports.PlanVersion, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" || strings.Contains("/"+dir+"/", "/../") {
		return ports.PlanVersion{}, fmt.Errorf("github: plan directory %q: %w", dir, ports.ErrConflict)
	}
	var cs []struct {
		SHA string `json:"sha"`
	}
	q := url.Values{"sha": {h.base}, "path": {dir}, "per_page": {"1"}}
	if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/commits"), q, &cs); err != nil {
		return ports.PlanVersion{}, err
	}
	if len(cs) == 0 {
		return ports.PlanVersion{}, fmt.Errorf("github: no commit on %s changed %s: %w", h.base, dir, ports.ErrNotFound)
	}
	v := ports.PlanVersion{Commit: ports.Commit(cs[0].SHA)}
	p, err := h.broughtIn(ctx, cs[0].SHA)
	if err != nil {
		return ports.PlanVersion{}, err
	}
	if p != nil {
		v.Change = ports.ChangeRef(strconv.Itoa(p.Number))
		if p.MergedBy != nil {
			a := p.MergedBy.actor()
			v.MergedBy = &a
		}
		if p.MergedAt != nil {
			v.MergedAt = *p.MergedAt
		}
	}
	if v.Files, err = h.planFiles(ctx, dir, dir, cs[0].SHA); err != nil {
		return ports.PlanVersion{}, err
	}
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
	return v, nil
}

// broughtIn returns the pull request merged into the base that brought sha
// in: the one whose merge commit is sha, else the earliest merged. It reads
// that pull request again for merged_by (#156 D3).
func (h *Host) broughtIn(ctx context.Context, sha string) (*pullRequest, error) {
	ps, err := List[pullRequest](ctx, h.c, asLoop, h.c.RepoPath("/commits/%s/pulls", sha), nil)
	if err != nil {
		return nil, err
	}
	var best *pullRequest
	for i := range ps {
		p := &ps[i]
		if !p.merged() || p.MergedAt == nil || p.Base.Ref != h.base {
			continue
		}
		if p.MergeCommitSHA == sha {
			best = p
			break
		}
		if best == nil || p.MergedAt.Before(*best.MergedAt) {
			best = p
		}
	}
	if best == nil {
		return nil, nil
	}
	full, err := h.pull(ctx, ports.ChangeRef(strconv.Itoa(best.Number)))
	if err != nil {
		return nil, err
	}
	return &full, nil
}

// planFiles returns the regular files under path at sha, recursively, with
// their paths relative to root. A path that doesn't exist at sha has no
// files (#156 D4).
func (h *Host) planFiles(ctx context.Context, root, path, sha string) ([]ports.PlanFile, error) {
	var raw json.RawMessage
	err := h.c.Get(ctx, asLoop, h.c.RepoPath("/contents/%s", escapePath(path)), url.Values{"ref": {sha}}, &raw)
	if errors.Is(err, ports.ErrNotFound) && path == root {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []contentEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("github: plan directory %s is not a directory: %w", path, ports.ErrConflict)
	}
	var out []ports.PlanFile
	for _, e := range entries {
		switch e.Type {
		case "dir":
			sub, err := h.planFiles(ctx, root, e.Path, sha)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		case "file":
			f, err := h.planFile(ctx, root, e, sha)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// planFile reads one file at sha. A file over MaxPlanFile has no content.
func (h *Host) planFile(ctx context.Context, root string, e contentEntry, sha string) (ports.PlanFile, error) {
	f := ports.PlanFile{Path: strings.TrimPrefix(e.Path, root+"/"), Size: e.Size}
	if e.Size > ports.MaxPlanFile {
		return f, nil
	}
	var c contentEntry
	if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/contents/%s", escapePath(e.Path)), url.Values{"ref": {sha}}, &c); err != nil {
		return f, err
	}
	if c.Encoding != "base64" {
		return f, nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return f, fmt.Errorf("github: %s at %s: %v: %w", e.Path, sha, err, ports.ErrTransient)
	}
	f.Size = int64(len(b))
	f.Content = untrusted.New(string(b))
	return f, nil
}
