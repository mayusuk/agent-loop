package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// ownerSecret is the owner's login token in the init tests. It must never
// reach the disk.
const ownerSecret = "gho_OwnerLoginTokenNeverStored42"

// initEnv is a repository checkout, a deployment config with the env-file
// secret store, and a stub GitHub that knows the owner by ownerSecret.
type initEnv struct {
	dir, home, repo, cfg, secrets string
	srv                           *githubtest.Server
	owner                         *githubtest.User
	browser                       *stubBrowser
}

// stubBrowser stands in for the owner's browser: it confirms each app in
// the stub, and installs it.
type stubBrowser struct {
	srv              *githubtest.Server
	created, install []string
	noInstall        bool
}

func (b *stubBrowser) ManifestCode(_ context.Context, createURL string, build func(string) ([]byte, error)) (string, error) {
	data, err := build("http://127.0.0.1:1/callback")
	if err != nil {
		return "", err
	}
	b.created = append(b.created, createURL)
	return b.srv.CreateFromManifest(data)
}

func (b *stubBrowser) Install(_ context.Context, installURL, slug string) error {
	b.install = append(b.install, installURL)
	if !b.noInstall {
		b.srv.InstallApp(slug)
	}
	return nil
}

func newInitEnv(t *testing.T) *initEnv {
	t.Helper()
	dir := t.TempDir()
	e := &initEnv{dir: dir, home: filepath.Join(dir, "home"), repo: filepath.Join(dir, "widgets"),
		cfg: filepath.Join(dir, "home", ".config", "agent-loop", "deployment.toml"), secrets: filepath.Join(dir, "home", "secrets.env")}
	for _, d := range []string{e.repo, filepath.Dir(e.cfg), filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", e.home)
	if err := os.WriteFile(filepath.Join(dir, "bin", "agent-loop-claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(e.cfg, []byte("[secrets]\nbackend = \"envfile\"\npath = \""+e.secrets+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv = githubtest.New("acme", "widgets")
	t.Cleanup(e.srv.Close)
	owner := e.srv.AddUser(1001, "owner", "User")
	e.srv.SetPermission(owner, "admin")
	e.owner = owner
	e.srv.RequireClassicChecks("test")
	e.browser = &stubBrowser{srv: e.srv}

	oldIn, oldTok, oldRemote, oldBrowser, oldClaude := stdin, ownerToken, gitRemote, newBrowser, claudeCheck
	oldWeb, oldWait, oldPoll, oldAPI, oldOpen := githubWeb, installWait, installPoll, githubAPI, openAdapters
	oldBackend, oldSig := openBackend, runningSignature
	t.Cleanup(func() {
		stdin, ownerToken, gitRemote, newBrowser, claudeCheck = oldIn, oldTok, oldRemote, oldBrowser, oldClaude
		githubWeb, installWait, installPoll, githubAPI, openAdapters = oldWeb, oldWait, oldPoll, oldAPI, oldOpen
		openBackend, runningSignature = oldBackend, oldSig
	})
	// The tests never touch the login keychain: the Keychain backend gets
	// a keychain file that doesn't exist, which holds no items.
	openBackend = func(s profile.Secrets) (secrets.Backend, error) {
		if s.Backend == profile.SecretsKeychain {
			return &secrets.Keychain{Path: filepath.Join(dir, "none.keychain-db")}, nil
		}
		return secrets.Open(s)
	}
	runningSignature = func(context.Context) (secrets.CodeSignature, error) {
		return secrets.CodeSignature{Signed: true, Requirement: `identifier "dev.agent-loop"`}, nil
	}
	stdin = strings.NewReader("")
	e.srv.SetToken(owner, ownerSecret)
	ownerToken = func(context.Context, *initOptions) (string, error) { return ownerSecret, nil }
	gitRemote = func(string) (string, error) { return "git@github.com:acme/widgets.git", nil }
	newBrowser = func(io.Writer) appBrowser { return e.browser }
	claudeCheck = func(context.Context) []string { return nil }
	installWait, installPoll = 50*time.Millisecond, time.Millisecond
	githubAPI = e.srv.URL
	openAdapters = openGitHub
	return e
}

func (e *initEnv) run(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	stdin = strings.NewReader(input)
	return runCLI(append([]string{"init", "--config", e.cfg, "--repo-dir", e.repo}, args...)...)
}

// writes are the stub calls that change GitHub, leaving out installation
// tokens, which validate mints and revokes.
func writes(srv *githubtest.Server, from int) []string {
	var out []string
	for _, r := range srv.Requests()[from:] {
		if r.Method == "GET" || strings.HasSuffix(r.Path, "/access_tokens") || r.Path == "/installation/token" {
			continue
		}
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// snapshot reads every file under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		out[p] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFR1AC5_FirstRunMakesEveryChangeSecondRunNone: with --yes, a first
// run makes every change of INTEGRATION-SPEC §12.2 and ends with a clean
// validate; a second run changes nothing on GitHub or on disk.
func TestFR1AC5_FirstRunMakesEveryChangeSecondRunNone(t *testing.T) {
	e := newInitEnv(t)
	code, out, errOut := e.run(t, "", "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	// Step 2: three apps, created with their permissions and installed.
	if len(e.browser.created) != 3 || len(e.browser.install) != 3 || e.browser.created[0] != githubWeb+"/settings/apps/new" {
		t.Errorf("browser %+v", e.browser)
	}
	for _, id := range []string{"coder", "reviewer", "loop"} {
		a := e.srv.AppBySlug("widgets-" + id)
		if a == nil || a.Installation == 0 {
			t.Fatalf("app widgets-%s: %+v", id, a)
		}
	}
	sec, _ := os.ReadFile(e.secrets)
	for _, k := range []string{"GITHUB_APP_WIDGETS_CODER_APP_ID", "GITHUB_APP_WIDGETS_REVIEWER_PRIVATE_KEY", "GITHUB_APP_WIDGETS_LOOP_PRIVATE_KEY"} {
		if !strings.Contains(string(sec), "AGENT_LOOP_SECRET_"+k+"=") {
			t.Errorf("secret %s not stored", k)
		}
	}
	// Step 3: the profile loads and is registered.
	dep, err := profile.LoadDeployment(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := dep.Projects["widgets"]
	if !ok || reg.Profile != filepath.Join(e.repo, ".agent-loop.toml") {
		t.Fatalf("registration %+v", dep.Projects)
	}
	p, err := profile.LoadProfile(reg.Profile, dep)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tracker.Repo != "acme/widgets" || p.BaseBranch != "main" || p.Identities.Coder != "secret://github-app/widgets/coder" ||
		len(p.Identities.Owners) != 1 || p.Identities.Owners[0].ID != "1001" || p.Stages["review"].Command[1] != "review" {
		t.Errorf("profile %+v", p)
	}
	// Step 4: every label of the marker mapping.
	want := []string{"aborted", "blocked", "bug", "feature", "follow-up", "in-progress", "needs-owner", "paused", "plan", "plan-proposal", "ready"}
	if got := e.srv.Labels(); !slices.Equal(got, want) {
		t.Errorf("labels %v", got)
	}
	// Step 5: the three rulesets, the optional fourth, and up to date.
	if n := len(e.srv.Rulesets()); n != 4 {
		t.Errorf("%d rulesets", n)
	}
	if !e.srv.UpToDate() {
		t.Error("up to date not turned on")
	}
	// Steps 7–9.
	for _, s := range []string{"required checks on main: test", "widgets: 0 error(s)", "agent-loop server start"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}
	if strings.Contains(out, "ruleset_") || strings.Contains(out, "label_missing") || strings.Contains(out, "app_permission") {
		t.Errorf("validate found init's work lacking:\n%s", out)
	}

	before := snapshot(t, e.dir)
	from := len(e.srv.Requests())
	code, out, errOut = e.run(t, "", "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("second run: code %d\n%s\n%s", code, out, errOut)
	}
	if w := writes(e.srv, from); len(w) != 0 {
		t.Errorf("second run wrote to GitHub: %v", w)
	}
	if strings.Contains(out, "   + ") {
		t.Errorf("second run proposed changes:\n%s", out)
	}
	if len(e.browser.created) != 3 || len(e.browser.install) != 3 {
		t.Errorf("second run opened the browser: %+v", e.browser)
	}
	after := snapshot(t, e.dir)
	if len(before) != len(after) {
		t.Errorf("files %d -> %d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("second run changed %s", p)
		}
	}
	if code, out, _ := e.run(t, "", "--check"); code != 0 || !strings.Contains(out, "nothing to change") {
		t.Errorf("--check after init: code %d\n%s", code, out)
	}
}

// TestFR1AC5_CheckChangesNothingAndReportsDrift: --check on a fresh
// repository writes nothing, opens no browser, lists every change and
// exits 1; after drift (a deleted label and ruleset) it reports only that.
func TestFR1AC5_CheckChangesNothingAndReportsDrift(t *testing.T) {
	e := newInitEnv(t)
	before := snapshot(t, e.dir)
	code, out, errOut := e.run(t, "", "--check")
	if code != 1 || errOut != "" {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	if w := writes(e.srv, 0); len(w) != 0 {
		t.Errorf("--check wrote to GitHub: %v", w)
	}
	if len(e.browser.created)+len(e.browser.install) != 0 {
		t.Errorf("--check opened the browser")
	}
	after := snapshot(t, e.dir)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("--check changed files")
	}
	for _, s := range []string{"+ create the GitHub App widgets-coder", "+ write " + filepath.Join(e.repo, ".agent-loop.toml"),
		"+ register project widgets", `+ create label "ready"`, "the rulesets need the coder and loop apps",
		"+ turn on \"require branches to be up to date", "change(s) to make"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}

	if code, out, _ := e.run(t, "", "--yes"); code != 0 {
		t.Fatalf("init: %d\n%s", code, out)
	}
	c, err := github.NewOwnerClient(e.srv.URL, "acme/widgets", ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, rs := range e.srv.Rulesets() {
		if rs.Name == github.RulesetNameTags {
			if err := c.Send(context.Background(), github.AsOwner, "DELETE", c.RepoPath("/rulesets/%d", rs.ID), nil, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.srv.DeleteLabel("paused")
	from := len(e.srv.Requests())
	code, out, _ = e.run(t, "", "--check")
	if code != 1 || !strings.Contains(out, `+ create label "paused"`) || !strings.Contains(out, `+ create ruleset "agent-loop: tags"`) ||
		!strings.Contains(out, "2 change(s) to make") {
		t.Errorf("code %d\n%s", code, out)
	}
	if w := writes(e.srv, from); len(w) != 0 {
		t.Errorf("--check wrote to GitHub: %v", w)
	}
	if code, out, _ = e.run(t, "", "--yes"); code != 0 || !strings.Contains(out, `+ create label "paused"`) {
		t.Errorf("repair: code %d\n%s", code, out)
	}
	if code, _, _ = e.run(t, "", "--check"); code != 0 {
		t.Errorf("--check after repair: code %d", code)
	}
}

// TestFR1AC5_OwnerTokenNeverWrittenToDisk: after a full run, the owner's
// login token is in no file under the home directory, the repository or
// the secret store, and in nothing init printed.
func TestFR1AC5_OwnerTokenNeverWrittenToDisk(t *testing.T) {
	e := newInitEnv(t)
	code, out, errOut := e.run(t, "", "--yes")
	if code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	files := snapshot(t, e.dir)
	if len(files) < 3 {
		t.Fatalf("expected the profile, deployment config and secrets: %v", files)
	}
	for p, v := range files {
		if strings.Contains(v, ownerSecret) {
			t.Errorf("%s holds the owner's token", p)
		}
	}
	if strings.Contains(out+errOut, ownerSecret) {
		t.Error("init printed the owner's token")
	}
	// Only the owner's own calls carry it: the loop never uses it.
	for _, r := range e.srv.Requests() {
		if r.Login == "owner" && strings.Contains(r.Path, "/access_tokens") {
			t.Errorf("owner token used for %s", r.Path)
		}
	}
}

// TestFR1AC5_AsksBeforeEachChange: without --yes, each step asks; a "no"
// skips it, and init says it didn't finish.
func TestFR1AC5_AsksBeforeEachChange(t *testing.T) {
	e := newInitEnv(t)
	code, out, _ := e.run(t, strings.Repeat("n\n", 20))
	if code != 1 || !strings.Contains(out, "[y/N]") || !strings.Contains(out, "skipped") || !strings.Contains(out, "run agent-loop init again to finish") {
		t.Errorf("code %d\n%s", code, out)
	}
	if w := writes(e.srv, 0); len(w) != 0 {
		t.Errorf("declined steps wrote: %v", w)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".agent-loop.toml")); err == nil {
		t.Error("declined profile was written")
	}
	// Answer yes to the apps, choose a custom review command, name no
	// gates, and accept the rest.
	in := "y\ny\ny\n\n./bin/review\n\ny\ny\ny\ny\ny\ny\ny\n"
	code, out, _ = e.run(t, in)
	if code != 1 && code != 0 {
		t.Fatalf("code %d", code)
	}
	p, err := profile.LoadProfile(filepath.Join(e.repo, ".agent-loop.toml"), profile.DefaultDeployment())
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !slices.Equal(p.Stages["review"].Command, []string{"./bin/review"}) || p.Stages["implement"].Command[0] != "agent-loop-claude" {
		t.Errorf("stages %+v", p.Stages)
	}
}

// TestFR1AC5_StopsWithoutAdminOrRulesets: step 1 stops and says why.
func TestFR1AC5_StopsWithoutAdminOrRulesets(t *testing.T) {
	t.Run("NotAdmin", func(t *testing.T) {
		e := newInitEnv(t)
		e.srv.SetPermission(e.owner, "write")
		code, _, errOut := e.run(t, "", "--yes")
		if code != 1 || !strings.Contains(errOut, "isn't an admin of acme/widgets") {
			t.Errorf("code %d, %s", code, errOut)
		}
		if w := writes(e.srv, 0); len(w) != 0 {
			t.Errorf("wrote %v", w)
		}
	})
	t.Run("NoRulesets", func(t *testing.T) {
		e := newInitEnv(t)
		e.srv.Fail("GET", `/rulesets$`, 403, nil, `{"message":"Upgrade"}`, 0)
		code, _, errOut := e.run(t, "", "--yes")
		if code != 1 || !strings.Contains(errOut, "rulesets need a public repository or a paid plan") {
			t.Errorf("code %d, %s", code, errOut)
		}
	})
	t.Run("NotGitHub", func(t *testing.T) {
		e := newInitEnv(t)
		gitRemote = func(string) (string, error) { return "https://gitlab.com/a/b.git", nil }
		if code, _, errOut := e.run(t, "", "--yes"); code != 1 || !strings.Contains(errOut, "isn't a GitHub repository") {
			t.Errorf("code %d, %s", code, errOut)
		}
	})
	t.Run("NameTaken", func(t *testing.T) {
		e := newInitEnv(t)
		f, _ := os.OpenFile(e.cfg, os.O_APPEND|os.O_WRONLY, 0)
		fmt.Fprintf(f, "\n[projects.widgets]\nprofile = %q\n", filepath.Join(e.dir, "other", ".agent-loop.toml"))
		f.Close()
		if code, _, errOut := e.run(t, "", "--yes"); code != 1 || !strings.Contains(errOut, "pass --name") {
			t.Errorf("code %d, %s", code, errOut)
		}
	})
	t.Run("Flags", func(t *testing.T) {
		e := newInitEnv(t)
		if code, _, errOut := e.run(t, "", "--yes", "--check"); code != 2 || !strings.Contains(errOut, "not both") {
			t.Errorf("code %d, %s", code, errOut)
		}
	})
}

// TestFR1AC5_NewDeploymentConfigAndOrgApps: with no deployment config,
// init creates one holding only the registration; an organisation's apps
// are created under the organisation.
func TestFR1AC5_NewDeploymentConfigAndOrgApps(t *testing.T) {
	e := newInitEnv(t)
	e.srv.SetOwnerType("Organization")
	if err := os.Remove(e.cfg); err != nil {
		t.Fatal(err)
	}
	// Without the env-file config the default backend is used; check mode
	// keeps this test off the Keychain.
	code, out, _ := e.run(t, "", "--check")
	if code != 1 || !strings.Contains(out, "+ register project widgets in "+e.cfg) {
		t.Errorf("code %d\n%s", code, out)
	}
	if _, err := os.Stat(e.cfg); err == nil {
		t.Error("--check created the deployment config")
	}
	if err := registerProject(e.cfg, false, "widgets", "/x/.agent-loop.toml"); err != nil {
		t.Fatal(err)
	}
	dep, err := profile.LoadDeployment(e.cfg)
	if err != nil || dep.Projects["widgets"].Profile != "/x/.agent-loop.toml" {
		t.Fatalf("%+v %v", dep, err)
	}
	if err := os.WriteFile(e.cfg, []byte("[secrets]\nbackend = \"envfile\"\npath = \""+e.secrets+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.run(t, "", "--yes"); code != 0 || len(e.browser.created) == 0 || e.browser.created[0] != githubWeb+"/organizations/acme/settings/apps/new" {
		t.Errorf("code %d %v\n%s", code, e.browser.created, out)
	}
}

// TestFR1AC5_InstallTimesOut: an app the owner doesn't install stops init
// with a fix.
func TestFR1AC5_InstallTimesOut(t *testing.T) {
	e := newInitEnv(t)
	e.browser.noInstall = true
	code, _, errOut := e.run(t, "", "--yes")
	if code != 1 || !strings.Contains(errOut, "wasn't installed on acme/widgets") {
		t.Errorf("code %d, %s", code, errOut)
	}
}

func TestFR1AC5_DeviceFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("client_id") != "Iv1.abc" {
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"device_code":"dc","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":0,"expires_in":60}`)
		case "/login/oauth/access_token":
			if r.Form.Get("device_code") != "dc" {
				fmt.Fprint(w, `{"error":"access_denied"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"gho_device"}`)
		}
	}))
	defer srv.Close()
	var out bytes.Buffer
	tok, err := deviceFlow(context.Background(), srv.URL, "Iv1.abc", &out)
	if err != nil || tok != "gho_device" || !strings.Contains(out.String(), "ABCD-1234") {
		t.Errorf("%q %v %s", tok, err, out.String())
	}
	if _, err := deviceFlow(context.Background(), srv.URL, "wrong", &out); err == nil {
		t.Error("bad client ID logged in")
	}
}

func TestFR1AC5_RemoteParsing(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/widgets.git":      "acme/widgets",
		"https://github.com/acme/widgets":      "acme/widgets",
		"https://github.com/acme/wid.gets.git": "acme/wid.gets",
		"ssh://git@github.com/acme/widgets/":   "acme/widgets",
	} {
		m := remoteRepo.FindStringSubmatch(in)
		if m == nil || m[1]+"/"+m[2] != want {
			t.Errorf("%s: %v", in, m)
		}
	}
}

// TestFR1AC5_ValidateStepReadsBypassListsAsOwner: init's validate step
// reads the rulesets' bypass lists with the owner's login, so it reports
// no false ruleset_bypass_missing or ruleset_bypass_unverified, and it
// records every ruleset in the projection, after which plain project
// validate (the loop app's token only) finds them right too (work plan
// #101 D2, D4).
func TestFR1AC5_ValidateStepReadsBypassListsAsOwner(t *testing.T) {
	e := newInitEnv(t)
	asked := 0
	ownerToken = func(context.Context, *initOptions) (string, error) { asked++; return ownerSecret, nil }
	code, out, errOut := e.run(t, "", "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if strings.Contains(out, "ruleset_bypass_missing") || strings.Contains(out, "ruleset_bypass_unverified") || !strings.Contains(out, "widgets: 0 error(s)") {
		t.Fatalf("init's validate step:\n%s", out)
	}
	dep, err := profile.LoadDeployment(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	db, err := projection.Open(context.Background(), dep.Projection.Path)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := projection.Rulesets{DB: db, Project: "widgets"}.RulesetRecords(context.Background())
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{}
	for _, rs := range e.srv.Rulesets() {
		want[rs.ID] = true
	}
	got := map[int64]bool{}
	for _, r := range recs {
		got[r.ID] = true
		if len(r.Bypass) == 0 {
			t.Errorf("ruleset %d recorded with no bypass actors", r.ID)
		}
	}
	if len(want) != 4 || !maps.Equal(got, want) {
		t.Errorf("recorded rulesets %v, want %v", got, want)
	}
	if strings.Contains(out+errOut, ownerSecret) {
		t.Error("the owner's token was printed")
	}
	// Plain validate, with no owner login, uses the record.
	code, out, _ = runCLI("project", "validate", "--config", e.cfg, "widgets")
	if code != 0 || strings.Contains(out, "ruleset_bypass") || asked != 1 {
		t.Errorf("plain validate after init (code %d, owner asked %d times):\n%s", code, asked, out)
	}
}

// TestFR1AC5_MapsReadyMarkerToStatusReady: on a repository with a
// status:ready label and no ready label, --yes maps the new profile's
// ready marker to status:ready, so no ready label is created and the next
// steps name status:ready (work plan #104 D1, D2).
func TestFR1AC5_MapsReadyMarkerToStatusReady(t *testing.T) {
	e := newInitEnv(t)
	e.srv.CreateLabel("status:ready")
	code, out, errOut := e.run(t, "", "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	p, err := profile.LoadProfile(filepath.Join(e.repo, ".agent-loop.toml"), profile.DefaultDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Markers["ready"].Label; got != "status:ready" {
		t.Errorf("ready marker maps to %q", got)
	}
	if slices.Contains(e.srv.Labels(), "ready") {
		t.Errorf("a ready label was created: %v", e.srv.Labels())
	}
	for _, s := range []string{`ready mapped to label "status:ready"`, `label an issue "status:ready"`} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}
}

// TestFR1AC5_ReadyMarkerOfferDeclinedOrNotMade: declining the offer keeps
// the default ready label; a repository that has a ready label gets no
// offer (work plan #104 D2).
func TestFR1AC5_ReadyMarkerOfferDeclinedOrNotMade(t *testing.T) {
	e := newInitEnv(t)
	e.srv.CreateLabel("status:ready")
	// Decline the offer (after the three apps and three handler
	// questions), then accept the rest.
	in := "y\ny\ny\n\n\n\nn\n" + strings.Repeat("y\n", 10)
	if _, out, _ := e.run(t, in); !strings.Contains(out, "Map the ready marker") {
		t.Fatalf("no offer:\n%s", out)
	}
	p, err := profile.LoadProfile(filepath.Join(e.repo, ".agent-loop.toml"), profile.DefaultDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Markers["ready"].Label; got != "ready" {
		t.Errorf("declined: ready marker maps to %q", got)
	}

	e = newInitEnv(t)
	e.srv.CreateLabel("status:ready", "ready")
	_, out, _ := e.run(t, "", "--yes")
	if strings.Contains(out, "Map the ready marker") {
		t.Errorf("offered with a ready label:\n%s", out)
	}
	if p, err := profile.LoadProfile(filepath.Join(e.repo, ".agent-loop.toml"), profile.DefaultDeployment()); err != nil || p.Markers["ready"].Label != "ready" {
		t.Errorf("profile %v, %v", p, err)
	}
}

// TestFR1AC5_ReadyMarkerOfferNotMadeUnderCheckOrExistingProfile: --check
// asks no ready-marker question and writes no profile; an existing profile
// keeps its ready marker and gets no offer (work plan #104 D2).
func TestFR1AC5_ReadyMarkerOfferNotMadeUnderCheckOrExistingProfile(t *testing.T) {
	e := newInitEnv(t)
	e.srv.CreateLabel("status:ready")
	_, out, _ := e.run(t, "", "--check")
	if strings.Contains(out, "Map the ready marker") {
		t.Errorf("--check asked the offer:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".agent-loop.toml")); !os.IsNotExist(err) {
		t.Errorf("--check wrote a profile: %v", err)
	}

	e = newInitEnv(t)
	if code, out, errOut := e.run(t, "", "--yes"); code != 0 {
		t.Fatalf("first run: code %d\n%s\n%s", code, out, errOut)
	}
	e.srv.DeleteLabel("ready")
	e.srv.CreateLabel("status:ready")
	_, out, _ = e.run(t, "", "--yes")
	if strings.Contains(out, "Map the ready marker") || strings.Contains(out, `ready mapped to label`) {
		t.Errorf("offered with an existing profile:\n%s", out)
	}
	p, err := profile.LoadProfile(filepath.Join(e.repo, ".agent-loop.toml"), profile.DefaultDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Markers["ready"].Label; got != "ready" {
		t.Errorf("existing profile: ready marker maps to %q", got)
	}
}
