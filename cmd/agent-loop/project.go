package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// The local project commands (project validate, run --dry-run) run in the
// CLI process with the deployment config, the profile and the SecretStore,
// without the server (TECHNICAL-SPEC §11).

// localProject is a registered project, loaded for a local command.
type localProject struct {
	name string
	dep  *profile.Deployment
	// prof is nil when the profile didn't load; profErr then says why.
	prof    *profile.Profile
	profErr error
	// ownerToken is the owner's login, held in memory only, when the
	// command has one (init, project validate --owner-login): the host
	// reads the rulesets' bypass lists with it and records them (work plan
	// #101 D2, D3). The loop never has it.
	ownerToken string
}

// projectAdapters are a project's ports, opened for a local command.
type projectAdapters struct {
	tracker ports.WorkTracker
	host    ports.CodeHost
	ids     profile.IdentityIDs
	close   func()
}

// openAdapters opens the project's tracker and code host, acting as the
// loop identity with read access only (work plan #23 D8). Problems are the
// identities it couldn't read; with any, the adapters are nil. Tests
// replace it.
var openAdapters = openGitHub

// projectFlags parses "[--config <path>] <project>" (work plan #23 D1).
// extra adds the command's own flags.
func projectFlags(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (cfg, project string, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg, "config", "", "the deployment config (default ~/.config/agent-loop/deployment.toml)")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return "", "", false
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "agent-loop %s: name one project\n", name)
		return "", "", false
	}
	return cfg, fs.Arg(0), true
}

// loadProject loads the deployment config and the project's profile. An
// error means the project can't be checked at all.
func loadProject(cfg, name string) (*localProject, error) {
	if cfg == "" {
		p, err := profile.DefaultDeploymentPath()
		if err != nil {
			return nil, err
		}
		cfg = p
	}
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no deployment config at %s: run agent-loop init, or pass --config", cfg)
	}
	dep, err := profile.LoadDeployment(cfg)
	if err != nil {
		return nil, err
	}
	reg, ok := dep.Projects[name]
	if !ok {
		names := dep.ProjectNames()
		if len(names) == 0 {
			return nil, fmt.Errorf("project %q isn't registered in %s, which has no projects: run agent-loop init", name, cfg)
		}
		return nil, fmt.Errorf("project %q isn't registered in %s (registered: %s)", name, cfg, strings.Join(names, ", "))
	}
	lp := &localProject{name: name, dep: dep}
	lp.prof, lp.profErr = profile.LoadProfile(reg.Profile, dep)
	return lp, nil
}

// readOnlyLoop is the loop token's scope for the local commands (work plan
// #23 D8).
var readOnlyLoop = map[string]string{
	"issues": "read", "pull_requests": "read", "contents": "read",
	"checks": "read", "statuses": "read", "administration": "read",
}

// githubAPI is the REST API's base URL; tests point it at a stub.
var githubAPI = ""

func openGitHub(ctx context.Context, lp *localProject) (*projectAdapters, []ports.Problem, error) {
	p := lp.prof
	if p.Tracker.Adapter != "github" || p.CodeHost.Adapter != "github" {
		return nil, nil, fmt.Errorf("adapters %q and %q: only github is supported", p.Tracker.Adapter, p.CodeHost.Adapter)
	}
	backend, err := secrets.Open(lp.dep.Secrets)
	if err != nil {
		return nil, nil, err
	}
	red := secrets.NewRedactor()
	apps, err := secrets.NewGitHubApps(backend, p.Identities, red)
	if err != nil {
		return nil, nil, err
	}
	apps.BaseURL = githubAPI
	var probs []ports.Problem
	acct := map[ports.Identity]secrets.AppAccount{}
	for _, id := range []ports.Identity{ports.IdentityCoder, ports.IdentityReviewer, ports.IdentityLoop} {
		a, err := apps.Account(ctx, id)
		if err != nil {
			probs = append(probs, ports.Problem{Severity: ports.SeverityError, Code: "identity_unreadable",
				Message: fmt.Sprintf("can't read the %s identity's GitHub App: %s", id, red.Redact(err.Error())),
				Hint:    fmt.Sprintf("store the %s app's app_id and private_key in the %s secret store, as agent-loop init does", id, lp.dep.Secrets.Backend)})
			continue
		}
		acct[id] = a
	}
	if len(probs) > 0 {
		return nil, probs, nil
	}
	run := secrets.NewRun(apps)
	lease, err := run.Issue(ctx, ports.IdentityLoop, ports.Scope{Repos: []string{p.Tracker.Repo}, Permissions: readOnlyLoop})
	if err != nil {
		_ = run.Close(context.WithoutCancel(ctx))
		return nil, []ports.Problem{{Severity: ports.SeverityError, Code: "identity_unreadable",
			Message: fmt.Sprintf("can't get a read token for the loop app on %s: %s", p.Tracker.Repo, red.Redact(err.Error())),
			Hint:    "check the loop app is installed on the repository"}}, nil
	}
	// The owner's last read of the rulesets, which the host compares the
	// loop's view with, is in the projection (work plan #101 D4).
	db, err := projection.Open(ctx, lp.dep.Projection.Path)
	if err != nil {
		_ = run.Close(context.WithoutCancel(ctx))
		return nil, []ports.Problem{{Severity: ports.SeverityError, Code: "projection_unavailable",
			Message: fmt.Sprintf("can't open the projection database, which holds the rulesets your login last read: %v", err),
			Hint:    "check projection.path in the deployment config"}}, nil
	}
	closeRun := func() {
		_ = run.Close(context.WithoutCancel(ctx))
		_ = db.Close()
	}
	var owner *github.Client
	if lp.ownerToken != "" {
		red.Add(lp.ownerToken, time.Time{})
		if owner, err = github.NewOwnerClient(githubAPI, p.CodeHost.Repo, lp.ownerToken); err != nil {
			closeRun()
			return nil, nil, err
		}
	}
	c, err := github.NewClient(github.ClientConfig{BaseURL: githubAPI, Repo: p.Tracker.Repo,
		Tokens: map[ports.Identity]github.TokenSource{ports.IdentityLoop: lease}})
	if err != nil {
		closeRun()
		return nil, nil, err
	}
	actor := func(id ports.Identity) ports.Actor {
		a := acct[id]
		return ports.Actor{ID: strconv.FormatInt(a.BotID, 10), Login: a.BotLogin, Kind: ports.ActorBot}
	}
	ids := github.Identities{Coder: actor(ports.IdentityCoder), Reviewer: actor(ports.IdentityReviewer), Loop: actor(ports.IdentityLoop)}
	labels := map[ports.Marker]string{}
	for m, mm := range p.Markers {
		if mm.Label != "" {
			labels[ports.Marker(m)] = mm.Label
		}
	}
	owners := make([]string, len(p.Identities.Owners))
	for i, o := range p.Identities.Owners {
		owners[i] = o.ID
	}
	base := p.BaseBranch
	if base == "" {
		var repo struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := c.Get(ctx, ports.IdentityLoop, c.RepoPath(""), nil, &repo); err != nil || repo.DefaultBranch == "" {
			closeRun()
			why := "the repository reported no default branch"
			if err != nil {
				why = red.Redact(err.Error())
			}
			return nil, []ports.Problem{{Severity: ports.SeverityError, Code: "repo_unreadable",
				Message: fmt.Sprintf("can't read %s's default branch: %s", p.Tracker.Repo, why),
				Hint:    "check the repository name and that the loop app is installed on it"}}, nil
		}
		base = repo.DefaultBranch
	}
	tr, err := github.NewTracker(c, github.TrackerConfig{Labels: labels, Identities: ids, BranchPrefix: p.BranchPrefix})
	if err != nil {
		closeRun()
		return nil, nil, err
	}
	host, err := github.NewHost(c, github.HostConfig{Labels: labels, Identities: ids, BaseBranch: base,
		BranchPrefix: p.BranchPrefix, Gates: p.Gates, ManualCommentBots: p.ManualCommentBots, Owners: owners,
		Apps:        github.AppIDs{Coder: acct[ports.IdentityCoder].AppID, Reviewer: acct[ports.IdentityReviewer].AppID, Loop: acct[ports.IdentityLoop].AppID},
		ReleaseApps: p.ReleaseApps, Permissions: apps,
		Rulesets: projection.Rulesets{DB: db, Project: lp.name}, OwnerClient: owner})
	if err != nil {
		closeRun()
		return nil, nil, err
	}
	return &projectAdapters{tracker: tr, host: host, close: closeRun, ids: profile.IdentityIDs{
		Coder: ids.Coder.ID, Reviewer: ids.Reviewer.ID, Loop: ids.Loop.ID}}, nil, nil
}
