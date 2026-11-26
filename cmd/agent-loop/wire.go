package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/notify"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/runners"
	"github.com/mayusuk/agent-loop/internal/secrets"
	"github.com/mayusuk/agent-loop/internal/workflows"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// onlyProject returns the one registered project. v0.1 runs one project
// (PRODUCT-SPEC §12; FR-15 is v0.3): the activities of a worker are named
// by method, so two projects' activities on one core queue would act on
// each other's repositories (work plan D8).
func onlyProject(dep *profile.Deployment) (string, *profile.Profile, error) {
	names := dep.ProjectNames()
	switch len(names) {
	case 0:
		return "", nil, errors.New("no project is registered in the deployment config: run agent-loop init in the repository")
	case 1:
	default:
		return "", nil, fmt.Errorf("projects %v are registered, but v0.1 runs one project per deployment (FR-15 is v0.3)", names)
	}
	profs, err := dep.LoadProjects()
	if err != nil {
		return "", nil, err
	}
	return names[0], profs[names[0]], nil
}

// githubSetup is what the GitHub adapters need beyond the profile: the API,
// a token source per identity, and the identities' accounts and apps.
type githubSetup struct {
	BaseURL     string // "" is api.github.com
	Tokens      map[ports.Identity]github.TokenSource
	Identities  github.Identities
	Apps        github.AppIDs
	Permissions github.AppPermissions
	Now         func() time.Time
}

// githubAdapters are one project's GitHub adapters and the coder-run store
// they share with the activities.
type githubAdapters struct {
	Tracker *github.Tracker
	Host    *github.Host
	// Runs is the one coder-run store: the activities record coder runs in
	// it and the host reads them to tell who pushed a head (INTEGRATION-SPEC
	// §9.8 step 6, I11, #21 D3).
	Runs projection.CoderRuns
}

func markerLabels(p *profile.Profile) map[ports.Marker]string {
	out := map[ports.Marker]string{}
	for m, mm := range p.Markers {
		if mm.Label != "" {
			out[ports.Marker(m)] = mm.Label
		}
	}
	return out
}

// newGitHubAdapters builds the project's tracker and code host. They share
// one client, and so one rate budget, when they are on one repository
// (INTEGRATION-SPEC §9.4).
func newGitHubAdapters(name string, p *profile.Profile, db *projection.DB, g githubSetup) (*githubAdapters, error) {
	if p.Tracker.Adapter != "github" || p.CodeHost.Adapter != "github" {
		return nil, fmt.Errorf("project %s: only the github tracker and code host are built in v0.1", name)
	}
	client := func(repo string) (*github.Client, error) {
		return github.NewClient(github.ClientConfig{BaseURL: g.BaseURL, Repo: repo, Tokens: g.Tokens, Now: g.Now})
	}
	hc, err := client(p.CodeHost.Repo)
	if err != nil {
		return nil, err
	}
	tc := hc
	if p.Tracker.Repo != p.CodeHost.Repo {
		if tc, err = client(p.Tracker.Repo); err != nil {
			return nil, err
		}
	}
	labels := markerLabels(p)
	tr, err := github.NewTracker(tc, github.TrackerConfig{Labels: labels, Identities: g.Identities, BranchPrefix: p.BranchPrefix})
	if err != nil {
		return nil, err
	}
	runs := projection.CoderRuns{DB: db, Project: name}
	var owners []string
	for _, o := range p.Identities.Owners {
		owners = append(owners, o.ID)
	}
	hcfg := github.HostConfig{
		Labels: labels, Identities: g.Identities, BaseBranch: p.BaseBranch, BranchPrefix: p.BranchPrefix,
		ManualCommentBots: p.ManualCommentBots, Owners: owners, Apps: g.Apps, ReleaseApps: p.ReleaseApps,
		Runs: runs, Permissions: g.Permissions, PollInterval: p.Intervals.Poll,
		// The owner's last read of the rulesets (work plan #101 D1, D4).
		Rulesets: projection.Rulesets{DB: db, Project: name},
	}
	if !p.GatesFromBase {
		hcfg.Gates = p.Gates
	}
	host, err := github.NewHost(hc, hcfg)
	if err != nil {
		return nil, err
	}
	return &githubAdapters{Tracker: tr, Host: host, Runs: runs}, nil
}

// activityConfig is the activities' view of the profile.
func activityConfig(name string, p *profile.Profile, ids github.Identities, requireApproval bool, runsDir string) workflows.Config {
	stages := map[contract.Stage]workflows.StageConfig{}
	for n, s := range p.Stages {
		if len(s.Command) > 0 {
			stages[contract.Stage(n)] = workflows.StageConfig{Command: s.Command, Runner: s.Runner, TimeLimit: s.TimeLimit}
		}
	}
	var owners []ports.OwnerAccount
	var ownerIDs []string
	for _, o := range p.Identities.Owners {
		owners = append(owners, ports.OwnerAccount{Login: o.Login, ID: o.ID})
		ownerIDs = append(ownerIDs, o.ID)
	}
	coder := ids.Coder
	return workflows.Config{
		Project: name, TrackerAdapter: p.Tracker.Adapter, CodeHostAdapter: p.CodeHost.Adapter,
		Repo:     ports.RepoRef{URL: "https://github.com/" + p.CodeHost.Repo + ".git", BaseBranch: p.BaseBranch},
		RepoName: p.CodeHost.Repo, BranchPrefix: p.BranchPrefix, BlockingThreshold: p.BlockingThreshold,
		RequireApproval: requireApproval, MergeMethod: ports.MergeMethod(p.MergeMethod), Stages: stages,
		Egress: p.Egress.Effective, Caches: p.Caches, Owners: owners,
		Identities: core.Identities{Owners: ownerIDs, Coder: ids.Coder.ID, Reviewer: ids.Reviewer.ID, Loop: ids.Loop.ID},
		// GitHub's noreply address of the coder app's bot (INTEGRATION-SPEC
		// §5.3, work plan D12).
		CoderSignature: workspace.Signature{Name: coder.Login, Email: coder.ID + "+" + coder.Login + "@users.noreply.github.com"},
		FollowUpType:   ports.Marker(profile.TypeMarkerPrefix + p.FollowUp.Type), FollowUpReady: p.FollowUp.Ready,
		RunsDir: runsDir,
	}
}

// projectConfig is the supervisor's view of the profile.
func projectConfig(name string, p *profile.Profile) workflows.ProjectConfig {
	stages := map[contract.Stage]core.StageLimits{}
	caps := map[contract.Stage][]string{}
	for n, s := range p.Stages {
		stages[contract.Stage(n)] = core.StageLimits{TimeLimit: s.TimeLimit, Attempts: s.Attempts}
		caps[contract.Stage(n)] = s.Capabilities
	}
	return workflows.ProjectConfig{
		Name: name,
		Item: workflows.ItemSettings{
			Project: name,
			Policy: core.Policy{MaxRounds: p.MaxRounds, ResumeRounds: p.ResumeRounds, BlockingThreshold: p.BlockingThreshold,
				MaxBaseUpdates: p.MaxBaseUpdates, Stages: stages},
			StageCaps: caps, PollInterval: p.Intervals.Poll,
		},
		Pick:      core.PickPolicy{Types: p.Types, Priority: p.Priority, WIP: p.WIP},
		DailyRuns: p.Limits.DailyAgentRuns, DailySpend: p.Limits.DailySpend, Timezone: p.TimezoneName,
		PollInterval: p.Intervals.Poll, SupervisorInterval: p.Intervals.Supervisor,
	}
}

// boardStageCaps are the stage capabilities the board judges capable-host
// waits by: the ones the project workflow gives its items, every stage
// included (work plan #162 D3).
func boardStageCaps(cfg workflows.ProjectConfig) map[string]map[contract.Stage][]string {
	return map[string]map[contract.Stage][]string{cfg.Name: cfg.Item.StageCaps}
}

// stageCaps are each stage's required capabilities, for the worker's queues.
func stageCaps(p *profile.Profile) map[contract.Stage][]string {
	out := map[contract.Stage][]string{}
	for n, s := range p.Stages {
		if len(s.Command) > 0 {
			out[contract.Stage(n)] = s.Capabilities
		}
	}
	return out
}

// clientScope is the token the GitHub client acts with for an identity.
// The coder's is the "Coder, worker" row without contents: write: it opens
// changes and replies but can't push, so no coder push token is live
// outside a recorded coder run (I11, work plan D14).
func clientScope(id ports.Identity, repo string) (ports.Scope, error) {
	use := map[ports.Identity]secrets.Use{
		ports.IdentityLoop: secrets.UseLoop, ports.IdentityCoder: secrets.UseCoderWorker, ports.IdentityReviewer: secrets.UseReviewerWorker,
	}[id]
	_, s, err := secrets.ScopeFor(use, repo)
	if err != nil {
		return s, err
	}
	if id == ports.IdentityCoder {
		s.Permissions["contents"] = "read"
	}
	return s, nil
}

// resolveGitHub issues the client's token leases and finds each identity's
// app and bot account (work plan D12). The leases renew themselves; tokens
// closes them.
func resolveGitHub(ctx context.Context, store *secrets.GitHubApps, repo string) (githubSetup, *secrets.Run, error) {
	tokens := secrets.NewRun(store)
	g := githubSetup{Tokens: map[ports.Identity]github.TokenSource{}, Permissions: store}
	for _, id := range []ports.Identity{ports.IdentityLoop, ports.IdentityCoder, ports.IdentityReviewer} {
		s, err := clientScope(id, repo)
		if err != nil {
			return g, tokens, err
		}
		l, err := tokens.Issue(ctx, id, s)
		if err != nil {
			return g, tokens, fmt.Errorf("%s token: %w", id, err)
		}
		g.Tokens[id] = l
	}
	c, err := github.NewClient(github.ClientConfig{Repo: repo, Tokens: g.Tokens})
	if err != nil {
		return g, tokens, err
	}
	for _, id := range []ports.Identity{ports.IdentityLoop, ports.IdentityCoder, ports.IdentityReviewer} {
		app, err := store.App(ctx, id)
		if err != nil {
			return g, tokens, err
		}
		var u struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		}
		if err := c.Get(ctx, ports.IdentityLoop, "/users/"+url.PathEscape(app.Slug+"[bot]"), nil, &u); err != nil {
			return g, tokens, fmt.Errorf("the %s app's bot account: %w", id, err)
		}
		a := ports.Actor{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Kind: ports.ActorBot}
		switch id {
		case ports.IdentityLoop:
			g.Identities.Loop, g.Apps.Loop = a, app.ID
		case ports.IdentityCoder:
			g.Identities.Coder, g.Apps.Coder = a, app.ID
		case ports.IdentityReviewer:
			g.Identities.Reviewer, g.Apps.Reviewer = a, app.ID
		}
	}
	return g, tokens, nil
}

// newActivities builds the worker's activities for the project, with the
// coder-run store shared with the host.
func newActivities(ctx context.Context, dep *profile.Deployment, name string, p *profile.Profile, db *projection.DB,
	gh *githubAdapters, g githubSetup, store ports.SecretStore, red *secrets.Redactor) (*workflows.Activities, error) {
	approvals, err := gh.Host.RequiredApprovals(ctx)
	if err != nil {
		return nil, err
	}
	git, err := workspace.New("")
	if err != nil {
		return nil, err
	}
	// The fetch uses the client's coder token, which reads contents and
	// can't push (work plan D16).
	if coder := g.Tokens[ports.IdentityCoder]; coder != nil {
		git.Auth = func(ctx context.Context, _ ports.RepoRef) (ports.Token, error) { return coder.Token(ctx) }
	}
	runner, err := runners.NewLocal(dep, red)
	if err != nil {
		return nil, err
	}
	note, warnings, err := notify.New(dep.Notifiers, notify.Options{})
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "agent-loop worker:", w)
	}
	runsDir := filepath.Join(git.Root, "results")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, err
	}
	a := projectActivities(activityConfig(name, p, g.Identities, approvals > 0, runsDir), gh, db)
	a.Workspace, a.Runner, a.Secrets, a.Git, a.Notifier, a.Redactor = git, runner, store, git, note, red
	return a, nil
}

// projectActivities are the activities' adapters and stores: the coder
// runs they record go to the store the host reads (I11, #21 D3).
func projectActivities(cfg workflows.Config, gh *githubAdapters, db *projection.DB) *workflows.Activities {
	return &workflows.Activities{Config: cfg, Tracker: gh.Tracker, Host: gh.Host, CoderRuns: gh.Runs, Projection: db}
}
