package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// textDependency is a "Blocked by #n" line, which the loop doesn't read
// (§9.5). Its group is the leading list of issue references ("#2, #3 and
// #4"); issues the line mentions after that list aren't blockers.
var textDependency = regexp.MustCompile(`(?im)^\s*[-*]?\s*blocked by:?\s+(#\d+(?:\s*,?\s*(?:and\s+|&\s*)?#\d+)*)`)

var issueNumber = regexp.MustCompile(`#(\d+)`)

// textDependencyHint names how to add each "Blocked by #n" of body as a
// native dependency of issue n: with the gh CLI, or on GitHub (work plan
// #104 D3). The REST call takes the blocker's ID, not its number.
func textDependencyHint(repo string, n int, body string) string {
	var blockers []string
	seen := map[string]bool{}
	for _, m := range textDependency.FindAllStringSubmatch(body, -1) {
		for _, b := range issueNumber.FindAllStringSubmatch(m[1], -1) {
			if !seen[b[1]] {
				seen[b[1]] = true
				blockers = append(blockers, b[1])
			}
		}
	}
	var cmds []string
	for _, b := range blockers {
		cmds = append(cmds, fmt.Sprintf("gh api -X POST repos/%s/issues/%d/dependencies/blocked_by -F issue_id=$(gh api repos/%s/issues/%s --jq .id)", repo, n, repo, b))
	}
	return fmt.Sprintf("add the dependency with GitHub's \"Blocked by\" relationship instead (§9.5): run %s; or on GitHub, open issue #%d and add it under Relationships, \"Mark as blocked by\"",
		strings.Join(cmds, " and "), n)
}

// Validate checks the tracker part of the profile against the repository
// (work plan D16). App permissions are checked by the CodeHost's
// Validate (#16 D8).
func (t *Tracker) Validate(ctx context.Context, p ports.ProfileTracker) []ports.Problem {
	var out []ports.Problem
	add := func(sev ports.Severity, code, msg, hint string) {
		out = append(out, ports.Problem{Severity: sev, Code: code, Message: msg, Hint: hint})
	}
	if p.Adapter != "github" {
		add(ports.SeverityError, "wrong_adapter", fmt.Sprintf("the profile names tracker adapter %q, not github", p.Adapter), `set tracker.adapter = "github"`)
		return out
	}
	if !strings.EqualFold(p.Repo, t.c.Repo()) {
		add(ports.SeverityError, "wrong_repo", fmt.Sprintf("the profile names repository %q, but the adapter is for %q", p.Repo, t.c.Repo()), "use the repository the GitHub Apps are installed on")
	}
	markers := make([]string, 0, len(p.Markers))
	for m := range p.Markers {
		markers = append(markers, string(m))
	}
	sort.Strings(markers)
	for _, m := range markers {
		mm := p.Markers[ports.Marker(m)]
		switch {
		case mm.Status != "":
			add(ports.SeverityError, "status_marker_unsupported", fmt.Sprintf("marker %s maps to a workflow status, which GitHub issues don't have", m), fmt.Sprintf("map markers.%s to a label", m))
		case mm.Label == "":
			add(ports.SeverityError, "marker_unmapped", fmt.Sprintf("marker %s maps to no label", m), fmt.Sprintf("set markers.%s.label, or remove it to use the default", m))
		}
	}
	for _, o := range p.Owners {
		if _, err := strconv.ParseInt(o.ID, 10, 64); err != nil {
			add(ports.SeverityError, "owner_id_not_numeric", fmt.Sprintf("owner %q has ID %q, which isn't a GitHub account ID", o.Login, o.ID), fmt.Sprintf("set the id from https://api.github.com/users/%s", o.Login))
			continue
		}
		out = append(out, t.validateOwner(ctx, o)...)
	}

	labels, err := List[label](ctx, t.c, asLoop, t.c.RepoPath("/labels"), nil)
	if err != nil {
		add(ports.SeverityError, "labels_unreadable", fmt.Sprintf("can't list the repository's labels: %v", err), "check the loop app is installed with issues: write")
		return out
	}
	have := map[string]bool{}
	for _, l := range labels {
		have[strings.ToLower(l.Name)] = true
	}
	want := make([]string, 0, len(t.labels))
	for m, l := range t.labels {
		if !have[strings.ToLower(l)] {
			want = append(want, string(m)+"="+l)
		}
	}
	sort.Strings(want)
	for _, w := range want {
		m, l, _ := strings.Cut(w, "=")
		add(ports.SeverityError, "label_missing", fmt.Sprintf("label %q for marker %s doesn't exist in %s", l, m, t.c.Repo()), "run agent-loop init, or create the label")
	}

	cands, err := t.Candidates(ctx)
	if err != nil {
		add(ports.SeverityError, "issues_unreadable", fmt.Sprintf("can't list ready issues: %v", err), "check the loop app is installed with issues: write")
		return out
	}
	for _, c := range cands {
		it, err := t.issue(ctx, c.Ref)
		if err != nil || !textDependency.MatchString(it.Body) {
			continue
		}
		deps, err := t.blockedBy(ctx, it.Number)
		if err == nil && len(deps) == 0 {
			add(ports.SeverityWarning, "text_dependency", fmt.Sprintf("ready issue #%d says \"Blocked by #n\" in its text, which the loop doesn't read", it.Number), textDependencyHint(t.c.Repo(), it.Number, it.Body))
		}
	}
	return out
}

// validateOwner checks that an owner account exists and is a human, and
// warns when its login changed (FR-13 AC1, INTEGRATION-SPEC §13, work plan
// #23 D7).
func (t *Tracker) validateOwner(ctx context.Context, o ports.OwnerAccount) []ports.Problem {
	var u user
	err := t.c.Get(ctx, asLoop, "/user/"+o.ID, nil, &u)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return []ports.Problem{{Severity: ports.SeverityError, Code: "owner_missing",
			Message: fmt.Sprintf("owner %q: no GitHub account has ID %s", o.Login, o.ID),
			Hint:    fmt.Sprintf("set the id from https://api.github.com/users/%s", o.Login)}}
	case err != nil:
		return []ports.Problem{{Severity: ports.SeverityError, Code: "owner_unreadable",
			Message: fmt.Sprintf("can't read owner account %s: %v", o.ID, err), Hint: "check the loop app is installed on the repository"}}
	}
	var out []ports.Problem
	if u.Type != "User" {
		out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "owner_not_human",
			Message: fmt.Sprintf("owner account %s (%s) is a %s, not a human account", o.ID, u.Login, u.Type),
			Hint:    "list only human accounts under identities.owners (FR-13 AC1)"})
	}
	if o.Login != "" && !strings.EqualFold(o.Login, u.Login) {
		out = append(out, ports.Problem{Severity: ports.SeverityWarning, Code: "owner_login_changed",
			Message: fmt.Sprintf("owner account %s is now %q, not %q; the ID is what counts", o.ID, u.Login, o.Login),
			Hint:    fmt.Sprintf("set login = %q for this owner", u.Login)})
	}
	return out
}
