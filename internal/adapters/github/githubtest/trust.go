package githubtest

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bypass is one entry of a ruleset's bypass list, as GitHub names it:
// Type "Integration" (ID is the app ID) or "RepositoryRole" (ID 5 is the
// admin role).
type Bypass struct {
	Type string
	ID   int64
}

// AdminRole is the RepositoryRole ID of the admin role.
const AdminRole = 5

// Ruleset is a repository ruleset.
type Ruleset struct {
	ID          int64
	Name        string
	Target      string // "branch" or "tag"
	Enforcement string // "active", "evaluate" or "disabled"
	Include     []string
	Exclude     []string
	Rules       []string // rule types: creation, update, deletion, non_fast_forward, ...
	Bypass      []Bypass
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// trustState is the repository's access settings: rulesets, collaborator
// permissions, forks, tags and the app behind each bot user.
type trustState struct {
	rulesets  map[int64]*Ruleset
	perms     map[int64]string // user ID to "read", "write" or "admin"; write when unset
	forks     map[string]bool  // logins with a fork
	appIDs    map[int64]int64  // bot user ID to its app ID
	tags      map[string]string
	conv      bool // classic protection: require conversation resolution
	owners    bool // classic protection: require code owner reviews
	rulePR    map[string]any
	nextRSID  int64
	forkLogin string // the fork the current request is for
}

func (s *Server) initTrust() {
	s.trustState = trustState{rulesets: map[int64]*Ruleset{}, perms: map[int64]string{}, forks: map[string]bool{},
		appIDs: map[int64]int64{}, tags: map[string]string{}, nextRSID: 10}
}

// SetPermission sets an account's permission on the repository: "read",
// "write" or "admin".
func (s *Server) SetPermission(u *User, perm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perms[u.ID] = perm
}

// SetAppID records the GitHub App behind a bot user.
func (s *Server) SetAppID(u *User, app int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appIDs[u.ID] = app
}

// AddRuleset creates an active repository ruleset and returns its ID.
func (s *Server) AddRuleset(rs Ruleset) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addRuleset(rs)
}

func (s *Server) addRuleset(rs Ruleset) int64 {
	s.tick()
	s.nextRSID++
	rs.ID = s.nextRSID
	if rs.Enforcement == "" {
		rs.Enforcement = "active"
	}
	rs.CreatedAt, rs.UpdatedAt = s.now, s.now
	s.rulesets[rs.ID] = &rs
	return rs.ID
}

// Rulesets returns copies of the repository's rulesets, by ID.
func (s *Server) Rulesets() []Ruleset {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ruleset
	for _, id := range s.rulesetIDs() {
		out = append(out, *s.rulesets[id])
	}
	return out
}

// RequireConversationResolution sets classic protection's "require
// conversation resolution before merging".
func (s *Server) RequireConversationResolution(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.classic, s.conv = true, on
}

// RequireCodeOwnerReviews sets classic protection's "require review from
// code owners".
func (s *Server) RequireCodeOwnerReviews(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.classic, s.owners = true, on
}

// SetPullRequestRule adds a "pull_request" rule with these parameters to
// the base branch's rules (GET /rules/branches/{base}); nil removes it.
func (s *Server) SetPullRequestRule(params map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rulePR = params
}

func (s *Server) rulesetIDs() []int64 {
	ids := make([]int64, 0, len(s.rulesets))
	for id := range s.rulesets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Server) perm(u *User) string {
	if p, ok := s.perms[u.ID]; ok {
		return p
	}
	return "write"
}

// refMatches applies a ruleset's ref_name conditions to ref.
func (s *Server) refMatches(rs *Ruleset, ref string) bool {
	match := func(p string) bool {
		switch p {
		case "~ALL":
			return true
		case "~DEFAULT_BRANCH":
			return ref == "refs/heads/"+s.defaultBranch
		}
		if pre, ok := strings.CutSuffix(p, "**"); ok {
			return strings.HasPrefix(ref, pre)
		}
		return p == ref
	}
	in := false
	for _, p := range rs.Include {
		in = in || match(p)
	}
	for _, p := range rs.Exclude {
		if match(p) {
			return false
		}
	}
	return in
}

func (s *Server) bypasses(rs *Ruleset, u *User) bool {
	for _, b := range rs.Bypass {
		switch {
		case b.Type == "RepositoryRole" && b.ID == AdminRole && s.perm(u) == "admin":
			return true
		case b.Type == "Integration" && s.appIDs[u.ID] != 0 && s.appIDs[u.ID] == b.ID:
			return true
		}
	}
	return false
}

// refused reports whether u may not take op ("creation", "update" or
// "deletion") on ref ("refs/heads/x" or "refs/tags/x"): it has no write
// access, the ref is in another account's fork, or an active ruleset with
// that rule covers the ref and u doesn't bypass it (work plan #16 D11).
func (s *Server) refused(u *User, ref, op string) (response, bool) {
	if s.perm(u) == "read" {
		return errorResponse(http.StatusForbidden, "Resource not accessible by integration"), true
	}
	name := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	if owner, _, ok := strings.Cut(name, ":"); ok {
		if owner != u.Login {
			return errorResponse(http.StatusForbidden, "Permission to "+owner+"/"+s.Name+" denied"), true
		}
		return response{}, false
	}
	target := "branch"
	if strings.HasPrefix(ref, "refs/tags/") {
		target = "tag"
	}
	for _, id := range s.rulesetIDs() {
		rs := s.rulesets[id]
		if rs.Enforcement != "active" || rs.Target != target || !s.refMatches(rs, ref) || s.bypasses(rs, u) {
			continue
		}
		for _, r := range rs.Rules {
			if r == op {
				return errorResponse(http.StatusForbidden, "Repository rule violations found\n\n- Cannot "+op+" this reference"), true
			}
		}
	}
	return response{}, false
}

// refusedPush checks a commit to an existing or new branch.
func (s *Server) refusedPush(u *User, branch string) (response, bool) {
	op := "update"
	if _, ok := s.branches[branch]; !ok {
		op = "creation"
	}
	return s.refused(u, "refs/heads/"+branch, op)
}

func (s *Server) admin(u *User) (response, bool) {
	if s.perm(u) != "admin" {
		return errorResponse(http.StatusForbidden, "Must have admin rights to Repository."), true
	}
	return response{}, false
}

func rulesetJSON(rs *Ruleset, full bool) map[string]any {
	m := map[string]any{"id": rs.ID, "name": rs.Name, "target": rs.Target, "enforcement": rs.Enforcement,
		"source_type": "Repository", "created_at": rs.CreatedAt.Format(time.RFC3339), "updated_at": rs.UpdatedAt.Format(time.RFC3339)}
	if !full {
		return m
	}
	inc, exc := []any{}, []any{}
	for _, p := range rs.Include {
		inc = append(inc, p)
	}
	for _, p := range rs.Exclude {
		exc = append(exc, p)
	}
	rules := []any{}
	for _, r := range rs.Rules {
		rules = append(rules, map[string]any{"type": r})
	}
	by := []any{}
	for _, b := range rs.Bypass {
		by = append(by, map[string]any{"actor_type": b.Type, "actor_id": b.ID, "bypass_mode": "always"})
	}
	m["conditions"] = map[string]any{"ref_name": map[string]any{"include": inc, "exclude": exc}}
	m["rules"], m["bypass_actors"] = rules, by
	return m
}

func (s *Server) listRulesets(_ *User, _ []string, _ url.Values, _ map[string]any) response {
	list := []any{}
	for _, id := range s.rulesetIDs() {
		list = append(list, rulesetJSON(s.rulesets[id], false))
	}
	return response{list: list}
}

func (s *Server) getRuleset(_ *User, m []string, _ url.Values, _ map[string]any) response {
	id, _ := strconv.ParseInt(m[1], 10, 64)
	rs := s.rulesets[id]
	if rs == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	return response{body: rulesetJSON(rs, true)}
}

// rulesetFrom reads a ruleset body as the rulesets API takes it.
func rulesetFrom(in map[string]any, rs *Ruleset) {
	if v, ok := in["name"].(string); ok {
		rs.Name = v
	}
	if v, ok := in["target"].(string); ok {
		rs.Target = v
	}
	if v, ok := in["enforcement"].(string); ok {
		rs.Enforcement = v
	}
	strs := func(x any) []string {
		var out []string
		xs, _ := x.([]any)
		for _, v := range xs {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if c, ok := in["conditions"].(map[string]any); ok {
		if rn, ok := c["ref_name"].(map[string]any); ok {
			rs.Include, rs.Exclude = strs(rn["include"]), strs(rn["exclude"])
		}
	}
	if rules, ok := in["rules"].([]any); ok {
		rs.Rules = nil
		for _, r := range rules {
			if m, ok := r.(map[string]any); ok {
				t, _ := m["type"].(string)
				rs.Rules = append(rs.Rules, t)
			}
		}
	}
	if by, ok := in["bypass_actors"].([]any); ok {
		rs.Bypass = nil
		for _, b := range by {
			if m, ok := b.(map[string]any); ok {
				t, _ := m["actor_type"].(string)
				id, _ := m["actor_id"].(float64)
				rs.Bypass = append(rs.Bypass, Bypass{Type: t, ID: int64(id)})
			}
		}
	}
}

func (s *Server) createRuleset(u *User, _ []string, _ url.Values, in map[string]any) response {
	if r, no := s.admin(u); no {
		return r
	}
	var rs Ruleset
	rulesetFrom(in, &rs)
	id := s.addRuleset(rs)
	return response{status: http.StatusCreated, body: rulesetJSON(s.rulesets[id], true)}
}

func (s *Server) updateRuleset(u *User, m []string, _ url.Values, in map[string]any) response {
	if r, no := s.admin(u); no {
		return r
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	rs := s.rulesets[id]
	if rs == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	s.tick()
	rulesetFrom(in, rs)
	rs.UpdatedAt = s.now
	return response{body: rulesetJSON(rs, true)}
}

func (s *Server) deleteRuleset(u *User, m []string, _ url.Values, _ map[string]any) response {
	if r, no := s.admin(u); no {
		return r
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	if s.rulesets[id] == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	s.tick()
	delete(s.rulesets, id)
	return response{status: http.StatusNoContent}
}

// listCollaborators lists the accounts with access, filtered by
// ?permission=admin. Every account the stub knows is a collaborator.
func (s *Server) listCollaborators(_ *User, _ []string, q url.Values, _ map[string]any) response {
	var us []*User
	for _, u := range s.users {
		us = append(us, u)
	}
	sort.Slice(us, func(i, j int) bool { return us[i].ID < us[j].ID })
	list := []any{}
	for _, u := range us {
		p := s.perm(u)
		if q.Get("permission") == "admin" && p != "admin" {
			continue
		}
		j := u.json()
		j["permissions"] = map[string]any{"admin": p == "admin", "push": p != "read", "pull": true}
		list = append(list, j)
	}
	return response{list: list}
}

func (s *Server) putCollaborator(u *User, m []string, _ url.Values, in map[string]any) response {
	if r, no := s.admin(u); no {
		return r
	}
	login, _ := url.PathUnescape(m[1])
	c := s.userByLogin(login)
	if c == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	p, _ := in["permission"].(string)
	switch p {
	case "admin":
	case "push", "maintain", "":
		p = "write"
	case "pull", "triage":
		p = "read"
	default:
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: permission")
	}
	s.perms[c.ID] = p
	return response{status: http.StatusNoContent}
}

func (s *Server) deleteCollaborator(u *User, m []string, _ url.Values, _ map[string]any) response {
	if r, no := s.admin(u); no {
		return r
	}
	login, _ := url.PathUnescape(m[1])
	c := s.userByLogin(login)
	if c == nil {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	s.perms[c.ID] = "read"
	return response{status: http.StatusNoContent}
}

// createFork forks the repository for u: its branches are served under
// /repos/<login>/<name>, and kept here as "<login>:<branch>".
func (s *Server) createFork(u *User, _ []string, _ url.Values, _ map[string]any) response {
	if !s.forks[u.Login] {
		s.forks[u.Login] = true
		s.branches[u.Login+":"+s.defaultBranch] = s.branches[s.defaultBranch]
	}
	return response{status: http.StatusAccepted, body: map[string]any{"full_name": u.Login + "/" + s.Name}}
}

// forkRequest rewrites a request on a fork's repository path to the stub's
// branch names. ok is false when the path isn't a known fork's.
func (s *Server) forkRequest(path string, q url.Values, in map[string]any) (rest string, ok bool) {
	for login := range s.forks {
		r, found := strings.CutPrefix(path, "/repos/"+login+"/"+s.Name)
		if !found {
			continue
		}
		pre := login + ":"
		if q.Get("ref") != "" {
			q.Set("ref", pre+q.Get("ref"))
		}
		if b, ok := in["branch"].(string); ok {
			in["branch"] = pre + b
		}
		if ref, ok := in["ref"].(string); ok {
			if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
				in["ref"] = "refs/heads/" + pre + name
			}
		}
		if b, ok := strings.CutPrefix(r, "/branches/"); ok {
			r = "/branches/" + url.PathEscape(pre+mustUnescape(b))
		}
		s.forkLogin = login
		return r, true
	}
	return "", false
}

func mustUnescape(s string) string {
	u, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return u
}

// createTag creates refs/tags/<name> at sha.
func (s *Server) createTag(u *User, name, sha string) response {
	if r, no := s.refused(u, "refs/tags/"+name, "creation"); no {
		return r
	}
	if _, ok := s.tags[name]; ok {
		return errorResponse(http.StatusUnprocessableEntity, "Reference already exists")
	}
	s.tags[name] = sha
	return response{status: http.StatusCreated, body: map[string]any{"ref": "refs/tags/" + name, "object": map[string]any{"sha": sha}}}
}

// getProtection is classic branch protection (GET /branches/{b}/protection).
func (s *Server) getProtection(_ *User, m []string, _ url.Values, _ map[string]any) response {
	name, _ := url.PathUnescape(m[1])
	if name != s.defaultBranch || !s.classic {
		return errorResponse(http.StatusNotFound, "Branch not protected")
	}
	ctxs := []any{}
	for _, c := range s.classicChecks {
		ctxs = append(ctxs, c)
	}
	out := map[string]any{
		"required_conversation_resolution": map[string]any{"enabled": s.conv},
	}
	if len(s.classicChecks) > 0 {
		out["required_status_checks"] = map[string]any{"strict": s.strict, "contexts": ctxs}
	}
	if s.approvals > 0 || s.owners {
		out["required_pull_request_reviews"] = map[string]any{"required_approving_review_count": s.approvals, "require_code_owner_reviews": s.owners}
	}
	return response{body: out}
}

// Names of the rulesets AddRequiredRulesets creates; the Driver's
// SetRuleset finds them by these names (conformance.RulesetLoopBranches and
// so on).
const (
	RulesetLoopBranches = "loop_branches"
	RulesetBaseBranch   = "base_branch"
	RulesetTags         = "tags"
)

// AddRequiredRulesets creates the three rulesets of INTEGRATION-SPEC §9.8
// step 1 as init would: the loop branches under prefix (bypass: the coder
// app and the admin role), the base branch (the loop app and the admin
// role) and tags (the admin role).
func (s *Server) AddRequiredRulesets(prefix, base string, coderApp, loopApp int64) {
	admin := Bypass{Type: "RepositoryRole", ID: AdminRole}
	s.AddRuleset(Ruleset{Name: RulesetLoopBranches, Target: "branch", Include: []string{"refs/heads/" + prefix + "**"},
		Rules:  []string{"creation", "update", "deletion", "non_fast_forward"},
		Bypass: []Bypass{{Type: "Integration", ID: coderApp}, admin}})
	s.AddRuleset(Ruleset{Name: RulesetBaseBranch, Target: "branch", Include: []string{"refs/heads/" + base},
		Rules:  []string{"update", "deletion", "non_fast_forward"},
		Bypass: []Bypass{{Type: "Integration", ID: loopApp}, admin}})
	s.AddRuleset(Ruleset{Name: RulesetTags, Target: "tag", Include: []string{"refs/tags/**"},
		Rules: []string{"creation", "update", "deletion"}, Bypass: []Bypass{admin}})
}
