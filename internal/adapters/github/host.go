package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// HostConfig configures the CodeHost.
type HostConfig struct {
	// Labels maps each marker to its label, as for the tracker. Markers it
	// leaves out take DefaultLabels.
	Labels       map[ports.Marker]string
	Identities   Identities
	BaseBranch   string // "main" when empty
	BranchPrefix string // "agent/" when empty
	// Gates replaces the base branch's required checks when set (§9.7).
	Gates []string
	// ManualCommentBots are the bot logins whose comments are manual
	// comments (I7).
	ManualCommentBots []string
	PageSize          int // events per Changes call; 100 when zero

	// Owners are the owner accounts' IDs (I13): the only admins trust
	// accepts (§9.8 step 2).
	Owners []string
	// Apps are the identities' app IDs, as ruleset bypass lists name them.
	Apps AppIDs
	// ReleaseApps are the app IDs the tag ruleset may also let bypass it.
	ReleaseApps []int64
	// Runs is the worker's coder-run records, for HeadMoved.Pusher and
	// trust (I11). Without it every push is PusherUnknown.
	Runs RunLog
	// Permissions reads the apps' installed permissions for Validate.
	Permissions AppPermissions
	// Rulesets keeps the owner's last read of the rulesets, which trust
	// and Validate compare the loop's view with (#101 D1). Without it every
	// hidden bypass list is unverified.
	Rulesets RulesetRecords
	// OwnerClient, when set, acts with the owner's token: Validate reads
	// the rulesets with it first and saves them as the records (init and
	// project validate --owner-login, #101 D2). The loop never has one.
	OwnerClient *Client
	// PollInterval is how long the ruleset and admin state is cached; 60s
	// when zero (§9.8 step 3).
	PollInterval time.Duration
}

// Host is the GitHub CodeHost over pull requests (INTEGRATION-SPEC §8.3,
// §9). It shares its Client, and so its rate budget, with the Tracker.
type Host struct {
	c        *Client
	ids      Identities
	labels   map[ports.Marker]string
	markers  map[string]ports.Marker
	base     string
	prefix   string
	gates    []string
	bots     map[string]bool
	pageSize int

	owners      map[string]bool
	apps        AppIDs
	releaseApps []int64
	runs        RunLog
	perms       AppPermissions
	poll        time.Duration
	records     RulesetRecords
	owner       *Client

	mu        sync.Mutex
	obs       map[string]threadObs    // what was seen of each review thread (work plan D9)
	pushers   map[string]pushObs      // who pushed each commit seen (#16 D2)
	lastPoll  time.Time               // the last Changes call that returned no events
	feedStart time.Time               // the window start of this host's first Changes cursor
	seen      map[string][2]time.Time // heads a feed read saw with no head move, and their push window
	trust     *trustState             // cached for one poll interval (#16 D6)
	adminSeen time.Time               // the last time an admin who isn't an owner was seen
}

var _ ports.CodeHost = (*Host)(nil)

// NewHost returns the code host on c's repository.
func NewHost(c *Client, cfg HostConfig) (*Host, error) {
	h := &Host{
		c:        c,
		ids:      cfg.Identities,
		base:     cfg.BaseBranch,
		prefix:   cfg.BranchPrefix,
		gates:    append([]string(nil), cfg.Gates...),
		bots:     map[string]bool{},
		pageSize: cfg.PageSize,
		obs:      map[string]threadObs{},

		owners:      map[string]bool{},
		apps:        cfg.Apps,
		releaseApps: append([]int64(nil), cfg.ReleaseApps...),
		runs:        cfg.Runs,
		perms:       cfg.Permissions,
		records:     cfg.Rulesets,
		owner:       cfg.OwnerClient,
		poll:        cfg.PollInterval,
		pushers:     map[string]pushObs{},
		seen:        map[string][2]time.Time{},
	}
	for _, o := range cfg.Owners {
		h.owners[o] = true
	}
	if h.poll <= 0 {
		h.poll = time.Minute
	}
	if h.ids.Loop.ID == "" || h.ids.Coder.ID == "" || h.ids.Reviewer.ID == "" {
		return nil, errors.New("github: host needs the IDs of all three identities")
	}
	if h.base == "" {
		h.base = "main"
	}
	if h.prefix == "" {
		h.prefix = "agent/"
	}
	if h.pageSize <= 0 {
		h.pageSize = 100
	}
	for _, b := range cfg.ManualCommentBots {
		h.bots[botLogin(b)] = true
	}
	var err error
	if h.labels, h.markers, err = labelMaps(cfg.Labels); err != nil {
		return nil, err
	}
	return h, nil
}

// botLogin is a bot's login without its "[bot]" suffix, lower case, so the
// REST and GraphQL forms compare equal (work plan D8).
func botLogin(login string) string {
	return strings.ToLower(strings.TrimSuffix(login, "[bot]"))
}

const (
	asCoder    = ports.IdentityCoder
	asReviewer = ports.IdentityReviewer
)

// Capabilities: GitHub's push ruleset is the push record (D40), and review
// comments can be on a file (I8).
func (h *Host) Capabilities() ports.HostCaps {
	return ports.HostCaps{PushRule: true, FileLevelComments: true, MaxBody: MaxBody}
}

// pullRequest is a pull request as the REST API returns it.
type pullRequest struct {
	Number    int        `json:"number"`
	HTMLURL   string     `json:"html_url"`
	State     string     `json:"state"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	User      user       `json:"user"`
	Labels    []label    `json:"labels"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	MergedBy  *user      `json:"merged_by"`
	// MergeCommitSHA is the commit the merge made on the base.
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (p pullRequest) merged() bool { return p.Merged || p.MergedAt != nil }

func (p pullRequest) hasLabel(name string) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// changeNumber parses a change ref.
func changeNumber(ref ports.ChangeRef) (int, error) {
	n, err := strconv.Atoi(string(ref))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("github: change %q is not a pull request number: %w", ref, ports.ErrNotFound)
	}
	return n, nil
}

func (h *Host) pull(ctx context.Context, ref ports.ChangeRef) (pullRequest, error) {
	n, err := changeNumber(ref)
	if err != nil {
		return pullRequest{}, err
	}
	var p pullRequest
	err = h.c.Get(ctx, asLoop, h.c.RepoPath("/pulls/%d", n), nil, &p)
	return p, err
}

// branchFor returns the branch §3.3 names for the item's n-th change.
func (h *Host) branchFor(item string, n int) string {
	if n <= 1 {
		return h.prefix + item
	}
	return h.prefix + item + "-" + strconv.Itoa(n)
}

var branchSuffix = regexp.MustCompile(`^-([2-9]|[1-9][0-9]+)$`)

// branchN returns n when branch is the name of the item's n-th change.
func (h *Host) branchN(item, branch string) (int, bool) {
	rest, ok := strings.CutPrefix(branch, h.prefix+item)
	switch {
	case !ok:
		return 0, false
	case rest == "":
		return 1, true
	case branchSuffix.MatchString(rest):
		n, _ := strconv.Atoi(rest[1:])
		return n, true
	}
	return 0, false
}

// changeRecord is the part of a change record the adapter reads.
type changeRecord struct {
	Item string `json:"item"`
	N    int    `json:"n"`
}

// recordItem returns the item a pull request is linked to by its coder
// change record on the branch §3.3 names (§6.2, work plan D2).
func (h *Host) recordItem(p pullRequest) (string, bool) {
	r, ok := h.ids.recordIn(p.Body, p.User, time.Time{}, "", ports.RecordChange)
	if !ok {
		return "", false
	}
	var d changeRecord
	if json.Unmarshal(r.Data, &d) != nil || d.Item == "" {
		return "", false
	}
	if d.N > 0 {
		return d.Item, p.Head.Ref == h.branchFor(d.Item, d.N)
	}
	_, ok = h.branchN(d.Item, p.Head.Ref)
	return d.Item, ok
}

const pullClosingQuery = `query PullClosing($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { closingIssuesReferences(first: 10) { nodes { number } } }
  }
}`

// closingItem returns the first issue a pull request closes, or "".
func (h *Host) closingItem(ctx context.Context, n int) (string, error) {
	var res struct {
		Repository struct {
			PullRequest struct {
				Refs struct {
					Nodes []struct {
						Number int `json:"number"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": h.c.Owner(), "name": h.c.Name(), "number": n}
	if err := h.c.GraphQL(ctx, asLoop, pullClosingQuery, vars, &res); err != nil {
		return "", err
	}
	if nodes := res.Repository.PullRequest.Refs.Nodes; len(nodes) > 0 {
		return strconv.Itoa(nodes[0].Number), nil
	}
	return "", nil
}

func (h *Host) view(ctx context.Context, p pullRequest) (ports.Change, error) {
	v := ports.Change{
		Ref:    ports.ChangeRef(strconv.Itoa(p.Number)),
		URL:    p.HTMLURL,
		Branch: p.Head.Ref,
		Base:   p.Base.Ref,
		Head:   ports.Commit(p.Head.SHA),
		State:  ports.ChangeStateOpen,
		Fork:   p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, h.c.Repo()),
		Author: p.User.actor(),
	}
	switch {
	case p.merged():
		v.State = ports.ChangeStateMerged
	case p.State == "closed":
		v.State = ports.ChangeStateClosed
	}
	if p.MergedBy != nil {
		a := p.MergedBy.actor()
		v.MergedBy = &a
	}
	if p.MergedAt != nil {
		v.MergedAt = *p.MergedAt
	}
	for _, l := range p.Labels {
		if m, ok := h.markers[strings.ToLower(l.Name)]; ok {
			v.Markers = append(v.Markers, m)
		}
	}
	sort.Slice(v.Markers, func(i, j int) bool { return v.Markers[i] < v.Markers[j] })
	if v.Plan = h.proposalPlan(p); v.Plan != "" {
		return v, nil // a proposal has no item (#156 D6)
	}
	if item, ok := h.recordItem(p); ok {
		v.Item = ports.ItemRef(item)
		return v, nil
	}
	item, err := h.closingItem(ctx, p.Number)
	if err != nil {
		return ports.Change{}, err
	}
	v.Item = ports.ItemRef(item)
	return v, nil
}

// Change returns the pull request's state, head, base, labels and link.
func (h *Host) Change(ctx context.Context, ref ports.ChangeRef) (ports.Change, error) {
	p, err := h.pull(ctx, ref)
	if err != nil {
		return ports.Change{}, err
	}
	return h.view(ctx, p)
}

// pullsOn lists the pull requests, open and closed, whose head is branch.
func (h *Host) pullsOn(ctx context.Context, branch string) ([]pullRequest, error) {
	q := url.Values{"state": {"all"}, "head": {h.c.Owner() + ":" + branch}}
	ps, err := List[pullRequest](ctx, h.c, asLoop, h.c.RepoPath("/pulls"), q)
	if err != nil {
		return nil, err
	}
	out := ps[:0]
	for _, p := range ps {
		if p.Head.Ref == branch {
			out = append(out, p)
		}
	}
	return out, nil
}

const closingRefsQuery = `query ClosingRefs($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) { closedByPullRequestsReferences(first: 25, includeClosedPrs: false) { nodes { number state } } }
  }
}`

// ChangeForItem returns the item's open change, or nil (§6.2, work plan D2).
func (h *Host) ChangeForItem(ctx context.Context, item ports.ItemRef) (*ports.Change, error) {
	n, err := number(item)
	if err != nil {
		return nil, err
	}
	ref := strconv.Itoa(n)
	var best *pullRequest
	for i := 1; i <= maxBranches; i++ {
		ps, err := h.pullsOn(ctx, h.branchFor(ref, i))
		if err != nil {
			return nil, err
		}
		if len(ps) == 0 {
			break
		}
		for j := range ps {
			p := ps[j]
			if p.State != "open" {
				continue
			}
			if it, ok := h.recordItem(p); ok && it == ref && (best == nil || p.Number < best.Number) {
				best = &p
			}
		}
	}
	if best == nil {
		var res struct {
			Repository struct {
				Issue struct {
					Refs struct {
						Nodes []struct {
							Number int    `json:"number"`
							State  string `json:"state"`
						} `json:"nodes"`
					} `json:"closedByPullRequestsReferences"`
				} `json:"issue"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": h.c.Owner(), "name": h.c.Name(), "number": n}
		if err := h.c.GraphQL(ctx, asLoop, closingRefsQuery, vars, &res); err != nil {
			return nil, err
		}
		low := 0
		for _, p := range res.Repository.Issue.Refs.Nodes {
			if p.State == "OPEN" && (low == 0 || p.Number < low) {
				low = p.Number
			}
		}
		if low == 0 {
			return nil, nil
		}
		p, err := h.pull(ctx, ports.ChangeRef(strconv.Itoa(low)))
		if err != nil {
			return nil, err
		}
		best = &p
	}
	v, err := h.view(ctx, *best)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// OpenChange opens a pull request from n.Branch as the coder (work plan D1).
// A pull request on the branch with the coder's change record for key is
// returned instead.
func (h *Host) OpenChange(ctx context.Context, n ports.NewChange, as ports.Identity, key ports.IdemKey) (ports.ChangeRef, error) {
	if as != asCoder {
		return "", fmt.Errorf("github: changes are opened as the coder, not %s: %w", as, ports.ErrPermission)
	}
	if n.Record.Type == ports.RecordProposal {
		return h.openProposal(ctx, n, key)
	}
	num, err := number(n.Item)
	if err != nil {
		return "", err
	}
	item := strconv.Itoa(num)
	base := n.Base
	if base == "" {
		base = h.base
	}
	if n.Branch == "" || n.Branch == base {
		return "", fmt.Errorf("github: a change from branch %q onto %q: %w", n.Branch, base, ports.ErrConflict)
	}
	ps, err := h.pullsOn(ctx, n.Branch)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if r, ok := h.ids.recordIn(p.Body, p.User, time.Time{}, "", ports.RecordChange); ok && r.Key == key {
			return ports.ChangeRef(strconv.Itoa(p.Number)), nil
		}
	}
	rec, err := withFields(n.Record, func(f map[string]json.RawMessage) error {
		if raw, ok := f["item"]; ok {
			var have string
			if json.Unmarshal(raw, &have) != nil || have != item {
				return fmt.Errorf("github: the change record names item %s, not %s: %w", raw, item, ports.ErrConflict)
			}
		} else {
			f["item"], _ = json.Marshal(item)
		}
		if _, ok := f["n"]; !ok {
			if k, ok := h.branchN(item, n.Branch); ok {
				f["n"], _ = json.Marshal(k)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	marker, err := encodeMarker(rec, key)
	if err != nil {
		return "", err
	}
	if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/branches/%s", url.PathEscape(n.Branch)), nil, nil); err != nil {
		return "", err
	}
	title := strings.TrimSpace(untrusted.Read(n.Title))
	if title == "" {
		title = "agent-loop: #" + item
	}
	in := map[string]any{"title": title, "head": n.Branch, "base": base, "body": withMarker(untrusted.Read(n.Body), marker)}
	var p pullRequest
	if err := h.c.Send(ctx, asCoder, http.MethodPost, h.c.RepoPath("/pulls"), in, &p); err != nil {
		return "", err
	}
	return ports.ChangeRef(strconv.Itoa(p.Number)), nil
}

// withFields returns rec with its data's JSON object changed by fn.
func withFields(rec ports.Record, fn func(map[string]json.RawMessage) error) (ports.Record, error) {
	f := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(rec.Data))) > 0 {
		if err := json.Unmarshal(rec.Data, &f); err != nil {
			return rec, fmt.Errorf("github: record data is not a JSON object: %v: %w", err, ports.ErrConflict)
		}
	}
	if err := fn(f); err != nil {
		return rec, err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return rec, fmt.Errorf("github: record data: %v: %w", err, ports.ErrConflict)
	}
	rec.Data = data
	return rec, nil
}

// SetLabel sets or clears a marker's label on the pull request as the loop.
// It is set-to-value (work plan D18).
func (h *Host) SetLabel(ctx context.Context, ref ports.ChangeRef, m ports.Marker, on bool, _ ports.IdemKey) error {
	name, ok := h.labels[m]
	if !ok {
		return fmt.Errorf("github: marker %q has no label: %w", m, ports.ErrConflict)
	}
	p, err := h.pull(ctx, ref)
	if err != nil {
		return err
	}
	if p.hasLabel(name) == on {
		return nil
	}
	return setIssueLabel(ctx, h.c, p.Number, name, on)
}

// comparison is the part of GET /compare the host reads.
type comparison struct {
	AheadBy  int             `json:"ahead_by"`
	BehindBy int             `json:"behind_by"`
	Commits  []compareCommit `json:"commits"`
}

func (h *Host) compare(ctx context.Context, base, head string) (comparison, error) {
	var c comparison
	err := h.c.Get(ctx, asLoop, h.c.RepoPath("/compare/%s...%s", url.PathEscape(base), url.PathEscape(head)), nil, &c)
	return c, err
}

// ContainsBase reports whether the head contains the base tip: the compare
// of base...head is behind by nothing (§9.7).
func (h *Host) ContainsBase(ctx context.Context, ref ports.ChangeRef) (bool, error) {
	p, err := h.pull(ctx, ref)
	if err != nil {
		return false, err
	}
	c, err := h.compare(ctx, p.Base.Ref, p.Head.SHA)
	if err != nil {
		return false, err
	}
	return c.BehindBy == 0, nil
}

// Merge merges the pull request as the loop, pinned to the reviewed head
// (§9.7, I15, work plan D15).
func (h *Host) Merge(ctx context.Context, ref ports.ChangeRef, pinned ports.Commit, m ports.MergeMethod, _ ports.IdemKey) (ports.MergeOutcome, error) {
	p, err := h.pull(ctx, ref)
	if err != nil {
		return "", err
	}
	switch {
	case p.merged() && p.Head.SHA == string(pinned):
		return ports.MergeMerged, nil
	case p.merged(), p.State != "open":
		return ports.MergeNotMergeable, nil
	}
	if m == "" {
		m = ports.MergeMethodSquash
	}
	err = h.c.Send(ctx, asLoop, http.MethodPut, h.c.RepoPath("/pulls/%d/merge", p.Number),
		map[string]any{"sha": string(pinned), "merge_method": string(m)}, nil)
	return mergeOutcome(err)
}

// mergeOutcome maps the merge call's result (work plan D15).
func mergeOutcome(err error) (ports.MergeOutcome, error) {
	if err == nil {
		return ports.MergeMerged, nil
	}
	var se *StatusError
	if !errors.As(err, &se) || errors.Is(err, ports.ErrRateLimited) || se.Code >= 500 {
		return "", err
	}
	switch se.Code {
	case http.StatusConflict:
		return ports.MergeHeadMoved, nil
	case http.StatusMethodNotAllowed:
		return ports.MergeNotMergeable, nil
	}
	return ports.MergeFailed, nil
}
