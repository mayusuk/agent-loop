package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// agent-loop init (FR-1 AC5, INTEGRATION-SPEC §12.2). It acts with the
// owner's own login, held in memory only, shows every change and asks
// before making it. --yes accepts every step and --check only reports.

// Seams the tests replace.
var (
	// stdin is where init reads the owner's answers.
	stdin io.Reader = os.Stdin
	// ownerToken gets the owner's login (work plan #24 D1).
	ownerToken = defaultOwnerToken
	// gitRemote reads the repository's origin URL.
	gitRemote = func(dir string) (string, error) {
		out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
		return strings.TrimSpace(string(out)), err
	}
	// newBrowser opens the pages the owner confirms in.
	newBrowser = func(w io.Writer) appBrowser { return &systemBrowser{w: w} }
	// claudeCheck returns warnings about the claude CLI (work plan #24 D12).
	claudeCheck = defaultClaudeCheck
	// githubWeb is GitHub's web origin.
	githubWeb = "https://github.com"
	// installWait and installPoll bound the wait for an app's installation
	// (work plan #24 D10).
	installWait = 10 * time.Minute
	installPoll = 2 * time.Second
)

// appBrowser runs the browser parts of the GitHub App flow.
type appBrowser interface {
	// ManifestCode posts the manifest that build returns, for the redirect
	// URL it is given, to GitHub's createURL, and returns the one-time code
	// GitHub redirects with once the owner confirms.
	ManifestCode(ctx context.Context, createURL string, build func(redirectURL string) ([]byte, error)) (string, error)
	// Install opens the page where the owner installs the app with slug on
	// the repository.
	Install(ctx context.Context, installURL, slug string) error
}

type initOptions struct {
	yes, check            bool
	cfg, repoDir, name    string
	clientID              string
	in                    *bufio.Reader
	out                   io.Writer
	drift, declined, errs int
}

// step prints a step's heading.
func (o *initOptions) step(n int, title string) {
	fmt.Fprintf(o.out, "\n%d. %s\n", n, title)
}

func (o *initOptions) note(format string, args ...any) {
	fmt.Fprintf(o.out, "   "+format+"\n", args...)
}

func (o *initOptions) warn(format string, args ...any) {
	fmt.Fprintf(o.out, "   warning: "+format+"\n", args...)
}

// confirm shows changes and returns whether to make them: never with
// --check (they count as drift), always with --yes, else on "y".
func (o *initOptions) confirm(question string, changes []string) bool {
	if len(changes) == 0 {
		return false
	}
	for _, c := range changes {
		fmt.Fprintf(o.out, "   + %s\n", c)
	}
	if o.check {
		o.drift += len(changes)
		return false
	}
	if o.yes || o.ask(question) {
		return true
	}
	o.declined++
	o.note("skipped")
	return false
}

// ask asks a yes/no question; anything but y or yes is no.
func (o *initOptions) ask(question string) bool {
	a := strings.ToLower(o.line(question + " [y/N] "))
	return a == "y" || a == "yes"
}

// line asks for one line of input. End of input is an empty answer.
func (o *initOptions) line(prompt string) string {
	fmt.Fprintf(o.out, "   %s", prompt)
	s, _ := o.in.ReadString('\n')
	if !strings.HasSuffix(s, "\n") {
		fmt.Fprintln(o.out)
	}
	return strings.TrimSpace(s)
}

// apply makes the changes, stopping at the first failure.
func (o *initOptions) apply(ctx context.Context, red *secrets.Redactor, cs []github.SetupChange) bool {
	for _, c := range cs {
		if err := c.Apply(ctx); err != nil {
			fmt.Fprintf(o.out, "   error: %s: %s\n", c.Summary, red.Redact(err.Error()))
			o.errs++
			return false
		}
	}
	o.note("done")
	return true
}

func summaries(cs []github.SetupChange) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Summary
	}
	return out
}

func runInit(args []string, stdout, stderr io.Writer) int {
	o := &initOptions{out: stdout, in: bufio.NewReader(stdin)}
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&o.yes, "yes", false, "accept every step")
	fs.BoolVar(&o.check, "check", false, "only report what would change; change nothing")
	fs.StringVar(&o.cfg, "config", "", "the deployment config (default ~/.config/agent-loop/deployment.toml)")
	fs.StringVar(&o.repoDir, "repo-dir", ".", "the project's repository")
	fs.StringVar(&o.name, "name", "", "the project's name (default: the repository's name)")
	fs.StringVar(&o.clientID, "oauth-client-id", "", "log in with the device flow of this OAuth App, instead of the gh CLI's token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent-loop init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if o.yes && o.check {
		fmt.Fprintln(stderr, "agent-loop init: use --yes or --check, not both")
		return 2
	}
	code, err := initProject(context.Background(), o)
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop init: %v\n", err)
		return 1
	}
	return code
}

var remoteRepo = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// initProject runs the nine steps. An error stops init.
func initProject(ctx context.Context, o *initOptions) (int, error) {
	repoDir, err := filepath.Abs(o.repoDir)
	if err != nil {
		return 0, err
	}
	cfg := o.cfg
	if cfg == "" {
		if cfg, err = profile.DefaultDeploymentPath(); err != nil {
			return 0, err
		}
	}
	dep := profile.DefaultDeployment()
	depExists := false
	if _, err := os.Stat(cfg); err == nil {
		if dep, err = profile.LoadDeployment(cfg); err != nil {
			return 0, err
		}
		depExists = true
	}

	// 1. Detect.
	o.step(1, "Detect")
	remote, err := gitRemote(repoDir)
	if err != nil {
		return 0, fmt.Errorf("reading the git remote of %s: %v", repoDir, err)
	}
	m := remoteRepo.FindStringSubmatch(remote)
	if m == nil {
		return 0, fmt.Errorf("the origin remote %q isn't a GitHub repository", remote)
	}
	repo := m[1] + "/" + m[2]
	token, err := ownerToken(ctx, o)
	if err != nil {
		return 0, fmt.Errorf("getting your GitHub login: %w", err)
	}
	red := secrets.NewRedactor()
	red.Add(token, time.Time{})
	oc, err := github.NewOwnerClient(githubAPI, repo, token)
	if err != nil {
		return 0, err
	}
	info, err := github.Detect(ctx, oc)
	if err != nil {
		return 0, errors.New(red.Redact(err.Error()))
	}
	o.note("repository %s, default branch %s, you are %s (ID %d)", repo, info.DefaultBranch, info.Login, info.UserID)
	if !info.Admin {
		return 0, fmt.Errorf("%s isn't an admin of %s: init needs admin rights to create the rulesets", info.Login, repo)
	}
	if !info.RulesetsOK {
		return 0, fmt.Errorf("GitHub refuses to list the rulesets of %s: rulesets need a public repository or a paid plan (INTEGRATION-SPEC §9.8 step 5)", repo)
	}
	name := o.name
	if name == "" {
		name = m[2]
	}
	profPath := profile.DefaultProfilePath(repoDir)
	if reg, ok := dep.Projects[name]; ok && reg.Profile != profPath {
		return 0, fmt.Errorf("project %q is already registered in %s for %s: pass --name to name this one", name, cfg, reg.Profile)
	}
	var prof *profile.Profile
	if _, err := os.Stat(profPath); err == nil {
		if prof, err = profile.LoadProfile(profPath, dep); err != nil {
			return 0, fmt.Errorf("the existing profile doesn't load, fix it and run init again: %w", err)
		}
		if prof.Tracker.Repo != repo && !strings.EqualFold(prof.Tracker.Repo, repo) {
			return 0, fmt.Errorf("%s is for %s, not %s", profPath, prof.Tracker.Repo, repo)
		}
	}
	refs := map[ports.Identity]string{}
	for _, id := range []ports.Identity{ports.IdentityCoder, ports.IdentityReviewer, ports.IdentityLoop} {
		refs[id] = secrets.RefScheme + "github-app/" + name + "/" + string(id)
	}
	if prof != nil {
		refs[ports.IdentityCoder], refs[ports.IdentityReviewer], refs[ports.IdentityLoop] =
			prof.Identities.Coder, prof.Identities.Reviewer, prof.Identities.Loop
	}

	// 2. Identities.
	o.step(2, "Identities")
	backend, err := secrets.Open(dep.Secrets)
	if err != nil {
		return 0, err
	}
	apps, err := secrets.NewGitHubApps(backend, profile.Identities{Coder: refs[ports.IdentityCoder], Reviewer: refs[ports.IdentityReviewer], Loop: refs[ports.IdentityLoop]}, red)
	if err != nil {
		return 0, err
	}
	apps.BaseURL = githubAPI
	appIDs := map[ports.Identity]int64{}
	for _, id := range []ports.Identity{ports.IdentityCoder, ports.IdentityReviewer, ports.IdentityLoop} {
		if err := o.identity(ctx, red, apps, backend, info, repo, name, id); err != nil {
			return 0, err
		}
		if a, err := apps.Account(ctx, id); err == nil {
			appIDs[id] = a.AppID
		}
	}

	setup, err := github.NewSetup(oc, github.SetupConfig{BaseBranch: baseOf(prof, info), BranchPrefix: prefixOf(prof),
		Apps:        github.AppIDs{Coder: appIDs[ports.IdentityCoder], Reviewer: appIDs[ports.IdentityReviewer], Loop: appIDs[ports.IdentityLoop]},
		ReleaseApps: releaseOf(prof)})
	if err != nil {
		return 0, err
	}
	checks, err := setup.UpToDate(ctx)
	if err != nil {
		return 0, errors.New(red.Redact(err.Error()))
	}
	admins, err := setup.Admins(ctx)
	if err != nil {
		return 0, errors.New(red.Redact(err.Error()))
	}

	// 3. Profile.
	o.step(3, "Profile")
	owners := []profile.Owner{{Login: info.Login, ID: strconv.FormatInt(info.UserID, 10)}}
	stages := map[string][]string{}
	if prof == nil {
		o.note("%s doesn't exist yet", profPath)
		for _, a := range admins {
			if a.ID != owners[0].ID && !o.check && (o.yes || o.ask(fmt.Sprintf("%s (ID %s) is an admin of %s, and admins bypass the loop's rulesets. Add them to owners?", a.Login, a.ID, repo))) {
				owners = append(owners, profile.Owner{Login: a.Login, ID: a.ID})
			}
		}
		for _, st := range []string{profile.StageImplement, profile.StageReview, profile.StageAddress} {
			stages[st] = []string{"agent-loop-claude", st}
			if o.yes || o.check {
				continue
			}
			if cmd := strings.Fields(o.line(fmt.Sprintf("Handler for %s (empty: the starter handler agent-loop-claude %s; or type a command): ", st, st))); len(cmd) > 0 {
				stages[st] = cmd
			}
		}
		var gates []string
		if len(checks.Checks) == 0 && !o.yes && !o.check {
			for _, g := range strings.Split(o.line("The base branch has no required checks. Name the checks merges must wait for, separated by commas (empty: none): "), ",") {
				if g = strings.TrimSpace(g); g != "" {
					gates = append(gates, g)
				}
			}
		}
		ms, markerNote := defaultMarkers(), "the default markers"
		alt, err := setup.ReadyLabelOffer(ctx, ms["ready"])
		if err != nil {
			return 0, errors.New(red.Redact(err.Error()))
		}
		if alt != "" && !o.check && (o.yes || o.ask(fmt.Sprintf("%s has a %q label and no %q label. Map the ready marker to %q, so issues labelled %q are picked?", repo, alt, ms["ready"], alt, alt))) {
			ms["ready"] = alt
			markerNote = fmt.Sprintf("the default markers with ready mapped to label %q", alt)
		}
		text := profileText(repo, info.DefaultBranch, refs, owners, stages, gates, ms)
		if o.confirm("Write it?", []string{fmt.Sprintf("write %s (tracker and code host github on %s, base branch %s, owners %s, %s and limits)", profPath, repo, info.DefaultBranch, ownerList(owners), markerNote)}) {
			if err := os.WriteFile(profPath, []byte(text), 0o644); err != nil {
				return 0, err
			}
			o.note("done; review it and commit it, init doesn't")
		}
		if _, err := os.Stat(profPath); err == nil {
			if prof, err = profile.LoadProfile(profPath, dep); err != nil {
				return 0, fmt.Errorf("the written profile doesn't load: %w", err)
			}
		}
	} else {
		o.note("%s exists; it is kept as it is", profPath)
	}
	if _, ok := dep.Projects[name]; ok {
		o.note("project %s is registered in %s", name, cfg)
	} else if o.confirm("Register it?", []string{fmt.Sprintf("register project %s in %s", name, cfg)}) {
		if err := registerProject(cfg, depExists, name, profPath); err != nil {
			return 0, err
		}
		o.note("done")
	}

	// 4. Labels.
	o.step(4, "Labels")
	labels, err := setup.Labels(ctx, labelNames(prof))
	if err != nil {
		return 0, errors.New(red.Redact(err.Error()))
	}
	if len(labels) == 0 {
		o.note("every label exists")
	} else if o.confirm("Create them?", summaries(labels)) {
		o.apply(ctx, red, labels)
	}

	// 5. Push rules.
	o.step(5, "Push rules")
	if appIDs[ports.IdentityCoder] == 0 || appIDs[ports.IdentityLoop] == 0 {
		o.warn("the rulesets need the coder and loop apps; run init again once step 2 has created them")
		if o.check {
			o.drift++
		}
	} else {
		req, other, err := setup.Rulesets(ctx)
		if err != nil {
			return 0, errors.New(red.Redact(err.Error()))
		}
		if len(req) == 0 {
			o.note("the three rulesets are in place")
		} else if o.confirm("Make these changes?", summaries(req)) {
			o.apply(ctx, red, req)
		}
		if len(other) > 0 && o.confirm("Create it?", summaries(other)) {
			o.apply(ctx, red, other)
		}
	}
	switch {
	case checks.Change != nil:
		if o.confirm("Turn it on?", []string{checks.Change.Summary}) {
			o.apply(ctx, red, []github.SetupChange{*checks.Change})
		}
	case checks.Note != "":
		o.warn("%s", checks.Note)
	case checks.UpToDate:
		o.note("branches must be up to date before merging")
	}
	for _, p := range setup.BaseRuleProblems(ctx) {
		o.warn("%s (fix: %s)", p.Message, p.Hint)
	}
	isOwner := map[string]bool{}
	for _, ow := range ownersOf(prof, owners) {
		isOwner[ow.ID] = true
	}
	for _, a := range admins {
		if !isOwner[a.ID] {
			o.warn("%s (ID %s) is an admin of %s but not an owner account, and admins bypass the loop's rulesets (fix: lower their access, or add them to owners in the profile)", a.Login, a.ID, repo)
		}
	}

	// 6. Handlers.
	o.step(6, "Handlers")
	starter := false
	for _, st := range []string{profile.StageImplement, profile.StageReview, profile.StageAddress} {
		cmd := stages[st]
		if prof != nil {
			cmd = prof.Stages[st].Command
		}
		o.note("%s: %s", st, strings.Join(cmd, " "))
		starter = starter || (len(cmd) > 0 && filepath.Base(cmd[0]) == "agent-loop-claude")
	}
	if starter {
		for _, w := range claudeCheck(ctx) {
			o.warn("%s", w)
		}
	}

	// 7. Gates.
	o.step(7, "Gates")
	if len(checks.Checks) > 0 {
		o.note("required checks on %s: %s", baseOf(prof, info), strings.Join(checks.Checks, ", "))
	} else {
		o.warn("the base branch has no required checks, so merges wait only on review")
	}
	if prof != nil && prof.Gates != nil {
		o.note("the profile's gates replace them: %s", strings.Join(prof.Gates, ", "))
	}

	// 8. Validate.
	o.step(8, "Validate")
	code := 1
	if _, err := os.Stat(cfg); err == nil {
		if lp, err := loadProject(cfg, name); err == nil {
			code = printProblems(o.out, name, validateProject(ctx, lp))
		} else {
			o.note("skipped: %v", err)
		}
	} else {
		o.note("skipped: the project isn't registered")
	}

	// 9. Next steps.
	o.step(9, "Next steps")
	switch {
	case o.check && o.drift > 0:
		o.note("%d change(s) to make: run agent-loop init to make them", o.drift)
		return 1, nil
	case o.check:
		o.note("nothing to change")
		return code, nil
	case o.errs > 0 || o.declined > 0:
		o.note("%d step(s) failed and %d were skipped: run agent-loop init again to finish", o.errs, o.declined)
		return 1, nil
	}
	o.note("review and commit %s: git add %s && git commit -m \"Add agent-loop profile\"", profile.ProfileFileName, profile.ProfileFileName)
	o.note("start the server: agent-loop server start")
	o.note("label an issue %q, then watch: agent-loop board %s", labelOf(prof, "ready"), name)
	return code, nil
}

// identity is step 2 for one identity: reuse the stored app, or create it
// through the manifest flow and store its key; then make sure it is
// installed (work plan #24 D3, D10).
func (o *initOptions) identity(ctx context.Context, red *secrets.Redactor, apps *secrets.GitHubApps, backend secrets.Backend, info github.RepoInfo, repo, project string, id ports.Identity) error {
	ref := apps.Refs[id]
	appName := project + "-" + string(id)
	_, errID := backend.Get(ctx, ref+"/"+secrets.FieldAppID)
	_, errKey := backend.Get(ctx, ref+"/"+secrets.FieldPrivateKey)
	if errors.Is(errID, ports.ErrNotFound) || errors.Is(errKey, ports.ErrNotFound) {
		if !o.confirm("Create it?", []string{fmt.Sprintf("create the GitHub App %s with exactly its permissions (%s), install it on %s only, and store its key in the %s secret store as %s (your browser opens to confirm)",
			appName, permList(github.WantPermissions(id)), repo, backendName(backend), secrets.RefScheme+ref)}) {
			return nil
		}
		w, ok := backend.(secrets.WritableBackend)
		if !ok {
			return fmt.Errorf("the secret store can't store the %s app's key", id)
		}
		createURL := githubWeb + "/settings/apps/new"
		if info.OwnerType == "Organization" {
			createURL = githubWeb + "/organizations/" + strings.Split(repo, "/")[0] + "/settings/apps/new"
		}
		code, err := newBrowser(o.out).ManifestCode(ctx, createURL, func(redirect string) ([]byte, error) {
			return github.AppManifest(id, appName, info.HTMLURL, redirect)
		})
		if err != nil {
			return fmt.Errorf("creating the %s app: %v", id, err)
		}
		app, err := github.ConvertManifest(ctx, githubAPI, code, nil)
		if err != nil {
			return err
		}
		red.Add(app.PEM, time.Time{})
		if err := w.Set(ctx, ref+"/"+secrets.FieldAppID, strconv.FormatInt(app.ID, 10)); err != nil {
			return err
		}
		if err := w.Set(ctx, ref+"/"+secrets.FieldPrivateKey, app.PEM); err != nil {
			return err
		}
		o.note("created app %s (ID %d)", app.Slug, app.ID)
		return o.install(ctx, red, apps, repo, id, app.Slug)
	}
	if errID != nil || errKey != nil {
		return fmt.Errorf("reading the %s app's secrets: %v", id, errors.Join(errID, errKey))
	}
	acct, err := apps.Account(ctx, id)
	if err != nil {
		return fmt.Errorf("the %s app stored as %s can't be read: %s", id, secrets.RefScheme+ref, red.Redact(err.Error()))
	}
	perms, err := apps.InstalledPermissions(ctx, id, repo)
	if errors.Is(err, ports.ErrNotFound) {
		if !o.confirm("Install it?", []string{fmt.Sprintf("install the GitHub App %s on %s only (your browser opens to confirm)", acct.Slug, repo)}) {
			return nil
		}
		return o.install(ctx, red, apps, repo, id, acct.Slug)
	}
	if err != nil {
		return fmt.Errorf("reading the %s app's installation: %s", id, red.Redact(err.Error()))
	}
	o.note("%s: app %s (ID %d) is installed", id, acct.Slug, acct.AppID)
	want := github.WantPermissions(id)
	for k, v := range perms {
		if want[k] != v {
			o.warn("the %s app has %s: %s, but needs %s (fix: change it in the app's settings, %s/settings/apps/%s/permissions)", id, k, v, orNone(want[k]), githubWeb, acct.Slug)
		}
	}
	for k, v := range want {
		if _, ok := perms[k]; !ok {
			o.warn("the %s app lacks %s: %s (fix: add it in the app's settings, %s/settings/apps/%s/permissions)", id, k, v, githubWeb, acct.Slug)
		}
	}
	return nil
}

// install opens the install page and waits until the app is installed on
// the repository.
func (o *initOptions) install(ctx context.Context, red *secrets.Redactor, apps *secrets.GitHubApps, repo string, id ports.Identity, slug string) error {
	if err := newBrowser(o.out).Install(ctx, githubWeb+"/apps/"+slug+"/installations/new", slug); err != nil {
		return err
	}
	deadline := time.Now().Add(installWait)
	for {
		_, err := apps.InstalledPermissions(ctx, id, repo)
		if err == nil {
			o.note("installed %s on %s", slug, repo)
			return nil
		}
		if !errors.Is(err, ports.ErrNotFound) {
			return fmt.Errorf("checking the %s app's installation: %s", id, red.Redact(err.Error()))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the %s app wasn't installed on %s within %s: install it, then run init again", slug, repo, installWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(installPoll):
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func permList(p map[string]string) string {
	var out []string
	for k, v := range p {
		out = append(out, k+": "+v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func backendName(b secrets.Backend) string {
	if _, ok := b.(*secrets.Keychain); ok {
		return "macOS Keychain"
	}
	return "env-file"
}

func ownerList(os []profile.Owner) string {
	var out []string
	for _, o := range os {
		out = append(out, fmt.Sprintf("%s (%s)", o.Login, o.ID))
	}
	return strings.Join(out, ", ")
}

func baseOf(p *profile.Profile, info github.RepoInfo) string {
	if p != nil && p.BaseBranch != "" {
		return p.BaseBranch
	}
	return info.DefaultBranch
}

func prefixOf(p *profile.Profile) string {
	if p != nil {
		return p.BranchPrefix
	}
	return profile.DefaultBranchPrefix
}

func releaseOf(p *profile.Profile) []int64 {
	if p != nil {
		return p.ReleaseApps
	}
	return nil
}

func ownersOf(p *profile.Profile, fresh []profile.Owner) []profile.Owner {
	if p != nil {
		return p.Identities.Owners
	}
	return fresh
}

// defaultMarkers is the marker mapping of a new profile.
func defaultMarkers() map[string]string {
	out := map[string]string{}
	for m, l := range profile.LoopMarkers {
		out[m] = l
	}
	for _, t := range profile.DefaultTypes {
		out[profile.TypeMarkerPrefix+t] = t
	}
	return out
}

// labelNames are the labels of the profile's marker mapping (§12.2 step
// 4), sorted.
func labelNames(p *profile.Profile) []string {
	var out []string
	if p == nil {
		for _, l := range defaultMarkers() {
			out = append(out, l)
		}
	} else {
		for _, mm := range p.Markers {
			if mm.Label != "" {
				out = append(out, mm.Label)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func labelOf(p *profile.Profile, marker string) string {
	if p != nil && p.Markers[marker].Label != "" {
		return p.Markers[marker].Label
	}
	return profile.LoopMarkers[marker]
}

func tomlList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// profileText is the profile init writes, with the defaults spelt out
// (work plan #24 D5).
func profileText(repo, base string, refs map[ports.Identity]string, owners []profile.Owner, stages map[string][]string, gates []string, ms map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# agent-loop project profile, written by agent-loop init. Review it and commit it.\n")
	fmt.Fprintf(&b, "# Every key is in TECHNICAL-SPEC §13; the values below are the defaults.\n\n")
	fmt.Fprintf(&b, "base_branch   = %q\n", base)
	fmt.Fprintf(&b, "branch_prefix = %q\n", profile.DefaultBranchPrefix)
	fmt.Fprintf(&b, "wip           = %d\n", profile.DefaultWIP)
	fmt.Fprintf(&b, "max_rounds    = %d\n", profile.DefaultMaxRounds)
	fmt.Fprintf(&b, "merge_method  = %q\n", profile.DefaultMergeMethod)
	if len(gates) > 0 {
		fmt.Fprintf(&b, "gates         = %s\n", tomlList(gates))
	}
	fmt.Fprintf(&b, "\n[tracker]\nadapter = %q\nrepo    = %q\n", profile.DefaultAdapter, repo)
	fmt.Fprintf(&b, "\n[code_host]\nadapter = %q\nrepo    = %q\n", profile.DefaultAdapter, repo)
	fmt.Fprintf(&b, "\n[limits]\ndaily_agent_runs = %d\n", profile.DefaultDailyAgentRuns)
	fmt.Fprintf(&b, "\n[markers]\n")
	keys := make([]string, 0, len(ms))
	for m := range ms {
		keys = append(keys, m)
	}
	sort.Strings(keys)
	for _, m := range keys {
		k := m
		if strings.Contains(m, ":") {
			k = strconv.Quote(m)
		}
		fmt.Fprintf(&b, "%s = { label = %q }\n", k, ms[m])
	}
	fmt.Fprintf(&b, "\n[identities]\ncoder    = %q\nreviewer = %q\nloop     = %q\nowners   = [", refs[ports.IdentityCoder], refs[ports.IdentityReviewer], refs[ports.IdentityLoop])
	for i, ow := range owners {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "{ login = %q, id = %s }", ow.Login, ow.ID)
	}
	b.WriteString("]\n")
	for _, st := range []string{profile.StageImplement, profile.StageReview, profile.StageAddress} {
		fmt.Fprintf(&b, "\n[stages.%s]\ncommand  = %s\ncontract = \"1.0\"\n", st, tomlList(stages[st]))
	}
	return b.String()
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// registerProject adds [projects.<name>] to the deployment config,
// creating the file when it doesn't exist (work plan #24 D13). The rest
// of the file is left as it was.
func registerProject(cfg string, exists bool, name, profPath string) error {
	key := name
	if !bareKey.MatchString(key) {
		key = strconv.Quote(key)
	}
	entry := fmt.Sprintf("[projects.%s]\nprofile = %q\n", key, profPath)
	if !exists {
		if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(cfg, []byte("# agent-loop deployment config. Every key left out takes its default.\n\n"+entry), 0o600); err != nil {
			return err
		}
	} else {
		old, err := os.ReadFile(cfg)
		if err != nil {
			return err
		}
		text := string(old)
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if err := os.WriteFile(cfg, []byte(text+"\n"+entry), 0o600); err != nil {
			return err
		}
		if _, err := profile.LoadDeployment(cfg); err != nil {
			_ = os.WriteFile(cfg, old, 0o600)
			return fmt.Errorf("registering the project would break %s, so it is left as it was: %w", cfg, err)
		}
	}
	return nil
}

// defaultOwnerToken is the gh CLI's token, or a device-flow login when
// --oauth-client-id names an OAuth App (work plan #24 D1).
func defaultOwnerToken(ctx context.Context, o *initOptions) (string, error) {
	if o.clientID != "" {
		return deviceFlow(ctx, githubWeb, o.clientID, o.out)
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token failed (%v): log in with gh auth login, or pass --oauth-client-id for the device flow", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", errors.New("gh auth token printed no token: log in with gh auth login")
	}
	return tok, nil
}

// defaultClaudeCheck checks that the claude CLI is installed and logged
// in, for the starter handlers (§12.2 step 6).
func defaultClaudeCheck(ctx context.Context) []string {
	if _, err := exec.LookPath("claude"); err != nil {
		return []string{"the claude CLI isn't on PATH, and the starter handlers run it (fix: install Claude Code)"}
	}
	if err := exec.CommandContext(ctx, "claude", "auth", "status").Run(); err != nil {
		return []string{"couldn't confirm the claude CLI is logged in (fix: run claude and log in)"}
	}
	return nil
}
