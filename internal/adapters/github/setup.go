package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// The GitHub adapter's agent-loop init steps (INTEGRATION-SPEC §12.2).
// They act with the owner's own token, which a Client made by
// NewOwnerClient holds in memory under AsOwner. Every step reads the
// repository and returns the changes it would make, so init can show them,
// ask, apply them, or only report them for --check (work plan #24).

// AsOwner is the identity key under which an owner client holds the
// owner's token. It is the loop's key because the Host reads with that
// key, so init can reuse the Host's ruleset checks with the owner's token.
const AsOwner = ports.IdentityLoop

// NewOwnerClient returns a client for repo that acts with the owner's
// token. The token is held in memory only.
func NewOwnerClient(baseURL, repo, token string) (*Client, error) {
	return NewClient(ClientConfig{BaseURL: baseURL, Repo: repo,
		Tokens: map[ports.Identity]TokenSource{AsOwner: StaticToken(token)}})
}

// SetupChange is one change an init step would make.
type SetupChange struct {
	// Summary says what the change does, for the owner to approve.
	Summary string
	Apply   func(ctx context.Context) error
}

// RepoInfo is what step 1 reads.
type RepoInfo struct {
	Login         string // the owner's login
	UserID        int64  // the owner's account ID
	Admin         bool   // the owner has admin rights on the repository
	DefaultBranch string
	OwnerType     string // "User" or "Organization"
	HTMLURL       string
	// RulesetsOK is false when GitHub refuses to list the rulesets
	// (§9.8 step 5).
	RulesetsOK bool
}

// Detect is step 1: the owner's login and ID, their admin right, the
// default branch and whether the plan allows rulesets.
func Detect(ctx context.Context, c *Client) (RepoInfo, error) {
	var me struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := c.Get(ctx, AsOwner, "/user", nil, &me); err != nil {
		return RepoInfo{}, fmt.Errorf("reading your GitHub login: %w", err)
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
		Owner         struct {
			Type string `json:"type"`
		} `json:"owner"`
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if err := c.Get(ctx, AsOwner, c.RepoPath(""), nil, &repo); err != nil {
		return RepoInfo{}, fmt.Errorf("reading %s: %w", c.Repo(), err)
	}
	info := RepoInfo{Login: me.Login, UserID: me.ID, Admin: repo.Permissions.Admin, DefaultBranch: repo.DefaultBranch,
		OwnerType: repo.Owner.Type, HTMLURL: repo.HTMLURL, RulesetsOK: true}
	if info.HTMLURL == "" {
		info.HTMLURL = "https://github.com/" + c.Repo()
	}
	_, err := List[rulesetSummary](ctx, c, AsOwner, c.RepoPath("/rulesets"), nil)
	switch {
	case isStatus(err, http.StatusForbidden, http.StatusNotFound):
		info.RulesetsOK = false
	case err != nil:
		return RepoInfo{}, fmt.Errorf("reading the rulesets of %s: %w", c.Repo(), err)
	}
	return info, nil
}

// WantPermissions returns the permissions an identity's app is created
// with: the union of its rows in §9.1, the same table validate checks.
func WantPermissions(id ports.Identity) map[string]string {
	return maps.Clone(wantPermissions[id])
}

// AppManifest is the GitHub App manifest for an identity's app (§12.2
// step 2): exactly the §9.1 permissions, no webhook, no events, private.
func AppManifest(id ports.Identity, name, repoURL, redirectURL string) ([]byte, error) {
	perms, ok := wantPermissions[id]
	if !ok {
		return nil, fmt.Errorf("github: no app permissions for identity %q", id)
	}
	return json.Marshal(map[string]any{
		"name":                     name,
		"url":                      repoURL,
		"public":                   false,
		"redirect_url":             redirectURL,
		"default_permissions":      perms,
		"default_events":           []string{},
		"hook_attributes":          map[string]any{"url": repoURL, "active": false},
		"request_oauth_on_install": false,
	})
}

// CreatedApp is an app the manifest flow created.
type CreatedApp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	PEM  string `json:"pem"`
}

// ConvertManifest exchanges the manifest flow's one-time code for the new
// app's ID, slug and private key (POST /app-manifests/{code}/conversions,
// which takes no credentials).
func ConvertManifest(ctx context.Context, baseURL, code string, hc *http.Client) (CreatedApp, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return CreatedApp{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := hc.Do(req)
	if err != nil {
		return CreatedApp{}, fmt.Errorf("github: converting the app manifest: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusCreated {
		return CreatedApp{}, fmt.Errorf("github: converting the app manifest: status %d", res.StatusCode)
	}
	var app CreatedApp
	if err := json.Unmarshal(data, &app); err != nil || app.ID == 0 || app.PEM == "" {
		return CreatedApp{}, errors.New("github: the app manifest conversion returned no app ID or key")
	}
	return app, nil
}

// SetupConfig configures the steps that need the project's settings.
type SetupConfig struct {
	BaseBranch   string
	BranchPrefix string
	// Apps are the identities' app IDs; zero when an app doesn't exist yet.
	Apps        AppIDs
	ReleaseApps []int64
}

// Setup runs the steps after the profile: labels, push rules and gates.
type Setup struct {
	c    *Client
	h    *Host
	cfg  SetupConfig
	base string
}

// NewSetup returns the steps for an owner client.
func NewSetup(c *Client, cfg SetupConfig) (*Setup, error) {
	if cfg.BranchPrefix == "" {
		cfg.BranchPrefix = "agent/"
	}
	// The Host needs identity IDs, which the ruleset checks don't use.
	ids := Identities{Coder: ports.Actor{ID: "coder"}, Reviewer: ports.Actor{ID: "reviewer"}, Loop: ports.Actor{ID: "loop"}}
	h, err := NewHost(c, HostConfig{Identities: ids, BaseBranch: cfg.BaseBranch, BranchPrefix: cfg.BranchPrefix,
		Apps: cfg.Apps, ReleaseApps: cfg.ReleaseApps})
	if err != nil {
		return nil, err
	}
	return &Setup{c: c, h: h, cfg: cfg, base: h.base}, nil
}

// LabelColor is the colour of the labels init creates (work plan #24 D9).
const LabelColor = "ededed"

// Labels is step 4: a change for each label in names that doesn't exist,
// matched case-insensitively. Existing labels are left unchanged.
func (s *Setup) Labels(ctx context.Context, names []string) ([]SetupChange, error) {
	type label struct {
		Name string `json:"name"`
	}
	have, err := List[label](ctx, s.c, AsOwner, s.c.RepoPath("/labels"), nil)
	if err != nil {
		return nil, fmt.Errorf("listing the labels of %s: %w", s.c.Repo(), err)
	}
	seen := map[string]bool{}
	for _, l := range have {
		seen[strings.ToLower(l.Name)] = true
	}
	var out []SetupChange
	for _, n := range names {
		if seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, SetupChange{Summary: fmt.Sprintf("create label %q", n), Apply: func(ctx context.Context) error {
			err := s.c.Send(ctx, AsOwner, http.MethodPost, s.c.RepoPath("/labels"), map[string]any{"name": n, "color": LabelColor}, nil)
			if isStatus(err, http.StatusUnprocessableEntity) { // created since it was listed
				return nil
			}
			return err
		}})
	}
	return out, nil
}

// Ruleset names (work plan #24 D7).
const (
	RulesetNameLoop  = "agent-loop: loop branches"
	RulesetNameBase  = "agent-loop: base branch"
	RulesetNameTags  = "agent-loop: tags"
	RulesetNameOther = "agent-loop: other branches"
)

func (s *Setup) wantRuleset(k rulesetKind) (target string, include, exclude, rules []string, bypass []map[string]any) {
	admin := map[string]any{"actor_type": "RepositoryRole", "actor_id": adminRole, "bypass_mode": "always"}
	app := func(id int64) map[string]any {
		return map[string]any{"actor_type": "Integration", "actor_id": id, "bypass_mode": "always"}
	}
	switch k {
	case rulesetLoop:
		return "branch", s.h.includes(k, s.base), nil, []string{"creation", "update", "deletion", "non_fast_forward"},
			[]map[string]any{app(s.cfg.Apps.Coder), admin}
	case rulesetBase:
		return "branch", s.h.includes(k, s.base), nil, []string{"update", "deletion", "non_fast_forward"},
			[]map[string]any{app(s.cfg.Apps.Loop), admin}
	case rulesetTags:
		by := []map[string]any{admin}
		for _, a := range s.cfg.ReleaseApps {
			by = append(by, app(a))
		}
		return "tag", []string{"refs/tags/**"}, nil, []string{"creation", "update", "deletion"}, by
	}
	return "branch", []string{"~ALL"}, []string{"refs/heads/" + s.cfg.BranchPrefix + "**", "refs/heads/" + s.base},
		[]string{"creation", "update", "deletion", "non_fast_forward"}, []map[string]any{admin}
}

func rulesetName(k rulesetKind) string {
	return [...]string{RulesetNameLoop, RulesetNameBase, RulesetNameTags}[k]
}

// Rulesets is step 5's push rules: changes that create or repair the three
// rulesets of §9.8 step 1, and, separately, the optional ruleset for other
// branches when nothing covers them. A ruleset counts as in place by the
// same rules project validate uses.
func (s *Setup) Rulesets(ctx context.Context) (required, other []SetupChange, err error) {
	if s.cfg.Apps.Coder == 0 || s.cfg.Apps.Loop == 0 {
		return nil, nil, errors.New("github: the rulesets need the coder and loop apps' IDs")
	}
	ds, refused, err := s.h.rulesets(ctx)
	if refused {
		return nil, nil, fmt.Errorf("GitHub refuses to list the rulesets of %s: rulesets need a public repository or a paid plan", s.c.Repo())
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the rulesets of %s: %w", s.c.Repo(), err)
	}
	var fit [3]*rulesetDetail
	for k := rulesetLoop; k <= rulesetTags; k++ {
		if k == rulesetBase {
			var own *rulesetDetail
			var done bool
			fit[k], own, done = s.baseRuleset(ds)
			if !done {
				s.planRuleset(&required, k, own)
			}
			continue
		}
		var candidate *rulesetDetail
		for i := range ds {
			if s.h.misfit(ds[i], k, s.base) == "" {
				fit[k] = &ds[i]
				break
			}
			if candidate == nil && s.h.aimsAt(ds[i], k, s.base) && isRepoRuleset(ds[i]) {
				candidate = &ds[i]
			}
		}
		if d := fit[k]; d != nil {
			if k != rulesetLoop || d.bypasses("Integration", s.cfg.Apps.Coder) {
				continue
			}
			candidate = d
		}
		s.planRuleset(&required, k, candidate)
	}
	if !s.h.othersCovered(ds, fit) {
		target, include, exclude, rules, bypass := s.wantRuleset(rulesetKind(3))
		other = append(other, SetupChange{
			Summary: fmt.Sprintf("create the optional ruleset %q: every branch outside %s** and %s gets %s, bypassed only by repository admins (keeps the coder app off them; other collaborators can't push them either)",
				RulesetNameOther, s.cfg.BranchPrefix, s.base, strings.Join(rules, ", ")),
			Apply: func(ctx context.Context) error {
				return s.c.Send(ctx, AsOwner, http.MethodPost, s.c.RepoPath("/rulesets"),
					rulesetBody(RulesetNameOther, target, include, exclude, ruleList(nil, rules), bypass), nil)
			}})
	}
	return required, other, nil
}

func isRepoRuleset(d rulesetDetail) bool {
	return d.SourceType == "" || d.SourceType == "Repository"
}

// baseRuleset finds the base-branch ruleset (work plan #24 D7, D18). fit
// is a ruleset that fits and lets the loop app bypass it, or failing that
// the first one that fits; done is true when the first kind exists, so
// nothing needs to change. Otherwise own is the loop's own ruleset (named
// RulesetNameBase) to repair, or nil when a new one must be created. A
// ruleset of the owner's (any other name) is never repaired: giving the
// loop app bypass there would let its merges skip the owner's rules for
// the base branch, such as required checks and pull requests.
func (s *Setup) baseRuleset(ds []rulesetDetail) (fit, own *rulesetDetail, done bool) {
	for i := range ds {
		d := &ds[i]
		if own == nil && d.Name == RulesetNameBase && isRepoRuleset(*d) {
			own = d
		}
		if s.h.misfit(*d, rulesetBase, s.base) != "" {
			continue
		}
		if d.bypasses("Integration", s.cfg.Apps.Loop) {
			return d, nil, true
		}
		if fit == nil {
			fit = d
		}
	}
	return fit, own, false
}

// planRuleset adds the change that creates the ruleset of kind k, or
// repairs candidate when it is not nil.
func (s *Setup) planRuleset(required *[]SetupChange, k rulesetKind, candidate *rulesetDetail) {
	target, include, exclude, rules, bypass := s.wantRuleset(k)
	if candidate == nil {
		name := rulesetName(k)
		*required = append(*required, SetupChange{
			Summary: fmt.Sprintf("create ruleset %q on %s (%s), bypassed only by %s", name, strings.Join(include, ", "), strings.Join(rules, ", "), bypassNames(k)),
			Apply: func(ctx context.Context) error {
				return s.c.Send(ctx, AsOwner, http.MethodPost, s.c.RepoPath("/rulesets"),
					rulesetBody(name, target, include, exclude, ruleList(nil, rules), bypass), nil)
			}})
		return
	}
	id, name := candidate.ID, candidate.Name
	why := s.h.misfit(*candidate, k, s.base)
	if why == "" {
		why = fmt.Sprintf("ruleset %q lacks the %s app in its bypass list", name, map[rulesetKind]string{rulesetLoop: "coder", rulesetBase: "loop"}[k])
	}
	*required = append(*required, SetupChange{
		Summary: fmt.Sprintf("repair ruleset %q for the %s: %s", name, k, why),
		Apply: func(ctx context.Context) error {
			var raw map[string]any
			if err := s.c.Get(ctx, AsOwner, s.c.RepoPath("/rulesets/%d", id), nil, &raw); err != nil {
				return err
			}
			existing, _ := raw["rules"].([]any)
			if k == rulesetBase {
				// The loop app bypasses this ruleset, so it keeps only push
				// rules (§9.8 step 1).
				existing = slices.DeleteFunc(slices.Clone(existing), func(r any) bool {
					m, _ := r.(map[string]any)
					t, _ := m["type"].(string)
					return !slices.Contains(basePushRules, t)
				})
			}
			return s.c.Send(ctx, AsOwner, http.MethodPut, s.c.RepoPath("/rulesets/%d", id),
				rulesetBody(name, target, include, exclude, ruleList(existing, rules), bypass), nil)
		}})
}

func bypassNames(k rulesetKind) string {
	switch k {
	case rulesetLoop:
		return "the coder app and repository admins"
	case rulesetBase:
		return "the loop app and repository admins"
	}
	return "repository admins and the profile's release apps"
}

// ruleList keeps the existing rules (with their parameters) and adds each
// of want that is missing.
func ruleList(existing []any, want []string) []any {
	out := slices.Clone(existing)
	have := map[string]bool{}
	for _, r := range existing {
		if m, ok := r.(map[string]any); ok {
			t, _ := m["type"].(string)
			have[t] = true
		}
	}
	for _, t := range want {
		if !have[t] {
			out = append(out, map[string]any{"type": t})
		}
	}
	return out
}

func rulesetBody(name, target string, include, exclude []string, rules []any, bypass []map[string]any) map[string]any {
	if exclude == nil {
		exclude = []string{}
	}
	return map[string]any{"name": name, "target": target, "enforcement": "active",
		"conditions":    map[string]any{"ref_name": map[string]any{"include": include, "exclude": exclude}},
		"rules":         rules,
		"bypass_actors": bypass}
}

// BaseChecks is what the base branch's required checks look like (§9.7).
type BaseChecks struct {
	// Checks are the required checks on the base branch, sorted.
	Checks []string
	// UpToDate is true when branches must be up to date before merging.
	UpToDate bool
	// Change turns UpToDate on; nil when it is on or there are no checks.
	Change *SetupChange
	// Note says why UpToDate can't be turned on, when it can't.
	Note string
}

// UpToDate reads the base branch's required checks and returns the change
// that turns on "require branches to be up to date" where the checks are
// defined (work plan #24 D6).
func (s *Setup) UpToDate(ctx context.Context) (BaseChecks, error) {
	type rule struct {
		Type      string          `json:"type"`
		RulesetID int64           `json:"ruleset_id"`
		Params    json.RawMessage `json:"parameters"`
	}
	rules, err := List[rule](ctx, s.c, AsOwner, s.c.RepoPath("/rules/branches/%s", url.PathEscape(s.base)), nil)
	if err != nil {
		return BaseChecks{}, fmt.Errorf("reading the rules of branch %q: %w", s.base, err)
	}
	var out BaseChecks
	checks := map[string]bool{}
	var rulesets []int64
	for _, r := range rules {
		if r.Type != "required_status_checks" {
			continue
		}
		var p struct {
			Strict bool `json:"strict_required_status_checks_policy"`
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		}
		_ = json.Unmarshal(r.Params, &p)
		out.UpToDate = out.UpToDate || p.Strict
		for _, c := range p.Checks {
			checks[c.Context] = true
		}
		if len(p.Checks) > 0 && !slices.Contains(rulesets, r.RulesetID) {
			rulesets = append(rulesets, r.RulesetID)
		}
	}
	var classic struct {
		Strict   bool     `json:"strict"`
		Contexts []string `json:"contexts"`
	}
	hasClassic := false
	err = s.c.Get(ctx, AsOwner, s.c.RepoPath("/branches/%s/protection/required_status_checks", url.PathEscape(s.base)), nil, &classic)
	switch {
	case errors.Is(err, ports.ErrNotFound):
	case err != nil:
		return BaseChecks{}, fmt.Errorf("reading the protection of branch %q: %w", s.base, err)
	default:
		hasClassic = len(classic.Contexts) > 0
		out.UpToDate = out.UpToDate || classic.Strict
		for _, c := range classic.Contexts {
			checks[c] = true
		}
	}
	for c := range checks {
		out.Checks = append(out.Checks, c)
	}
	sort.Strings(out.Checks)
	if len(out.Checks) == 0 || out.UpToDate {
		return out, nil
	}
	for _, id := range rulesets {
		var raw map[string]any
		err := s.c.Get(ctx, AsOwner, s.c.RepoPath("/rulesets/%d", id), nil, &raw)
		if isStatus(err, http.StatusNotFound, http.StatusForbidden) { // not a repository ruleset
			continue
		}
		if err != nil {
			return BaseChecks{}, fmt.Errorf("reading ruleset %d: %w", id, err)
		}
		name, _ := raw["name"].(string)
		out.Change = &SetupChange{
			Summary: fmt.Sprintf("turn on \"require branches to be up to date before merging\" in ruleset %q, which requires the base branch's checks", name),
			Apply: func(ctx context.Context) error {
				var raw map[string]any
				if err := s.c.Get(ctx, AsOwner, s.c.RepoPath("/rulesets/%d", id), nil, &raw); err != nil {
					return err
				}
				rules, _ := raw["rules"].([]any)
				for _, r := range rules {
					m, _ := r.(map[string]any)
					if m == nil || m["type"] != "required_status_checks" {
						continue
					}
					p, _ := m["parameters"].(map[string]any)
					if p == nil {
						p = map[string]any{}
					}
					p["strict_required_status_checks_policy"] = true
					m["parameters"] = p
				}
				body := map[string]any{"name": raw["name"], "target": raw["target"], "enforcement": raw["enforcement"],
					"conditions": raw["conditions"], "rules": rules, "bypass_actors": raw["bypass_actors"]}
				return s.c.Send(ctx, AsOwner, http.MethodPut, s.c.RepoPath("/rulesets/%d", id), body, nil)
			}}
		return out, nil
	}
	if hasClassic {
		out.Change = &SetupChange{
			Summary: fmt.Sprintf("turn on \"require branches to be up to date before merging\" in the classic protection of branch %q", s.base),
			Apply: func(ctx context.Context) error {
				return s.c.Send(ctx, AsOwner, http.MethodPatch, s.c.RepoPath("/branches/%s/protection/required_status_checks", url.PathEscape(s.base)),
					map[string]any{"strict": true}, nil)
			}}
		return out, nil
	}
	out.Note = fmt.Sprintf("the required checks of branch %q come from an organisation ruleset, which init can't change: turn on \"require branches to be up to date\" there", s.base)
	return out, nil
}

// BaseRuleProblems are the base-branch rules the loop can't meet (§9.7),
// as project validate reports them, without the "up to date" warning,
// which UpToDate handles, and the rulesets that let the loop app bypass
// the base branch's own rules (§9.8 step 1).
func (s *Setup) BaseRuleProblems(ctx context.Context) []ports.Problem {
	var out []ports.Problem
	for _, p := range s.h.validateBaseRules(ctx, s.base) {
		if p.Code != "up_to_date_not_required" {
			out = append(out, p)
		}
	}
	// Rulesets the loop app bypasses on the base branch (work plan #71 D1).
	// The rulesets' own problems are the Rulesets step's, so only this one
	// is taken from them.
	for _, p := range s.h.validateRulesets(ctx, s.base) {
		if p.Code == "ruleset_bypasses_base_rules" {
			out = append(out, p)
		}
	}
	return out
}

// Admins lists the accounts with admin rights on the repository (§9.8
// step 2).
func (s *Setup) Admins(ctx context.Context) ([]ports.OwnerAccount, error) {
	cs, err := s.h.admins(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the admins of %s: %w", s.c.Repo(), err)
	}
	seen := map[int64]bool{}
	var out []ports.OwnerAccount
	for _, c := range cs {
		if c.Type == "Bot" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, ports.OwnerAccount{Login: c.Login, ID: strconv.FormatInt(c.ID, 10)})
	}
	return out, nil
}
