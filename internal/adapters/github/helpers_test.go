package github_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// stubProject is a fresh stub repository with the conformance actors.
type stubProject struct {
	srv                                      *githubtest.Server
	owner, human, bot, coder, reviewer, loop *githubtest.User
	client                                   *github.Client
	tracker                                  *github.Tracker
	host                                     *github.Host
	driver                                   *githubtest.Driver
	runs                                     *githubtest.RunLog
	records                                  *githubtest.RulesetRecords
	perms                                    staticPerms
}

// The identities' app IDs in the stub.
const (
	coderApp    = 501
	reviewerApp = 502
	loopApp     = 503
)

// staticPerms is a github.AppPermissions with fixed permissions.
type staticPerms map[ports.Identity]map[string]string

func (s staticPerms) InstalledPermissions(_ context.Context, id ports.Identity, _ string) (map[string]string, error) {
	p, ok := s[id]
	if !ok {
		return nil, fmt.Errorf("no installation for %s: %w", id, ports.ErrNotFound)
	}
	return p, nil
}

// goodPerms are the installed permissions INTEGRATION-SPEC §9.1 gives each
// app (work plan #16 D8).
func goodPerms() staticPerms {
	return staticPerms{
		ports.IdentityCoder:    {"contents": "write", "issues": "read", "pull_requests": "write", "metadata": "read"},
		ports.IdentityReviewer: {"contents": "read", "issues": "read", "pull_requests": "write", "metadata": "read"},
		ports.IdentityLoop: {"issues": "write", "pull_requests": "write", "contents": "write", "checks": "read",
			"statuses": "read", "administration": "read", "metadata": "read"},
	}
}

func actorOf(u *githubtest.User) ports.Actor {
	k := ports.ActorHuman
	if u.Type == "Bot" {
		k = ports.ActorBot
	}
	return ports.Actor{ID: itoa(u.ID), Login: u.Login, Kind: k}
}

func newStub(t *testing.T, pageSize int) *stubProject {
	t.Helper()
	return newStubWith(t, pageSize, true)
}

// newStubWith returns a stub project; labels says whether the loop's labels
// exist in it.
func newStubWith(t *testing.T, pageSize int, labels bool) *stubProject {
	t.Helper()
	srv := githubtest.New("acme", "widgets")
	t.Cleanup(srv.Close)
	p := &stubProject{
		srv:      srv,
		owner:    srv.AddUser(1001, "owner", "User"),
		human:    srv.AddUser(2001, "alice", "User"),
		bot:      srv.AddUser(3001, "scanner[bot]", "Bot"),
		coder:    srv.AddUser(9001, "project-coder[bot]", "Bot"),
		reviewer: srv.AddUser(9002, "project-reviewer[bot]", "Bot"),
		loop:     srv.AddUser(9003, "project-loop[bot]", "Bot"),
	}
	if labels {
		for _, l := range github.DefaultLabels {
			srv.CreateLabel(l)
		}
	}
	c, err := github.NewClient(github.ClientConfig{
		BaseURL: srv.URL,
		Repo:    "acme/widgets",
		Tokens: map[ports.Identity]github.TokenSource{
			ports.IdentityLoop:     github.StaticToken(p.loop.Token),
			ports.IdentityCoder:    github.StaticToken(p.coder.Token),
			ports.IdentityReviewer: github.StaticToken(p.reviewer.Token),
		},
		Now: srv.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.client = c
	p.tracker, err = github.NewTracker(c, github.TrackerConfig{
		Identities: github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)},
		PageSize:   pageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetPermission(p.owner, "admin")
	srv.SetPermission(p.reviewer, "read")
	srv.SetAppID(p.coder, coderApp)
	srv.SetAppID(p.reviewer, reviewerApp)
	srv.SetAppID(p.loop, loopApp)
	srv.AddRequiredRulesets("agent/", "main", coderApp, loopApp)
	p.runs = &githubtest.RunLog{Now: srv.Now}
	p.records = &githubtest.RulesetRecords{}
	p.perms = goodPerms()
	p.host, err = github.NewHost(c, github.HostConfig{
		Identities:        github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)},
		ManualCommentBots: []string{p.bot.Login},
		PageSize:          pageSize,
		Owners:            []string{itoa(p.owner.ID)},
		Apps:              github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp},
		Runs:              p.runs,
		Permissions:       p.perms,
		Rulesets:          p.records,
	})
	if err != nil {
		t.Fatal(err)
	}
	// init reads the rulesets with the owner's login and records them
	// (#101).
	p.recordRulesets(t)
	// The worker reads the feed when it starts, before any change exists.
	if _, _, err := p.host.Changes(ctx, ""); err != nil {
		t.Fatal(err)
	}
	srv.RequireChecks("ci")
	p.driver = &githubtest.Driver{BaseURL: srv.URL, Repo: "acme/widgets", Tokens: map[string]string{}, Owner: actorOf(p.owner), Runs: p.runs}
	for _, u := range []*githubtest.User{p.owner, p.human, p.bot, p.coder, p.reviewer, p.loop} {
		p.driver.Tokens[itoa(u.ID)] = u.Token
	}
	return p
}

// recordRulesets reads the rulesets with the owner's token and saves them
// as the records the loop's host compares with, as init and project
// validate --owner-login do (#101 D2).
func (p *stubProject) recordRulesets(t *testing.T) {
	t.Helper()
	if err := p.ownerHost(t).RecordRulesets(ctx); err != nil {
		t.Fatal(err)
	}
}

// ownerHost is a host like p.host that also has the owner's client, as
// init and project validate --owner-login build it.
func (p *stubProject) ownerHost(t *testing.T) *github.Host {
	t.Helper()
	oc, err := github.NewOwnerClient(p.srv.URL, "acme/widgets", p.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	h, err := github.NewHost(p.client, github.HostConfig{
		Identities:  github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)},
		Owners:      []string{itoa(p.owner.ID)},
		Apps:        github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp},
		Permissions: p.perms, Rulesets: p.records, OwnerClient: oc,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func hasMarker(ms []ports.Marker, m ports.Marker) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}
