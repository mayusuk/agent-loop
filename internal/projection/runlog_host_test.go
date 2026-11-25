package projection

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// I11, INTEGRATION-SPEC §9.8 step 6 (work plan #21 D3): the GitHub host
// reads the coder runs the worker records in the projection, so a coder
// run's push is the coder's and a push during another item's run is
// unknown.
func TestI11_GitHubHostReadsProjectionCoderRuns(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	runs := CoderRuns{DB: db, Project: "p"}

	srv := githubtest.New("acme", "widgets")
	t.Cleanup(srv.Close)
	const coderApp, reviewerApp, loopApp = 501, 502, 503
	owner := srv.AddUser(1001, "owner", "User")
	coder := srv.AddUser(9001, "project-coder[bot]", "Bot")
	reviewer := srv.AddUser(9002, "project-reviewer[bot]", "Bot")
	loop := srv.AddUser(9003, "project-loop[bot]", "Bot")
	for _, l := range github.DefaultLabels {
		srv.CreateLabel(l)
	}
	actor := func(u *githubtest.User) ports.Actor {
		k := ports.ActorHuman
		if u.Type == "Bot" {
			k = ports.ActorBot
		}
		return ports.Actor{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Kind: k}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	c, err := github.NewClient(github.ClientConfig{
		BaseURL: srv.URL,
		Repo:    "acme/widgets",
		Tokens: map[ports.Identity]github.TokenSource{
			ports.IdentityLoop:     github.StaticToken(loop.Token),
			ports.IdentityCoder:    github.StaticToken(coder.Token),
			ports.IdentityReviewer: github.StaticToken(reviewer.Token),
		},
		Now: srv.Now,
	})
	must(err)
	srv.SetPermission(owner, "admin")
	srv.SetPermission(reviewer, "read")
	srv.SetAppID(coder, coderApp)
	srv.SetAppID(reviewer, reviewerApp)
	srv.SetAppID(loop, loopApp)
	srv.AddRequiredRulesets("agent/", "main", coderApp, loopApp)
	cfg := github.HostConfig{
		Identities: github.Identities{Coder: actor(coder), Reviewer: actor(reviewer), Loop: actor(loop)},
		Owners:     []string{actor(owner).ID},
		Apps:       github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp},
		Runs:       runs,
		Rulesets:   Rulesets{DB: db, Project: "p"},
	}
	// init records the rulesets as the owner's login reads them (#101).
	oc, err := github.NewOwnerClient(srv.URL, "acme/widgets", owner.Token)
	must(err)
	ocfg := cfg
	ocfg.OwnerClient = oc
	oh, err := github.NewHost(c, ocfg)
	must(err)
	must(oh.RecordRulesets(ctx))
	host, err := github.NewHost(c, cfg)
	must(err)
	_, _, err = host.Changes(ctx, "")
	must(err)
	d := &githubtest.Driver{BaseURL: srv.URL, Repo: "acme/widgets", Owner: actor(owner), Tokens: map[string]string{}}
	for _, u := range []*githubtest.User{owner, coder, reviewer, loop} {
		d.Tokens[actor(u).ID] = u.Token
	}
	causes := func(ch ports.ChangeRef) string {
		t.Helper()
		r, err := host.Trust(ctx, ch)
		must(err)
		var out []string
		for _, c := range r.Causes {
			out = append(out, string(c))
		}
		return strings.Join(out, ",")
	}

	// Item 1's implement run pushes and opens the change.
	it, err := d.NewItem(actor(owner), "title", "body")
	must(err)
	branch := "agent/" + string(it)
	must(runs.Started(ctx, it, "run-1", srv.Now()))
	head, err := d.Push(actor(coder), branch)
	must(err)
	key := ports.IdemKey("open:" + branch)
	data, _ := json.Marshal(map[string]any{"type": "change", "v": 1, "key": key})
	ch, err := host.OpenChange(ctx, ports.NewChange{Item: it, Branch: branch, Base: "main",
		Record: ports.Record{Type: ports.RecordChange, Key: key, Data: data}}, ports.IdentityCoder, key)
	must(err)
	must(runs.Ended(ctx, it, "run-1", srv.Now(), head))
	if got := causes(ch); got != "" {
		t.Fatalf("push during a recorded coder run: causes %q, want none", got)
	}

	// A push to item 1's branch while only another item's run is live:
	// the coder token was live for someone else, so the pusher is unknown.
	must(runs.Started(ctx, "other", "run-2", srv.Now()))
	_, err = d.Push(actor(coder), branch)
	must(err)
	must(runs.Ended(ctx, "other", "run-2", srv.Now(), ""))
	if got := causes(ch); got != "pusher_unknown" {
		t.Fatalf("push during another item's run: causes %q, want pusher_unknown", got)
	}
}
