package githubtest

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pullState is the pull-request part of an issue.
type pullState struct {
	base      string
	headSHA   string
	mergedBy  *User
	mergedSHA string // the merge commit on the base
	timeline  []*tlItem
	reviews   []*review
	threads   []*thread
}

// tlItem is one PullRequest timeline item the host's GraphQL reads.
type tlItem struct {
	kind      string // PullRequestCommit, PullRequestReview, ReviewDismissedEvent, MergedEvent, ClosedEvent, ReopenedEvent
	at        time.Time
	actor     *User
	commit    string
	review    *review
	prevState string
}

type review struct {
	id     int64
	user   *User
	body   string
	state  string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	commit string
	at     time.Time
}

type rcomment struct {
	id      int64
	user    *User
	body    string
	review  int64
	at      time.Time
	pr      int
	replyTo int64
}

type thread struct {
	id         string // GraphQL node ID
	path       string
	line       int // 0 for a file-level thread
	comments   []*rcomment
	resolved   bool
	resolvedBy *User
}

var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

func (s *Server) findPull(m []string) (*issue, bool) {
	it, ok := s.find(m)
	if !ok || !it.pull || it.pr == nil {
		return nil, false
	}
	return it, true
}

func (s *Server) pullJSON(it *issue) map[string]any {
	m := map[string]any{
		"number": it.number, "node_id": fmt.Sprintf("PR_%d", it.number), "title": it.title, "body": it.body,
		"user": it.user.json(), "state": it.state, "labels": s.labelsJSON(it),
		"created_at": it.createdAt.Format(time.RFC3339), "updated_at": it.updatedAt.Format(time.RFC3339),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.Owner, s.Name, it.number),
		"merged":   it.mergedAt != nil, "merged_at": nil, "merged_by": nil,
	}
	repo := map[string]any{"full_name": s.Owner + "/" + s.Name}
	base := "main"
	head := ""
	if it.pr != nil {
		base, head = it.pr.base, it.pr.headSHA
		if it.pr.mergedBy != nil {
			m["merged_by"] = it.pr.mergedBy.json()
		}
	}
	m["head"] = map[string]any{"ref": it.head, "sha": head, "repo": repo}
	m["base"] = map[string]any{"ref": base, "sha": s.branches[base], "repo": repo}
	if it.mergedAt != nil {
		m["merged_at"] = it.mergedAt.Format(time.RFC3339)
	}
	return m
}

func (s *Server) createPull(u *User, _ []string, _ url.Values, in map[string]any) response {
	title, _ := in["title"].(string)
	head, _ := in["head"].(string)
	base, _ := in["base"].(string)
	body, _ := in["body"].(string)
	head = strings.TrimPrefix(head, s.Owner+":")
	if strings.TrimSpace(title) == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: title is missing")
	}
	hsha, ok := s.branches[head]
	if !ok || s.branches[base] == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: head or base invalid")
	}
	if head == base {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: head and base are the same")
	}
	for _, it := range s.issues {
		if it.pull && it.head == head && it.state == "open" {
			return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: A pull request already exists for "+s.Owner+":"+head)
		}
	}
	if len(s.newCommits(hsha, s.ancestors(s.branches[base]))) == 0 {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: No commits between "+base+" and "+head)
	}
	s.tick()
	pr := &issue{id: s.id(), number: s.next, title: title, body: body, user: u, state: "open",
		createdAt: s.now, updatedAt: s.now, pull: true, head: head, pr: &pullState{base: base, headSHA: hsha}}
	for _, mm := range closingRef.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(mm[1])
		if it := s.issues[n]; it != nil && !it.pull {
			pr.closes = append(pr.closes, n)
		}
	}
	// GitHub lists the commits already on the branch in the timeline too.
	for _, x := range s.newCommits(hsha, s.ancestors(s.branches[base])) {
		pr.pr.timeline = append(pr.pr.timeline, &tlItem{kind: "PullRequestCommit", at: s.commits[x].at, commit: x})
	}
	s.next++
	s.issues[pr.number] = pr
	return response{status: http.StatusCreated, body: s.pullJSON(pr)}
}

func (s *Server) getPull(_ *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	return response{body: s.pullJSON(it)}
}

func (s *Server) patchPull(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	st, _ := in["state"].(string)
	switch {
	case st == "closed" && it.state == "open":
		s.tick()
		it.state, it.updatedAt = "closed", s.now
		it.pr.timeline = append(it.pr.timeline, &tlItem{kind: "ClosedEvent", at: s.now, actor: u})
		s.event(it, "closed", u, "", nil, "")
	case st == "open" && it.state == "closed" && it.mergedAt == nil:
		s.tick()
		it.state, it.updatedAt = "open", s.now
		it.pr.timeline = append(it.pr.timeline, &tlItem{kind: "ReopenedEvent", at: s.now, actor: u})
		s.event(it, "reopened", u, "", nil, "")
		if tip := s.branches[it.head]; tip != it.pr.headSHA {
			seen := s.ancestors(it.pr.headSHA)
			it.pr.headSHA = tip
			for _, x := range s.newCommits(tip, seen) {
				it.pr.timeline = append(it.pr.timeline, &tlItem{kind: "PullRequestCommit", at: s.commits[x].at, commit: x})
			}
		}
	case st != "":
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: state")
	}
	return response{body: s.pullJSON(it)}
}

// approvals counts the approvals GitHub would count for a merge: the latest
// approving or changes-requested review of each user other than the author.
func (s *Server) approvalCount(it *issue) int {
	latest := map[int64]string{}
	for _, r := range it.pr.reviews {
		if r.user.ID == it.user.ID {
			continue
		}
		switch r.state {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest[r.user.ID] = r.state
		}
	}
	n := 0
	for _, st := range latest {
		if st == "APPROVED" {
			n++
		}
	}
	return n
}

// mergePull merges a pull request (PUT /pulls/{n}/merge) with GitHub's
// refusals: 405 when it isn't mergeable, 409 when sha isn't the head.
func (s *Server) mergePull(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	if it.state != "open" {
		return errorResponse(http.StatusMethodNotAllowed, "Pull Request is not mergeable")
	}
	if sha, _ := in["sha"].(string); sha != "" && sha != it.pr.headSHA {
		return errorResponse(http.StatusConflict, "Head branch was modified. Review and try the merge again.")
	}
	head, base := it.pr.headSHA, it.pr.base
	if it.pr.base == s.defaultBranch {
		for _, name := range s.requiredChecks() {
			if !s.checkPasses(head, name) {
				return errorResponse(http.StatusMethodNotAllowed, "Required status check \""+name+"\" is expected.")
			}
		}
		if s.approvalCount(it) < s.approvals {
			return errorResponse(http.StatusMethodNotAllowed, "At least "+itoa(s.approvals)+" approving review is required by reviewers with write access.")
		}
		if s.strict && !s.ancestors(head)[s.branches[base]] {
			return errorResponse(http.StatusMethodNotAllowed, "Head branch is out of date.")
		}
	}
	method, _ := in["merge_method"].(string)
	s.tick()
	tip := s.branches[base]
	files := s.merge3(tip, head)
	parents := []string{tip}
	if method == "merge" {
		parents = append(parents, head)
	}
	c := s.newCommit(parents, files, u)
	s.branches[base] = c.sha
	t := s.now
	it.mergedAt, it.state, it.updatedAt = &t, "closed", t
	it.pr.mergedBy, it.pr.mergedSHA = u, c.sha
	it.pr.timeline = append(it.pr.timeline,
		&tlItem{kind: "MergedEvent", at: t, actor: u, commit: c.sha},
		&tlItem{kind: "ClosedEvent", at: t, actor: u})
	s.event(it, "merged", u, "", nil, "")
	s.event(it, "closed", u, "", nil, "")
	return response{body: map[string]any{"merged": true, "sha": c.sha, "message": "Pull Request successfully merged"}}
}

// diff returns the files a pull request changes, with the lines of each
// that are new, from its merge base with the base branch.
func (s *Server) diff(it *issue) map[string][2][]string {
	head := it.pr.headSHA
	mb := s.commits[s.mergeBase(s.branches[it.pr.base], head)]
	out := map[string][2][]string{}
	hc := s.commits[head]
	if hc == nil {
		return out
	}
	for p, nv := range hc.files {
		ov, ok := "", false
		if mb != nil {
			ov, ok = mb.files[p]
		}
		if ok && ov == nv {
			continue
		}
		out[p] = [2][]string{lines(ov), lines(nv)}
	}
	return out
}

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (s *Server) pullFiles(_ *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	d := s.diff(it)
	paths := make([]string, 0, len(d))
	for p := range d {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	list := []any{}
	for _, p := range paths {
		old, nw := d[p][0], d[p][1]
		var b strings.Builder
		if len(old) == 0 {
			fmt.Fprintf(&b, "@@ -0,0 +1,%d @@", len(nw))
		} else {
			fmt.Fprintf(&b, "@@ -1,%d +1,%d @@", len(old), len(nw))
		}
		for _, l := range old {
			b.WriteString("\n-" + l)
		}
		for _, l := range nw {
			b.WriteString("\n+" + l)
		}
		st := "modified"
		if len(old) == 0 {
			st = "added"
		}
		list = append(list, map[string]any{"filename": p, "status": st, "patch": b.String()})
	}
	return response{list: list}
}

// inDiff reports whether a review comment may go on path and line (0: the
// file).
func (s *Server) inDiff(it *issue, path string, line int) bool {
	d, ok := s.diff(it)[path]
	if !ok {
		return false
	}
	return line == 0 || (line >= 1 && line <= len(d[1]))
}

func (s *Server) reviewJSON(it *issue, r *review) map[string]any {
	return map[string]any{"id": r.id, "node_id": fmt.Sprintf("PRR_%d", r.id), "user": r.user.json(), "body": r.body,
		"state": r.state, "commit_id": r.commit, "submitted_at": r.at.Format(time.RFC3339),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", s.Owner, s.Name, it.number, r.id)}
}

func (s *Server) listReviews(_ *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	list := []any{}
	for _, r := range it.pr.reviews {
		list = append(list, s.reviewJSON(it, r))
	}
	return response{list: list}
}

// addReview records a submitted review and its timeline item.
func (s *Server) addReview(it *issue, u *User, state, body, commit string) *review {
	if commit == "" {
		commit = it.pr.headSHA
	}
	r := &review{id: s.id(), user: u, body: body, state: state, commit: commit, at: s.now}
	it.pr.reviews = append(it.pr.reviews, r)
	it.pr.timeline = append(it.pr.timeline, &tlItem{kind: "PullRequestReview", at: s.now, actor: u, review: r})
	it.updatedAt = s.now
	return r
}

func (s *Server) newThread(it *issue, r *review, u *User, path string, line int, body string) *thread {
	s.threadNo++
	t := &thread{id: fmt.Sprintf("PRRT_%d_%d", it.number, s.threadNo), path: path, line: line}
	t.comments = append(t.comments, &rcomment{id: s.id(), user: u, body: body, review: r.id, at: s.now, pr: it.number})
	it.pr.threads = append(it.pr.threads, t)
	return t
}

// commentPlace reads a review comment's path, line and subject type.
func commentPlace(c map[string]any) (path string, line int, ok bool) {
	path, _ = c["path"].(string)
	if st, _ := c["subject_type"].(string); st == "file" {
		return path, 0, path != ""
	}
	l, _ := c["line"].(float64)
	return path, int(l), path != "" && l >= 1
}

func (s *Server) createReview(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	body, _ := in["body"].(string)
	commit, _ := in["commit_id"].(string)
	if commit != "" && !s.ancestors(it.pr.headSHA)[commit] {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: commit_id is not part of the pull request")
	}
	state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[fmt.Sprint(in["event"])]
	if state == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: event")
	}
	if state != "COMMENTED" && u.ID == it.user.ID {
		return errorResponse(http.StatusUnprocessableEntity, "Can not approve or request changes on your own pull request")
	}
	cs, _ := in["comments"].([]any)
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		path, line, ok := commentPlace(cm)
		if !ok || !s.inDiff(it, path, line) {
			return errorResponse(http.StatusUnprocessableEntity, "Unprocessable Entity: Line could not be resolved")
		}
	}
	s.tick()
	r := s.addReview(it, u, state, body, commit)
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		path, line, _ := commentPlace(cm)
		b, _ := cm["body"].(string)
		s.newThread(it, r, u, path, line, b)
	}
	return response{body: s.reviewJSON(it, r)}
}

func (s *Server) dismissReview(u *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	for _, r := range it.pr.reviews {
		if r.id != id {
			continue
		}
		if r.state != "APPROVED" && r.state != "CHANGES_REQUESTED" {
			return errorResponse(http.StatusUnprocessableEntity, "Can not dismiss a "+strings.ToLower(r.state)+" pull request review")
		}
		s.tick()
		prev := r.state
		r.state = "DISMISSED"
		it.updatedAt = s.now
		it.pr.timeline = append(it.pr.timeline, &tlItem{kind: "ReviewDismissedEvent", at: s.now, actor: u, review: r, prevState: prev})
		return response{body: s.reviewJSON(it, r)}
	}
	return errorResponse(http.StatusNotFound, "Not Found")
}

func (s *Server) rcommentJSON(c *rcomment, t *thread) map[string]any {
	m := map[string]any{"id": c.id, "body": c.body, "user": c.user.json(), "path": t.path,
		"pull_request_review_id": c.review, "created_at": c.at.Format(time.RFC3339),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", s.Owner, s.Name, c.pr, c.id)}
	if t.line > 0 {
		m["line"], m["subject_type"] = t.line, "line"
	} else {
		m["line"], m["subject_type"] = nil, "file"
	}
	if c.replyTo != 0 {
		m["in_reply_to_id"] = c.replyTo
	}
	return m
}

// createReviewComment starts a thread (POST /pulls/{n}/comments). GitHub
// wraps the comment in a COMMENTED review with an empty body.
func (s *Server) createReviewComment(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	path, line, ok := commentPlace(in)
	if !ok || !s.inDiff(it, path, line) {
		return errorResponse(http.StatusUnprocessableEntity, "Unprocessable Entity: Line could not be resolved")
	}
	body, _ := in["body"].(string)
	if body == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: body")
	}
	s.tick()
	r := s.addReview(it, u, "COMMENTED", "", "")
	t := s.newThread(it, r, u, path, line, body)
	return response{status: http.StatusCreated, body: s.rcommentJSON(t.comments[0], t)}
}

func (s *Server) replyReviewComment(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.findPull(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	body, _ := in["body"].(string)
	if body == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: body")
	}
	for _, t := range it.pr.threads {
		if t.comments[0].id != id {
			continue
		}
		s.tick()
		r := s.addReview(it, u, "COMMENTED", "", "")
		c := &rcomment{id: s.id(), user: u, body: body, review: r.id, at: s.now, pr: it.number, replyTo: id}
		t.comments = append(t.comments, c)
		return response{status: http.StatusCreated, body: s.rcommentJSON(c, t)}
	}
	return errorResponse(http.StatusNotFound, "Not Found")
}

// Reviews returns the bodies of a pull request's reviews, for tests.
func (s *Server) Reviews(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if it := s.issues[n]; it != nil && it.pr != nil {
		for _, r := range it.pr.reviews {
			out = append(out, r.body)
		}
	}
	return out
}

// ReviewComments returns the bodies of the first comment of each review
// thread, for tests.
func (s *Server) ReviewComments(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if it := s.issues[n]; it != nil && it.pr != nil {
		for _, t := range it.pr.threads {
			out = append(out, t.comments[0].body)
		}
	}
	return out
}

// ResolvedBy returns the login that resolved a thread, or "".
func (s *Server) ResolvedBy(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.threadByID(id); t != nil && t.resolved && t.resolvedBy != nil {
		return t.resolvedBy.Login
	}
	return ""
}
