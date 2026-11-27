package githubtest

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// commit is one commit of the stub's git model: its parents and a full
// snapshot of the files.
type commit struct {
	sha     string
	parents []string
	files   map[string]string
	author  *User
	at      time.Time
	date    time.Time // the committer date, which the pusher sets; at when zero
	seq     int       // creation order
}

type status struct {
	context, state string
	at             time.Time
}

type checkRun struct {
	id                       int64
	name, status, conclusion string
	startedAt                time.Time
}

// git is the repository content and settings: commits, branches, commit
// statuses, check runs and the base branch's rules.
type git struct {
	commits   map[string]*commit
	branches  map[string]string // name to tip
	seq       int
	statuses  map[string][]status
	checkRuns map[string][]checkRun

	rulesetChecks []string // required checks from a branch ruleset on the default branch
	classicChecks []string // required checks from classic protection
	classic       bool     // classic protection is set
	approvals     int      // required approving reviews (classic protection)
	strict        bool     // branches must be up to date before merging
	defaultBranch string
}

func (s *Server) initGit() {
	s.git = git{commits: map[string]*commit{}, branches: map[string]string{}, statuses: map[string][]status{},
		checkRuns: map[string][]checkRun{}, defaultBranch: "main"}
	root := s.newCommit(nil, map[string]string{"README.md": "stub repository\n"}, nil)
	s.branches["main"] = root.sha
}

func (s *Server) newCommit(parents []string, files map[string]string, author *User) *commit {
	s.seq++
	h := sha1.Sum([]byte(fmt.Sprintf("%s/%s commit %d", s.Owner, s.Name, s.seq)))
	c := &commit{sha: hex.EncodeToString(h[:]), parents: parents, files: files, author: author, at: s.now, seq: s.seq}
	s.commits[c.sha] = c
	return c
}

// SetCommitDate sets a commit's committer date, as a pusher can with git
// (GIT_COMMITTER_DATE): any time, earlier or later than the push.
func (s *Server) SetCommitDate(sha string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.commits[sha]; c != nil {
		c.date = at
	}
}

// ancestors returns every commit reachable from sha, itself included.
func (s *Server) ancestors(sha string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{sha}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		if c := s.commits[x]; c != nil {
			stack = append(stack, c.parents...)
		}
	}
	return seen
}

// mergeBase returns the newest common ancestor of a and b.
func (s *Server) mergeBase(a, b string) string {
	in := s.ancestors(b)
	best := ""
	for x := range s.ancestors(a) {
		if in[x] && (best == "" || s.commits[x].at.After(s.commits[best].at) ||
			(s.commits[x].at.Equal(s.commits[best].at) && x > best)) {
			best = x
		}
	}
	return best
}

// merge3 merges the files of ours and theirs from their merge base: a file
// changed on one side only takes that side. Both sides changing a file
// keeps ours (the stub doesn't model conflicts).
func (s *Server) merge3(ours, theirs string) map[string]string {
	base := s.commits[s.mergeBase(ours, theirs)]
	o, t := s.commits[ours].files, s.commits[theirs].files
	out := map[string]string{}
	paths := map[string]bool{}
	for p := range o {
		paths[p] = true
	}
	for p := range t {
		paths[p] = true
	}
	for p := range paths {
		bv, bok := "", false
		if base != nil {
			bv, bok = base.files[p]
		}
		ov, ook := o[p]
		tv, tok := t[p]
		switch {
		case ook == bok && ov == bv:
			if tok {
				out[p] = tv
			}
		default:
			if ook {
				out[p] = ov
			}
		}
	}
	return out
}

// resolve turns a branch name or SHA into a commit.
func (s *Server) resolve(ref string) *commit {
	if sha, ok := s.branches[ref]; ok {
		return s.commits[sha]
	}
	return s.commits[ref]
}

// moveBranch sets a branch's tip and updates the open pull requests from
// it, as a push does.
func (s *Server) moveBranch(branch string, c *commit) {
	old := s.branches[branch]
	s.branches[branch] = c.sha
	for _, it := range s.issues {
		if !it.pull || it.pr == nil || it.head != branch || it.state != "open" {
			continue
		}
		it.pr.headSHA = c.sha
		it.updatedAt = s.now
		// The new commits, oldest first, as PullRequestCommit items.
		seen := s.ancestors(old)
		for _, x := range s.newCommits(c.sha, seen) {
			it.pr.timeline = append(it.pr.timeline, &tlItem{kind: "PullRequestCommit", at: s.commits[x].at, commit: x})
		}
	}
}

// newCommits returns the commits reachable from tip and not in seen,
// oldest first.
func (s *Server) newCommits(tip string, seen map[string]bool) []string {
	var out []string
	for x := range s.ancestors(tip) {
		if !seen[x] {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := s.commits[out[i]], s.commits[out[j]]
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		return out[i] < out[j]
	})
	return out
}

func copyFiles(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func blobSHA(content string) string {
	h := sha1.Sum([]byte("blob " + content))
	return hex.EncodeToString(h[:])
}

// RequireChecks makes the named checks required on the default branch, by
// a branch ruleset.
func (s *Server) RequireChecks(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rulesetChecks = append(s.rulesetChecks, names...)
}

// RequireClassicChecks sets classic branch protection with the named
// required checks.
func (s *Server) RequireClassicChecks(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.classic = true
	s.classicChecks = append(s.classicChecks, names...)
}

// RequireUpToDate sets whether branches must be up to date before merging.
func (s *Server) RequireUpToDate(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strict = on
}

// AddCheckRun adds a check run on a commit: status "queued", "in_progress"
// or "completed", with a conclusion when completed.
func (s *Server) AddCheckRun(sha, name, status, conclusion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick()
	s.checkRuns[sha] = append(s.checkRuns[sha], checkRun{id: s.id(), name: name, status: status, conclusion: conclusion, startedAt: s.now})
}

// Branch returns a branch's tip, for tests.
func (s *Server) Branch(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.branches[name]
}

func (s *Server) getBranch(_ *User, m []string, _ url.Values, _ map[string]any) response {
	name, _ := url.PathUnescape(m[1])
	sha, ok := s.branches[name]
	if !ok {
		return errorResponse(http.StatusNotFound, "Branch not found")
	}
	return response{body: map[string]any{"name": name, "commit": map[string]any{"sha": sha}}}
}

func (s *Server) createRef(u *User, _ []string, _ url.Values, in map[string]any) response {
	ref, _ := in["ref"].(string)
	sha, _ := in["sha"].(string)
	if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok && s.commits[sha] != nil && s.forkLogin == "" {
		return s.createTag(u, tag, sha)
	}
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || s.commits[sha] == nil {
		return errorResponse(http.StatusUnprocessableEntity, "Reference update failed")
	}
	if _, exists := s.branches[name]; exists {
		return errorResponse(http.StatusUnprocessableEntity, "Reference already exists")
	}
	if r, no := s.refusedPush(u, name); no {
		return r
	}
	s.branches[name] = sha
	return response{status: http.StatusCreated, body: map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}}
}

func (s *Server) getContents(_ *User, m []string, q url.Values, _ map[string]any) response {
	path, _ := url.PathUnescape(m[1])
	ref := q.Get("ref")
	if ref == "" {
		ref = s.defaultBranch
	}
	c := s.resolve(ref)
	if c == nil {
		return errorResponse(http.StatusNotFound, "No commit found for the ref")
	}
	content, ok := c.files[path]
	if !ok {
		return s.listDir(c, path)
	}
	return response{body: map[string]any{"type": "file", "path": path, "sha": blobSHA(content), "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte(content))}}
}

// putContents commits one file to a branch, as the contents API does.
func (s *Server) putContents(u *User, m []string, _ url.Values, in map[string]any) response {
	path, _ := url.PathUnescape(m[1])
	branch, _ := in["branch"].(string)
	if branch == "" {
		branch = s.defaultBranch
	}
	tip := s.resolve(branch)
	if tip == nil {
		return errorResponse(http.StatusNotFound, "Branch not found")
	}
	if r, no := s.refusedPush(u, branch); no {
		return r
	}
	enc, _ := in["content"].(string)
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return errorResponse(http.StatusUnprocessableEntity, "content is not valid Base64")
	}
	if old, ok := tip.files[path]; ok {
		if sha, _ := in["sha"].(string); sha != blobSHA(old) {
			return errorResponse(http.StatusConflict, path+" does not match "+blobSHA(old))
		}
	}
	s.tick()
	files := copyFiles(tip.files)
	files[path] = string(raw)
	c := s.newCommit([]string{tip.sha}, files, u)
	s.moveBranch(branch, c)
	return response{status: http.StatusOK, body: map[string]any{"commit": map[string]any{"sha": c.sha},
		"content": map[string]any{"path": path, "sha": blobSHA(string(raw))}}}
}

// mergeBranches merges head into the base branch (POST /merges).
func (s *Server) mergeBranches(u *User, _ []string, _ url.Values, in map[string]any) response {
	base, _ := in["base"].(string)
	head, _ := in["head"].(string)
	b, h := s.resolve(base), s.resolve(head)
	if b == nil || h == nil || s.branches[base] == "" {
		return errorResponse(http.StatusNotFound, "Base or head does not exist")
	}
	if s.ancestors(b.sha)[h.sha] {
		return response{status: http.StatusNoContent}
	}
	if r, no := s.refusedPush(u, base); no {
		return r
	}
	s.tick()
	c := s.newCommit([]string{b.sha, h.sha}, s.merge3(b.sha, h.sha), u)
	s.moveBranch(base, c)
	return response{status: http.StatusCreated, body: map[string]any{"sha": c.sha}}
}

func (s *Server) compare(_ *User, m []string, _ url.Values, _ map[string]any) response {
	bn, _ := url.PathUnescape(m[1])
	hn, _ := url.PathUnescape(m[2])
	b, h := s.resolve(bn), s.resolve(hn)
	if b == nil || h == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	ab, ah := s.ancestors(b.sha), s.ancestors(h.sha)
	ahead := s.newCommits(h.sha, ab)
	behind := s.newCommits(b.sha, ah)
	st := "identical"
	switch {
	case len(ahead) > 0 && len(behind) > 0:
		st = "diverged"
	case len(ahead) > 0:
		st = "ahead"
	case len(behind) > 0:
		st = "behind"
	}
	commits := []any{}
	for _, x := range ahead {
		c := s.commits[x]
		d := c.at
		if !c.date.IsZero() {
			d = c.date
		}
		at := d.Format(time.RFC3339)
		commits = append(commits, map[string]any{"sha": x, "commit": map[string]any{
			"author": map[string]any{"date": at}, "committer": map[string]any{"date": at}}})
	}
	return response{body: map[string]any{"status": st, "ahead_by": len(ahead), "behind_by": len(behind),
		"total_commits": len(ahead), "commits": commits}}
}

func (s *Server) createStatus(_ *User, m []string, _ url.Values, in map[string]any) response {
	sha := m[1]
	if s.commits[sha] == nil {
		return errorResponse(http.StatusUnprocessableEntity, "No commit found for SHA: "+sha)
	}
	state, _ := in["state"].(string)
	ctx, _ := in["context"].(string)
	if ctx == "" {
		ctx = "default"
	}
	switch state {
	case "success", "failure", "error", "pending":
	default:
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed")
	}
	s.tick()
	s.statuses[sha] = append(s.statuses[sha], status{context: ctx, state: state, at: s.now})
	return response{status: http.StatusCreated, body: map[string]any{"context": ctx, "state": state}}
}

// latestStatuses returns the newest status of each context on sha.
func (s *Server) latestStatuses(sha string) []status {
	byCtx := map[string]status{}
	var order []string
	for _, st := range s.statuses[sha] {
		if _, ok := byCtx[st.context]; !ok {
			order = append(order, st.context)
		}
		byCtx[st.context] = st
	}
	out := make([]status, 0, len(order))
	for _, c := range order {
		out = append(out, byCtx[c])
	}
	return out
}

func (s *Server) combinedStatus(_ *User, m []string, _ url.Values, _ map[string]any) response {
	c := s.resolve(m[1])
	if c == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	list := []any{}
	for _, st := range s.latestStatuses(c.sha) {
		list = append(list, map[string]any{"context": st.context, "state": st.state})
	}
	return response{body: map[string]any{"sha": c.sha, "total_count": len(list)}, list: list, listKey: "statuses"}
}

func (s *Server) listCheckRuns(_ *User, m []string, _ url.Values, _ map[string]any) response {
	c := s.resolve(m[1])
	if c == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	runs := []any{}
	for i := len(s.checkRuns[c.sha]) - 1; i >= 0; i-- { // newest first
		r := s.checkRuns[c.sha][i]
		var concl any
		if r.conclusion != "" {
			concl = r.conclusion
		}
		runs = append(runs, map[string]any{"id": r.id, "name": r.name, "status": r.status, "conclusion": concl, "head_sha": c.sha})
	}
	return response{body: map[string]any{"total_count": len(runs)}, list: runs, listKey: "check_runs"}
}

func (s *Server) branchRules(_ *User, m []string, _ url.Values, _ map[string]any) response {
	name, _ := url.PathUnescape(m[1])
	list := []any{}
	if name == s.defaultBranch && len(s.rulesetChecks) > 0 {
		checks := []any{}
		for _, c := range s.rulesetChecks {
			checks = append(checks, map[string]any{"context": c})
		}
		list = append(list, map[string]any{"type": "required_status_checks", "ruleset_id": 1,
			"parameters": map[string]any{"strict_required_status_checks_policy": s.strict, "required_status_checks": checks}})
	}
	for _, id := range s.rulesetIDs() {
		rs := s.rulesets[id]
		if p, ok := rs.Params["required_status_checks"]; ok && rs.Target == "branch" && rs.Enforcement == "active" && s.refMatches(rs, "refs/heads/"+name) {
			list = append(list, map[string]any{"type": "required_status_checks", "ruleset_id": rs.ID, "parameters": p})
		}
	}
	if name == s.defaultBranch && s.rulePR != nil {
		list = append(list, map[string]any{"type": "pull_request", "ruleset_id": 2, "parameters": s.rulePR})
	}
	return response{list: list}
}

func (s *Server) getClassicChecks(_ *User, m []string, _ url.Values, _ map[string]any) response {
	name, _ := url.PathUnescape(m[1])
	if name != s.defaultBranch || !s.classic {
		return errorResponse(http.StatusNotFound, "Branch not protected")
	}
	ctxs, checks := []any{}, []any{}
	for _, c := range s.classicChecks {
		ctxs = append(ctxs, c)
		checks = append(checks, map[string]any{"context": c, "app_id": nil})
	}
	return response{body: map[string]any{"strict": s.strict, "contexts": ctxs, "checks": checks}}
}

func (s *Server) setRequiredReviews(_ *User, m []string, _ url.Values, in map[string]any) response {
	name, _ := url.PathUnescape(m[1])
	if name != s.defaultBranch {
		return errorResponse(http.StatusNotFound, "Branch not protected")
	}
	n, _ := in["required_approving_review_count"].(float64)
	s.classic = true
	s.approvals = int(n)
	return response{body: map[string]any{"required_approving_review_count": s.approvals}}
}

// requiredChecks are the checks the default branch requires.
func (s *Server) requiredChecks() []string {
	return append(append([]string(nil), s.rulesetChecks...), s.classicChecks...)
}

// checkPasses reports whether a required check passes on sha, as GitHub
// decides for required checks.
func (s *Server) checkPasses(sha, name string) bool {
	runs := s.checkRuns[sha]
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].name == name {
			r := runs[i]
			return r.status == "completed" && (r.conclusion == "success" || r.conclusion == "neutral" || r.conclusion == "skipped")
		}
	}
	for _, st := range s.latestStatuses(sha) {
		if st.context == name {
			return st.state == "success"
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
