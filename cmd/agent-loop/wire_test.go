package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

const testProfile = `
base_branch = "main"

[tracker]
repo = "acme/widgets"

[code_host]
repo = "acme/widgets"

[identities]
coder    = "secret://github-app/coder"
reviewer = "secret://github-app/reviewer"
loop     = "secret://github-app/loop"
owners   = [{ login = "owner", id = 1001 }]

[stages.implement]
command  = ["agent-loop-claude", "implement"]
contract = "1.0"

[stages.review]
command  = ["agent-loop-claude", "review"]
contract = "1.0"

[stages.address]
command  = ["agent-loop-claude", "address"]
contract = "1.0"
`

// writeDeployment writes a deployment config registering the profiles,
// and returns its path.
func writeDeployment(t *testing.T, profiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	body := "[projection]\npath = \"" + filepath.Join(dir, "projection.db") + "\"\n[secrets]\nbackend = \"envfile\"\npath = \"" + filepath.Join(dir, "secrets.env") + "\"\n"
	for name, prof := range profiles {
		p := filepath.Join(dir, name+".toml")
		if err := os.WriteFile(p, []byte(prof), 0o600); err != nil {
			t.Fatal(err)
		}
		body += "[projects." + name + "]\nprofile = \"" + p + "\"\n"
	}
	cfg := filepath.Join(dir, "deployment.toml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// I11, INTEGRATION-SPEC §9.8 step 6, #21 D3: the worker's composition
// passes one projection.CoderRuns as the activities' CoderRuns and the
// GitHub host's HostConfig.Runs, so a push the coder makes during a coder
// run the activities recorded is the coder's, not pusher_unknown.
func TestI11_WorkerHostAttributesCoderPushToCoder(t *testing.T) {
	ctx := context.Background()
	dep, err := profile.LoadDeployment(writeDeployment(t, map[string]string{"widgets": testProfile}))
	if err != nil {
		t.Fatal(err)
	}
	name, prof, err := onlyProject(dep)
	if err != nil {
		t.Fatal(err)
	}
	db, err := projection.Open(ctx, dep.Projection.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv := githubtest.New("acme", "widgets")
	t.Cleanup(srv.Close)
	const coderApp, reviewerApp, loopApp = 501, 502, 503
	owner := srv.AddUser(1001, "owner", "User")
	coder := srv.AddUser(9001, "widgets-coder[bot]", "Bot")
	reviewer := srv.AddUser(9002, "widgets-reviewer[bot]", "Bot")
	loop := srv.AddUser(9003, "widgets-loop[bot]", "Bot")
	for _, l := range github.DefaultLabels {
		srv.CreateLabel(l)
	}
	srv.SetPermission(owner, "admin")
	srv.SetPermission(reviewer, "read")
	srv.SetAppID(coder, coderApp)
	srv.SetAppID(reviewer, reviewerApp)
	srv.SetAppID(loop, loopApp)
	srv.AddRequiredRulesets("agent/", "main", coderApp, loopApp)
	actor := func(u *githubtest.User) ports.Actor {
		k := ports.ActorHuman
		if u.Type == "Bot" {
			k = ports.ActorBot
		}
		return ports.Actor{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Kind: k}
	}
	setup := githubSetup{
		BaseURL: srv.URL,
		Tokens: map[ports.Identity]github.TokenSource{
			ports.IdentityLoop: github.StaticToken(loop.Token), ports.IdentityCoder: github.StaticToken(coder.Token),
			ports.IdentityReviewer: github.StaticToken(reviewer.Token),
		},
		Identities: github.Identities{Coder: actor(coder), Reviewer: actor(reviewer), Loop: actor(loop)},
		Apps:       github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp},
		Now:        srv.Now,
	}
	gh, err := newGitHubAdapters(name, prof, db, setup)
	if err != nil {
		t.Fatal(err)
	}
	// The activities record coder runs in the store the host reads.
	cfg := activityConfig(name, prof, setup.Identities, false, t.TempDir())
	if cfg.Identities.Coder != "9001" || cfg.CoderSignature.Email != "9001+widgets-coder[bot]@users.noreply.github.com" {
		t.Fatalf("activity config %+v", cfg)
	}
	acts := projectActivities(cfg, gh, db)
	if acts.Host != ports.CodeHost(gh.Host) {
		t.Fatal("the activities use another host")
	}
	recorder := acts.CoderRuns

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// init's validate records the rulesets as the owner's login reads
	// them, in the projection the worker's host reads (#101 D4).
	oc, err := github.NewOwnerClient(srv.URL, "acme/widgets", owner.Token)
	must(err)
	oh, err := github.NewHost(oc, github.HostConfig{Identities: setup.Identities, Apps: setup.Apps,
		Rulesets: projection.Rulesets{DB: db, Project: name}, OwnerClient: oc})
	must(err)
	must(oh.RecordRulesets(ctx))
	_, _, err = gh.Host.Changes(ctx, "")
	must(err)
	d := &githubtest.Driver{BaseURL: srv.URL, Repo: "acme/widgets", Owner: actor(owner), Tokens: map[string]string{}}
	for _, u := range []*githubtest.User{owner, coder, reviewer, loop} {
		d.Tokens[actor(u).ID] = u.Token
	}
	causes := func(ch ports.ChangeRef) string {
		t.Helper()
		r, err := gh.Host.Trust(ctx, ch)
		must(err)
		var out []string
		for _, c := range r.Causes {
			out = append(out, string(c))
		}
		return strings.Join(out, ",")
	}

	it, err := d.NewItem(actor(owner), "title", "body")
	must(err)
	branch := "agent/" + string(it)
	must(recorder.Started(ctx, it, "run-1", srv.Now()))
	head, err := d.Push(actor(coder), branch)
	must(err)
	key := ports.IdemKey("open:" + branch)
	data, _ := json.Marshal(map[string]any{"type": "change", "v": 1, "key": key})
	ch, err := gh.Host.OpenChange(ctx, ports.NewChange{Item: it, Branch: branch, Base: "main",
		Record: ports.Record{Type: ports.RecordChange, Key: key, Data: data}}, ports.IdentityCoder, key)
	must(err)
	must(recorder.Ended(ctx, it, "run-1", srv.Now(), head))
	if got := causes(ch); got != "" {
		t.Fatalf("coder push during a recorded coder run: causes %q, want none (the coder's push)", got)
	}

	// A push to the branch while only another item's coder run is live:
	// the coder token was live for someone else, so the pusher is unknown.
	// The host reads the activities' records, not just the pushing account.
	must(recorder.Started(ctx, "other", "run-2", srv.Now()))
	_, err = d.Push(actor(coder), branch)
	must(err)
	must(recorder.Ended(ctx, "other", "run-2", srv.Now(), ""))
	if got := causes(ch); got != "pusher_unknown" {
		t.Fatalf("push during another item's coder run: causes %q, want pusher_unknown", got)
	}
}

// Work plan D8: v0.1 runs one project.
func TestOnlyProject(t *testing.T) {
	dep, err := profile.LoadDeployment(writeDeployment(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := onlyProject(dep); err == nil || !strings.Contains(err.Error(), "agent-loop init") {
		t.Fatalf("no project: %v", err)
	}
	dep, err = profile.LoadDeployment(writeDeployment(t, map[string]string{"a": testProfile, "b": testProfile}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := onlyProject(dep); err == nil || !strings.Contains(err.Error(), "FR-15") {
		t.Fatalf("two projects: %v", err)
	}
}

// Work plan D14: the client's coder token can't push.
func TestI11_ClientCoderTokenCantPush(t *testing.T) {
	s, err := clientScope(ports.IdentityCoder, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if s.Permissions["contents"] != "read" || s.Permissions["pull_requests"] != "write" {
		t.Fatalf("coder client scope %v", s.Permissions)
	}
	if l, _ := clientScope(ports.IdentityLoop, "acme/widgets"); l.Permissions["issues"] != "write" {
		t.Fatalf("loop scope %v", l.Permissions)
	}
}

func TestWorkerAndServerStartArguments(t *testing.T) {
	for _, args := range [][]string{{"worker"}, {"worker", "--capabilities", " , "}, {"worker", "--capabilities", "agent", "extra"}, {"server", "start", "x"}, {"server"}, {"server", "stop"}} {
		if code, _, _ := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	cfg := writeDeployment(t, nil)
	for _, args := range [][]string{{"worker", "--capabilities", "agent", "--config", cfg}, {"server", "start", "--config", cfg}} {
		if code, _, errs := cli(t, args...); code != 1 || !strings.Contains(errs, "no project is registered") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	if got := parseCapabilities("docker, agent,agent,"); strings.Join(got, ",") != "agent,docker" {
		t.Fatalf("capabilities %v", got)
	}
}

// The supervisor gets the profile's settings (TECHNICAL-SPEC §6.2, §13).
func TestProjectConfigFromProfile(t *testing.T) {
	dep, err := profile.LoadDeployment(writeDeployment(t, map[string]string{"widgets": "wip = 3\n" + testProfile}))
	if err != nil {
		t.Fatal(err)
	}
	name, prof, err := onlyProject(dep)
	if err != nil {
		t.Fatal(err)
	}
	c := projectConfig(name, prof)
	if c.Name != "widgets" || c.Item.Project != "widgets" || c.Pick.WIP != 3 || c.DailyRuns != 20 ||
		c.PollInterval != 60*time.Second || c.Item.Policy.MaxRounds != 3 || c.Item.Policy.Stages["implement"].Attempts != 2 {
		t.Fatalf("project config %+v", c)
	}
	if q := workflows.WorkerQueues([]string{"agent"}, stageCaps(prof)); strings.Join(q, ",") != "core,cap:agent" {
		t.Fatalf("queues %v", q)
	}
}

// TestActivityConfigCarriesTheProfilesCaches: the profile's caches reach
// the activities' config, which puts them in every run's RunSpec
// (TECHNICAL-SPEC §7.2, work plan #97 D1). With none, the list is empty.
func TestActivityConfigCarriesTheProfilesCaches(t *testing.T) {
	for _, tc := range []struct {
		top  string
		want []string
	}{
		{top: `caches = ["BUNDLE_PATH", "YARN_CACHE_FOLDER"]` + "\n", want: []string{"BUNDLE_PATH", "YARN_CACHE_FOLDER"}},
		{top: "", want: []string{}},
	} {
		dep, err := profile.LoadDeployment(writeDeployment(t, map[string]string{"widgets": tc.top + testProfile}))
		if err != nil {
			t.Fatal(err)
		}
		name, prof, err := onlyProject(dep)
		if err != nil {
			t.Fatal(err)
		}
		cfg := activityConfig(name, prof, github.Identities{}, false, t.TempDir())
		if cfg.Caches == nil || !slices.Equal(cfg.Caches, tc.want) {
			t.Errorf("%q: Caches = %#v, want %#v", tc.top, cfg.Caches, tc.want)
		}
	}
}
