// Package githubtest is a stateful stub of the GitHub API subset the github
// adapter calls, for tests that must not reach GitHub (work plan #14 D14).
// It answers REST and GraphQL calls as GitHub documents them, with ETags,
// paging, rate-limit headers and injected failures. Driver acts in it, or in
// a live sandbox repository, as a given account.
package githubtest

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User is an account in the stub. Type is "User" or "Bot".
type User struct {
	ID    int64
	Login string
	Type  string
	Token string
}

func (u *User) json() map[string]any {
	return map[string]any{"id": u.ID, "login": u.Login, "type": u.Type}
}

type issue struct {
	id          int64
	number      int
	title, body string
	user        *User
	labels      []string
	state       string
	stateReason string
	closedBy    *User
	assignees   []*User
	createdAt   time.Time
	updatedAt   time.Time
	blockedBy   []int

	// pull requests
	pull     bool
	head     string
	mergedAt *time.Time
	closes   []int // closing issue references
	pr       *pullState
}

type comment struct {
	id        int64
	issue     int
	body      string
	user      *User
	createdAt time.Time
	updatedAt time.Time
}

type event struct {
	id          int64
	issue       int
	kind        string
	actor       *User
	at          time.Time
	label       string
	assignee    *User
	stateReason string
}

type failure struct {
	method string
	path   *regexp.Regexp
	status int
	header http.Header
	body   string
	times  int
}

// Request is one call the stub answered.
type Request struct {
	Method, Path, Query string
	Status              int
	Login               string
}

// Server is the stub. Its exported methods change its state directly, as
// GitHub itself would (for setup that has no API in the Driver).
type Server struct {
	*httptest.Server
	Owner, Name string

	// RateLimit is the calls each token may make before the stub answers
	// 403 with x-ratelimit-remaining 0. Zero means unlimited. 304s don't
	// count, as on GitHub.
	RateLimit int

	mu       sync.Mutex
	now      time.Time
	nextID   int64
	users    map[string]*User // by token
	labels   map[string]string
	issues   map[int]*issue
	next     int
	comments []*comment
	events   []*event
	failures []*failure
	used     map[string]int
	requests []Request
	closers  map[int]int // issue to the pull request that closed it
	hooks    []hook

	git        // commits, branches, statuses and branch rules (git.go)
	trustState // rulesets, permissions, forks and tags (trust.go)
	appState   // GitHub Apps and their installation tokens (apps.go)
	setupState // the manifest flow and the owner type (setup.go)
	threadNo   int
}

// New starts a stub for repository owner/name. Close it when done.
func New(owner, name string) *Server {
	s := &Server{
		Owner:   owner,
		Name:    name,
		now:     time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		nextID:  100000,
		users:   map[string]*User{},
		labels:  map[string]string{},
		issues:  map[int]*issue{},
		next:    1,
		used:    map[string]int{},
		closers: map[int]int{},
	}
	s.initGit()
	s.initTrust()
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// AddUser adds an account and returns its token.
func (s *Server) AddUser(id int64, login, typ string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &User{ID: id, Login: login, Type: typ, Token: "tok-" + login}
	s.users[u.Token] = u
	return u
}

// CreateLabel adds a repository label.
func (s *Server) CreateLabel(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		s.labels[strings.ToLower(n)] = n
	}
}

// Fail makes the next times calls matching method and the path pattern
// answer status with header and body. Zero times means always.
func (s *Server) Fail(method, pathPattern string, status int, header http.Header, body string, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, &failure{method: method, path: regexp.MustCompile(pathPattern), status: status, header: header, body: body, times: times})
}

// ClearFailures removes every injected failure.
func (s *Server) ClearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = nil
}

// Requests returns the calls answered so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Advance moves the stub's clock.
func (s *Server) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

// Now is the stub's clock.
func (s *Server) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// AddPull opens a pull request by author from head, with body. A non-empty
// closes lists its closing issue references.
func (s *Server) AddPull(author *User, head, body string, closes ...int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick()
	pr := &issue{id: s.id(), number: s.next, title: "pull " + head, body: body, user: author, state: "open",
		createdAt: s.now, updatedAt: s.now, pull: true, head: head, closes: closes, pr: &pullState{base: "main"}}
	s.next++
	s.issues[pr.number] = pr
	return pr.number
}

// MergePull merges a pull request. Like GitHub, it closes the issues it
// references as completed, with the pull request as their closer, when
// closeIssues is set.
func (s *Server) MergePull(by *User, number int, closeIssues bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick()
	pr := s.issues[number]
	t := s.now
	pr.mergedAt, pr.state, pr.updatedAt = &t, "closed", t
	s.event(pr, "closed", by, "", nil, "")
	if closeIssues {
		for _, n := range pr.closes {
			it := s.issues[n]
			if it.state == "open" {
				s.close(it, by, "completed")
				s.closers[n] = number
			}
		}
	}
}

// IssueBody returns an issue's body, for tests.
func (s *Server) IssueBody(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it := s.issues[n]; it != nil {
		return it.body
	}
	return ""
}

// Comments returns the bodies of an issue's comments, for tests.
func (s *Server) Comments(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.comments {
		if c.issue == n {
			out = append(out, c.body)
		}
	}
	return out
}

func (s *Server) tick() { s.now = s.now.Add(time.Second) }

func (s *Server) id() int64 { s.nextID++; return s.nextID }

func (s *Server) event(it *issue, kind string, actor *User, label string, assignee *User, reason string) {
	s.events = append(s.events, &event{id: s.id(), issue: it.number, kind: kind, actor: actor, at: s.now, label: label, assignee: assignee, stateReason: reason})
}

func (s *Server) close(it *issue, by *User, reason string) {
	it.state, it.stateReason, it.closedBy, it.updatedAt = "closed", reason, by, s.now
	s.event(it, "closed", by, "", nil, reason)
}

func (s *Server) repo() string { return "/repos/" + s.Owner + "/" + s.Name }

func (s *Server) issueJSON(it *issue) map[string]any {
	labels := []any{}
	for _, l := range it.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	assignees := []any{}
	for _, u := range it.assignees {
		assignees = append(assignees, u.json())
	}
	kind := "issues"
	if it.pull {
		kind = "pull"
	}
	m := map[string]any{
		"id": it.id, "number": it.number, "title": it.title, "body": it.body, "user": it.user.json(),
		"labels": labels, "state": it.state, "assignees": assignees,
		"created_at": it.createdAt.Format(time.RFC3339), "updated_at": it.updatedAt.Format(time.RFC3339),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/%s/%d", s.Owner, s.Name, kind, it.number),
	}
	if it.stateReason != "" {
		m["state_reason"] = it.stateReason
	} else {
		m["state_reason"] = nil
	}
	if it.closedBy != nil {
		m["closed_by"] = it.closedBy.json()
	} else {
		m["closed_by"] = nil
	}
	if it.pull {
		m["pull_request"] = map[string]any{"url": s.URL + s.repo() + "/pulls/" + strconv.Itoa(it.number)}
	}
	return m
}

func (s *Server) commentJSON(c *comment) map[string]any {
	kind := "issues"
	if it := s.issues[c.issue]; it != nil && it.pull {
		kind = "pull"
	}
	return map[string]any{
		"id": c.id, "body": c.body, "user": c.user.json(),
		"created_at": c.createdAt.Format(time.RFC3339), "updated_at": c.updatedAt.Format(time.RFC3339),
		"issue_url": s.URL + s.repo() + "/issues/" + strconv.Itoa(c.issue),
		"html_url":  fmt.Sprintf("https://github.com/%s/%s/%s/%d#issuecomment-%d", s.Owner, s.Name, kind, c.issue, c.id),
	}
}

func (s *Server) eventJSON(e *event) map[string]any {
	m := map[string]any{"id": e.id, "event": e.kind, "created_at": e.at.Format(time.RFC3339), "issue": s.issueJSON(s.issues[e.issue])}
	if e.actor != nil {
		m["actor"] = e.actor.json()
	}
	if e.label != "" {
		m["label"] = map[string]any{"name": e.label}
	}
	if e.assignee != nil {
		m["assignee"] = e.assignee.json()
	}
	if e.kind == "closed" {
		if e.stateReason != "" {
			m["state_reason"] = e.stateReason
		} else {
			m["state_reason"] = nil
		}
	}
	return m
}

// response is what a handler returns.
type response struct {
	status int
	body   any
	list   []any // a paged list, when set
	// listKey, when set, pages list under this key of body (a map), as
	// GitHub does for check runs and the combined status.
	listKey string
}

func errorResponse(status int, msg string) response {
	return response{status: status, body: map[string]any{"message": msg}}
}

var routes = []struct {
	method string
	path   *regexp.Regexp
	fn     func(s *Server, u *User, m []string, q url.Values, in map[string]any) response
}{
	{"GET", regexp.MustCompile(`^$`), (*Server).getRepo},
	{"GET", regexp.MustCompile(`^/issues/(\d+)$`), (*Server).getIssue},
	{"PATCH", regexp.MustCompile(`^/issues/(\d+)$`), (*Server).patchIssue},
	{"GET", regexp.MustCompile(`^/issues$`), (*Server).listIssues},
	{"POST", regexp.MustCompile(`^/issues$`), (*Server).createIssue},
	{"POST", regexp.MustCompile(`^/issues/(\d+)/labels$`), (*Server).addLabels},
	{"DELETE", regexp.MustCompile(`^/issues/(\d+)/labels/(.+)$`), (*Server).removeLabel},
	{"POST", regexp.MustCompile(`^/issues/(\d+)/assignees$`), (*Server).addAssignees},
	{"DELETE", regexp.MustCompile(`^/issues/(\d+)/assignees$`), (*Server).removeAssignees},
	{"GET", regexp.MustCompile(`^/issues/(\d+)/comments$`), (*Server).listComments},
	{"POST", regexp.MustCompile(`^/issues/(\d+)/comments$`), (*Server).createComment},
	{"GET", regexp.MustCompile(`^/issues/comments$`), (*Server).listRepoComments},
	{"GET", regexp.MustCompile(`^/issues/events$`), (*Server).listEvents},
	{"GET", regexp.MustCompile(`^/issues/(\d+)/dependencies/blocked_by$`), (*Server).listBlockedBy},
	{"POST", regexp.MustCompile(`^/issues/(\d+)/dependencies/blocked_by$`), (*Server).addBlockedBy},
	{"GET", regexp.MustCompile(`^/labels$`), (*Server).listLabels},
	{"POST", regexp.MustCompile(`^/labels$`), (*Server).createLabel},
	{"GET", regexp.MustCompile(`^/pulls$`), (*Server).listPulls},
	{"POST", regexp.MustCompile(`^/pulls$`), (*Server).createPull},
	{"GET", regexp.MustCompile(`^/pulls/(\d+)$`), (*Server).getPull},
	{"PATCH", regexp.MustCompile(`^/pulls/(\d+)$`), (*Server).patchPull},
	{"PUT", regexp.MustCompile(`^/pulls/(\d+)/merge$`), (*Server).mergePull},
	{"GET", regexp.MustCompile(`^/pulls/(\d+)/files$`), (*Server).pullFiles},
	{"GET", regexp.MustCompile(`^/pulls/(\d+)/reviews$`), (*Server).listReviews},
	{"POST", regexp.MustCompile(`^/pulls/(\d+)/reviews$`), (*Server).createReview},
	{"PUT", regexp.MustCompile(`^/pulls/(\d+)/reviews/(\d+)/dismissals$`), (*Server).dismissReview},
	{"POST", regexp.MustCompile(`^/pulls/(\d+)/comments$`), (*Server).createReviewComment},
	{"POST", regexp.MustCompile(`^/pulls/(\d+)/comments/(\d+)/replies$`), (*Server).replyReviewComment},
	{"GET", regexp.MustCompile(`^/branches/([^/]+(?:/[^/]+)*)/protection/required_status_checks$`), (*Server).getClassicChecks},
	{"GET", regexp.MustCompile(`^/branches/([^/]+(?:/[^/]+)*)/protection$`), (*Server).getProtection},
	{"PATCH", regexp.MustCompile(`^/branches/([^/]+(?:/[^/]+)*)/protection/required_status_checks$`), (*Server).setClassicChecks},
	{"PATCH", regexp.MustCompile(`^/branches/([^/]+(?:/[^/]+)*)/protection/required_pull_request_reviews$`), (*Server).setRequiredReviews},
	{"GET", regexp.MustCompile(`^/branches/(.+)$`), (*Server).getBranch},
	{"POST", regexp.MustCompile(`^/git/refs$`), (*Server).createRef},
	{"GET", regexp.MustCompile(`^/contents/(.+)$`), (*Server).getContents},
	{"PUT", regexp.MustCompile(`^/contents/(.+)$`), (*Server).putContents},
	{"POST", regexp.MustCompile(`^/merges$`), (*Server).mergeBranches},
	{"GET", regexp.MustCompile(`^/compare/(.+)\.\.\.(.+)$`), (*Server).compare},
	{"POST", regexp.MustCompile(`^/statuses/([0-9a-f]+)$`), (*Server).createStatus},
	{"GET", regexp.MustCompile(`^/commits/([^/]+)/status$`), (*Server).combinedStatus},
	{"GET", regexp.MustCompile(`^/commits/([^/]+)/check-runs$`), (*Server).listCheckRuns},
	{"GET", regexp.MustCompile(`^/rules/branches/(.+)$`), (*Server).branchRules},
	{"GET", regexp.MustCompile(`^/rulesets$`), (*Server).listRulesets},
	{"POST", regexp.MustCompile(`^/rulesets$`), (*Server).createRuleset},
	{"GET", regexp.MustCompile(`^/rulesets/(\d+)$`), (*Server).getRuleset},
	{"PUT", regexp.MustCompile(`^/rulesets/(\d+)$`), (*Server).updateRuleset},
	{"DELETE", regexp.MustCompile(`^/rulesets/(\d+)$`), (*Server).deleteRuleset},
	{"GET", regexp.MustCompile(`^/collaborators$`), (*Server).listCollaborators},
	{"PUT", regexp.MustCompile(`^/collaborators/([^/]+)$`), (*Server).putCollaborator},
	{"DELETE", regexp.MustCompile(`^/collaborators/([^/]+)$`), (*Server).deleteCollaborator},
	{"POST", regexp.MustCompile(`^/forks$`), (*Server).createFork},
}

// getUserByID answers GET /user/{account_id}.
func (s *Server) getUserByID(id string) response {
	for _, u := range s.users {
		if strconv.FormatInt(u.ID, 10) == id {
			return response{body: u.json()}
		}
	}
	return errorResponse(http.StatusNotFound, "Not Found")
}

// getRepo answers GET /repos/{o}/{r} with the fields the adapter reads.
// It also carries the caller's permissions and the owner, as GitHub's does.
func (s *Server) getRepo(u *User, _ []string, _ url.Values, _ map[string]any) response {
	p := s.perm(u)
	ot := s.ownerType
	if ot == "" {
		ot = "User"
	}
	return response{body: map[string]any{"name": s.Name, "full_name": s.Owner + "/" + s.Name, "default_branch": s.defaultBranch,
		"html_url":    "https://github.com/" + s.Owner + "/" + s.Name,
		"owner":       map[string]any{"login": s.Owner, "type": ot},
		"permissions": map[string]any{"admin": p == "admin", "push": p != "read", "pull": true}}}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, login := s.handle(w, r)
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Status: status, Login: login})
	var run []func()
	for i := 0; i < len(s.hooks); i++ {
		h := s.hooks[i]
		if h.method == r.Method && h.path.MatchString(r.URL.Path) {
			run = append(run, h.fn)
			s.hooks = append(s.hooks[:i], s.hooks[i+1:]...)
			i--
		}
	}
	s.mu.Unlock()
	for _, fn := range run {
		fn()
	}
}

type hook struct {
	method string
	path   *regexp.Regexp
	fn     func()
}

// After runs fn once, after the next call matching method and the path
// pattern has been answered, for example to make a human change in the
// middle of an adapter's sequence of calls.
func (s *Server) After(method, pathPattern string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, hook{method: method, path: regexp.MustCompile(pathPattern), fn: fn})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) (int, string) {
	for i, f := range s.failures {
		if f.method == r.Method && f.path.MatchString(r.URL.Path) {
			if f.times > 0 {
				if f.times--; f.times == 0 {
					s.failures = append(s.failures[:i], s.failures[i+1:]...)
				}
			}
			for k, v := range f.header {
				w.Header()[k] = v
			}
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return f.status, ""
		}
	}
	if status, login, ok := s.handleApps(w, r); ok {
		return status, login
	}
	u := s.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if u == nil {
		return s.write(w, r, "", errorResponse(http.StatusUnauthorized, "Bad credentials")), ""
	}
	var in map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	if r.Method == "POST" && r.URL.Path == "/graphql" {
		return s.write(w, r, u.Token, s.graphql(u, in)), u.Login
	}
	if r.URL.Path == "/user" && r.Method == "GET" {
		return s.write(w, r, u.Token, response{body: u.json()}), u.Login
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/user/"); ok && r.Method == "GET" {
		return s.write(w, r, u.Token, s.getUserByID(id)), u.Login
	}
	q := r.URL.Query()
	s.forkLogin = ""
	rest, ok := strings.CutPrefix(r.URL.Path, s.repo())
	if !ok {
		if in == nil {
			in = map[string]any{}
		}
		rest, ok = s.forkRequest(r.URL.Path, q, in)
	}
	if !ok {
		return s.write(w, r, u.Token, errorResponse(http.StatusNotFound, "Not Found")), u.Login
	}
	for _, rt := range routes {
		if rt.method != r.Method {
			continue
		}
		if m := rt.path.FindStringSubmatch(rest); m != nil {
			return s.write(w, r, u.Token, rt.fn(s, u, m, q, in)), u.Login
		}
	}
	return s.write(w, r, u.Token, errorResponse(http.StatusNotFound, "Not Found")), u.Login
}

// write sends res with paging, ETag and rate-limit headers.
func (s *Server) write(w http.ResponseWriter, r *http.Request, token string, res response) int {
	body := res.body
	if res.list != nil {
		q := r.URL.Query()
		per, _ := strconv.Atoi(q.Get("per_page"))
		if per <= 0 {
			per = 30
		}
		per = min(per, 100)
		page, _ := strconv.Atoi(q.Get("page"))
		page = max(page, 1)
		lo := min((page-1)*per, len(res.list))
		hi := min(lo+per, len(res.list))
		if res.listKey != "" {
			wrapped := map[string]any{}
			for k, v := range res.body.(map[string]any) {
				wrapped[k] = v
			}
			wrapped[res.listKey] = res.list[lo:hi]
			body = wrapped
		} else {
			body = res.list[lo:hi]
		}
		if hi < len(res.list) {
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, s.URL, r.URL.Path, q.Encode()))
		}
	}
	if body == nil && res.status == 0 {
		res.status = http.StatusNoContent
	}
	if res.status == 0 {
		res.status = http.StatusOK
	}
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	if r.Method == "GET" && res.status == http.StatusOK {
		h := sha1.Sum(append(data, w.Header().Get("Link")...))
		etag := `"` + hex.EncodeToString(h[:]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			s.rateHeaders(w, token, false)
			w.WriteHeader(http.StatusNotModified)
			return http.StatusNotModified
		}
	}
	if s.RateLimit > 0 && token != "" {
		if s.used[token] >= s.RateLimit {
			s.rateHeaders(w, token, false)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return http.StatusForbidden
		}
		s.rateHeaders(w, token, true)
	}
	if data != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(res.status)
	_, _ = w.Write(data)
	return res.status
}

func (s *Server) rateHeaders(w http.ResponseWriter, token string, count bool) {
	if s.RateLimit <= 0 || token == "" {
		return
	}
	if count {
		s.used[token]++
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(s.RateLimit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(max(s.RateLimit-s.used[token], 0)))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(s.now.Add(time.Hour).Unix(), 10))
	w.Header().Set("X-RateLimit-Resource", "core")
}

func (s *Server) find(m []string) (*issue, bool) {
	n, _ := strconv.Atoi(m[1])
	it, ok := s.issues[n]
	return it, ok
}

func (s *Server) getIssue(_ *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	return response{body: s.issueJSON(it)}
}

func (s *Server) patchIssue(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	s.tick()
	if v, ok := in["title"].(string); ok {
		it.title, it.updatedAt = v, s.now
	}
	if v, ok := in["body"].(string); ok {
		it.body, it.updatedAt = v, s.now
	}
	if st, ok := in["state"].(string); ok && st != it.state {
		switch st {
		case "closed":
			reason, _ := in["state_reason"].(string)
			if reason == "" {
				reason = "completed"
			}
			s.close(it, u, reason)
		case "open":
			it.state, it.stateReason, it.closedBy, it.updatedAt = "open", "reopened", nil, s.now
			s.event(it, "reopened", u, "", nil, "")
		default:
			return errorResponse(http.StatusUnprocessableEntity, "Validation Failed")
		}
	}
	return response{body: s.issueJSON(it)}
}

func (s *Server) listIssues(_ *User, _ []string, q url.Values, _ map[string]any) response {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return errorResponse(http.StatusUnprocessableEntity, "Validation Failed")
		}
		since = t
	}
	var want []string
	if v := q.Get("labels"); v != "" {
		want = strings.Split(v, ",")
	}
	var its []*issue
	for _, it := range s.issues {
		if (state != "all" && it.state != state) || (q.Get("creator") != "" && !strings.EqualFold(q.Get("creator"), it.user.Login)) ||
			it.updatedAt.Before(since) || !hasAll(it.labels, want) {
			continue
		}
		its = append(its, it)
	}
	sort.Slice(its, func(i, j int) bool {
		a, b := its[i], its[j]
		if q.Get("sort") == "updated" {
			a, b = &issue{number: a.number, createdAt: a.updatedAt}, &issue{number: b.number, createdAt: b.updatedAt}
		}
		less := a.createdAt.Before(b.createdAt) || (a.createdAt.Equal(b.createdAt) && a.number < b.number)
		if q.Get("direction") == "asc" {
			return less
		}
		return !less
	})
	list := []any{}
	for _, it := range its {
		list = append(list, s.issueJSON(it))
	}
	return response{list: list}
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		ok := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (s *Server) createIssue(u *User, _ []string, _ url.Values, in map[string]any) response {
	title, _ := in["title"].(string)
	if strings.TrimSpace(title) == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: title is missing")
	}
	body, _ := in["body"].(string)
	s.tick()
	it := &issue{id: s.id(), number: s.next, title: title, body: body, user: u, state: "open", createdAt: s.now, updatedAt: s.now}
	s.next++
	s.issues[it.number] = it
	if ls, ok := in["labels"].([]any); ok {
		for _, l := range ls {
			if name, ok := l.(string); ok {
				s.addLabel(it, u, name)
			}
		}
	}
	return response{status: http.StatusCreated, body: s.issueJSON(it)}
}

// addLabel adds a label, creating it in the repository if needed. Adding a
// label the issue has makes no event (work plan #14 D14).
func (s *Server) addLabel(it *issue, u *User, name string) {
	for _, l := range it.labels {
		if strings.EqualFold(l, name) {
			return
		}
	}
	if canon, ok := s.labels[strings.ToLower(name)]; ok {
		name = canon
	} else {
		s.labels[strings.ToLower(name)] = name
	}
	it.labels = append(it.labels, name)
	it.updatedAt = s.now
	s.event(it, "labeled", u, name, nil, "")
}

func (s *Server) labelsJSON(it *issue) []any {
	out := []any{}
	for _, l := range it.labels {
		out = append(out, map[string]any{"name": l})
	}
	return out
}

func (s *Server) addLabels(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	ls, _ := in["labels"].([]any)
	s.tick()
	for _, l := range ls {
		if name, ok := l.(string); ok {
			s.addLabel(it, u, name)
		}
	}
	return response{body: s.labelsJSON(it)}
}

func (s *Server) removeLabel(u *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	name, _ := url.PathUnescape(m[2])
	for i, l := range it.labels {
		if strings.EqualFold(l, name) {
			s.tick()
			it.labels = append(it.labels[:i], it.labels[i+1:]...)
			it.updatedAt = s.now
			s.event(it, "unlabeled", u, l, nil, "")
			return response{body: s.labelsJSON(it)}
		}
	}
	return errorResponse(http.StatusNotFound, "Label does not exist")
}

func (s *Server) userByLogin(login string) *User {
	for _, u := range s.users {
		if strings.EqualFold(u.Login, login) {
			return u
		}
	}
	return nil
}

func (s *Server) addAssignees(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	ls, _ := in["assignees"].([]any)
	s.tick()
	for _, l := range ls {
		login, _ := l.(string)
		a := s.userByLogin(login)
		if a == nil || a.Type != "User" {
			continue // GitHub ignores assignees it can't assign, like apps
		}
		dup := false
		for _, x := range it.assignees {
			dup = dup || x.ID == a.ID
		}
		if !dup {
			it.assignees = append(it.assignees, a)
			it.updatedAt = s.now
			s.event(it, "assigned", u, "", a, "")
		}
	}
	return response{status: http.StatusCreated, body: s.issueJSON(it)}
}

func (s *Server) removeAssignees(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	ls, _ := in["assignees"].([]any)
	s.tick()
	for _, l := range ls {
		login, _ := l.(string)
		for i, x := range it.assignees {
			if strings.EqualFold(x.Login, login) {
				it.assignees = append(it.assignees[:i], it.assignees[i+1:]...)
				it.updatedAt = s.now
				s.event(it, "unassigned", u, "", x, "")
				break
			}
		}
	}
	return response{body: s.issueJSON(it)}
}

func (s *Server) listComments(_ *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	list := []any{}
	for _, c := range s.comments {
		if c.issue == it.number {
			list = append(list, s.commentJSON(c))
		}
	}
	return response{list: list}
}

func (s *Server) createComment(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	body, _ := in["body"].(string)
	if body == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: body is missing")
	}
	s.tick()
	c := &comment{id: s.id(), issue: it.number, body: body, user: u, createdAt: s.now, updatedAt: s.now}
	s.comments = append(s.comments, c)
	it.updatedAt = s.now
	return response{status: http.StatusCreated, body: s.commentJSON(c)}
}

func (s *Server) listRepoComments(_ *User, _ []string, q url.Values, _ map[string]any) response {
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return errorResponse(http.StatusUnprocessableEntity, "Validation Failed")
		}
		since = t
	}
	var cs []*comment
	for _, c := range s.comments {
		if !c.updatedAt.Before(since) {
			cs = append(cs, c)
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		less := cs[i].createdAt.Before(cs[j].createdAt) || (cs[i].createdAt.Equal(cs[j].createdAt) && cs[i].id < cs[j].id)
		if q.Get("direction") == "desc" {
			return !less
		}
		return less
	})
	list := []any{}
	for _, c := range cs {
		list = append(list, s.commentJSON(c))
	}
	return response{list: list}
}

// listEvents lists the repository's issue events, newest first.
func (s *Server) listEvents(_ *User, _ []string, _ url.Values, _ map[string]any) response {
	list := []any{}
	for i := len(s.events) - 1; i >= 0; i-- {
		list = append(list, s.eventJSON(s.events[i]))
	}
	return response{list: list}
}

func (s *Server) listBlockedBy(_ *User, m []string, _ url.Values, _ map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	list := []any{}
	for _, n := range it.blockedBy {
		list = append(list, s.issueJSON(s.issues[n]))
	}
	return response{list: list}
}

func (s *Server) addBlockedBy(u *User, m []string, _ url.Values, in map[string]any) response {
	it, ok := s.find(m)
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	id, _ := in["issue_id"].(float64)
	var blocker *issue
	for _, x := range s.issues {
		if x.id == int64(id) {
			blocker = x
		}
	}
	if blocker == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	for _, n := range it.blockedBy {
		if n == blocker.number {
			return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: dependency already exists")
		}
	}
	if blocker.number == it.number {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: an issue can't block itself")
	}
	s.tick()
	it.blockedBy = append(it.blockedBy, blocker.number)
	it.updatedAt = s.now
	s.event(it, "blocked_by_added", u, "", nil, "")
	s.event(blocker, "blocking_added", u, "", nil, "")
	return response{status: http.StatusCreated, body: s.issueJSON(blocker)}
}

func (s *Server) listLabels(_ *User, _ []string, _ url.Values, _ map[string]any) response {
	names := make([]string, 0, len(s.labels))
	for _, n := range s.labels {
		names = append(names, n)
	}
	sort.Strings(names)
	list := []any{}
	for _, n := range names {
		list = append(list, map[string]any{"name": n})
	}
	return response{list: list}
}

func (s *Server) listPulls(_ *User, _ []string, q url.Values, _ map[string]any) response {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var nums []int
	for n, it := range s.issues {
		if !it.pull || (state != "all" && it.state != state) {
			continue
		}
		if h := q.Get("head"); h != "" && h != s.Owner+":"+it.head {
			continue
		}
		nums = append(nums, n)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(nums)))
	list := []any{}
	for _, n := range nums {
		list = append(list, s.pullJSON(s.issues[n]))
	}
	return response{list: list}
}

// graphql answers the adapter's closers query (closedByPullRequestsReferences
// and the latest ClosedEvent's closer).
func (s *Server) graphql(u *User, in map[string]any) response {
	query, _ := in["query"].(string)
	vars, _ := in["variables"].(map[string]any)
	if res, ok := s.graphqlHost(u, query, vars); ok {
		return res
	}
	if !strings.Contains(query, "closedByPullRequestsReferences") {
		return response{body: map[string]any{"errors": []any{map[string]any{"type": "UNSUPPORTED", "message": "githubtest: unsupported query"}}}}
	}
	num, _ := vars["number"].(float64)
	it := s.issues[int(num)]
	if it == nil || it.pull {
		return response{body: map[string]any{"data": map[string]any{"repository": map[string]any{"issue": nil}},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to an Issue"}}}}
	}
	refs := []any{}
	var prs []int
	for n, p := range s.issues {
		if p.pull {
			for _, c := range p.closes {
				if c == it.number {
					prs = append(prs, n)
				}
			}
		}
	}
	sort.Ints(prs)
	for _, n := range prs {
		refs = append(refs, map[string]any{"number": n, "merged": s.issues[n].mergedAt != nil})
	}
	closed := []any{}
	if it.state == "closed" {
		node := map[string]any{"closer": nil}
		if pr, ok := s.closers[it.number]; ok {
			node["closer"] = map[string]any{"number": pr, "merged": s.issues[pr].mergedAt != nil}
		}
		closed = append(closed, node)
	}
	return response{body: map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{
		"closedByPullRequestsReferences": map[string]any{"nodes": refs},
		"timelineItems":                  map[string]any{"nodes": closed},
	}}}}}
}
