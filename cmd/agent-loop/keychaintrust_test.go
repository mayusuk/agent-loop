package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// trustStore is an env/file store that acts like the Keychain: the items in
// stale trust another agent-loop build.
type trustStore struct {
	*secrets.EnvFile
	stale     map[string]bool
	retrusted []string
	failOn    string
}

func (s *trustStore) Untrusted(_ context.Context, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		if s.stale[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *trustStore) Retrust(_ context.Context, name string) error {
	if name == s.failOn {
		return errors.New("the owner clicked Deny")
	}
	s.retrusted = append(s.retrusted, name)
	delete(s.stale, name)
	return nil
}

// useTrustStore makes the commands open a trustStore on macOS, with the
// running build signed as sig.
func useTrustStore(t *testing.T, sig secrets.CodeSignature, stale ...string) *trustStore {
	t.Helper()
	s := &trustStore{stale: map[string]bool{}}
	for _, n := range stale {
		s.stale[n] = true
	}
	oldOpen, oldGOOS, oldSig := openBackend, goos, runningSignature
	t.Cleanup(func() { openBackend, goos, runningSignature = oldOpen, oldGOOS, oldSig })
	openBackend = func(c profile.Secrets) (secrets.Backend, error) {
		b, err := secrets.Open(profile.Secrets{Backend: profile.SecretsEnvFile, Path: c.Path})
		if err != nil {
			return nil, err
		}
		s.EnvFile = b.(*secrets.EnvFile)
		return s, nil
	}
	goos = "darwin"
	runningSignature = func(context.Context) (secrets.CodeSignature, error) { return sig, nil }
	return s
}

var (
	stableSig = secrets.CodeSignature{Signed: true, Requirement: `identifier "dev.agent-loop" and certificate leaf = H"ab"`}
	adHocSig  = secrets.CodeSignature{Signed: true, AdHoc: true, Requirement: `cdhash H"cd"`}
)

// Work plan #95 D5: server start and worker stop, naming the items, before
// they read a secret that would wait at a password prompt.
func TestT12_ServerStartAndWorkerStopOnStaleKeychainItems(t *testing.T) {
	cfg := writeDeployment(t, map[string]string{"widgets": testProfile})
	for _, args := range [][]string{{"server", "start", "--config", cfg}, {"worker", "--capabilities", "agent", "--config", cfg}} {
		t.Run(args[0], func(t *testing.T) {
			useTrustStore(t, stableSig, "github-app/reviewer/private_key")
			code, _, errOut := runCLI(args...)
			if code != 1 || !strings.Contains(errOut, "the Keychain items trust a different agent-loop build") ||
				!strings.Contains(errOut, "github-app/reviewer/private_key") || !strings.Contains(errOut, "agent-loop sign") {
				t.Fatalf("code %d, stderr %s", code, errOut)
			}
		})
	}
}

func TestT12_CheckKeychainTrust(t *testing.T) {
	ctx := context.Background()
	names := identitySecretNames(profile.Identities{Coder: "secret://github-app/p/coder", Reviewer: "secret://github-app/p/reviewer", Loop: "secret://github-app/p/loop"})
	if len(names) != 9 || !slices.Contains(names, "github-app/p/loop/installation_id") {
		t.Fatalf("names = %q", names)
	}
	s := useTrustStore(t, adHocSig)
	var warned []string
	warn := func(w string) { warned = append(warned, w) }
	if err := checkKeychainTrust(ctx, s, names, warn); err != nil {
		t.Fatal(err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "agent-loop sign") {
		t.Fatalf("an ad-hoc build: warnings %q", warned)
	}
	warned = nil
	runningSignature = func(context.Context) (secrets.CodeSignature, error) { return stableSig, nil }
	s.stale["github-app/p/coder/app_id"] = true
	if err := checkKeychainTrust(ctx, s, names, warn); err == nil || !strings.Contains(err.Error(), "github-app/p/coder/app_id") || len(warned) != 0 {
		t.Fatalf("a stale item: %v, warnings %q", err, warned)
	}
	// The env/file store and other systems have nothing to check.
	env := &secrets.EnvFile{Path: "/nonexistent"}
	runningSignature = func(context.Context) (secrets.CodeSignature, error) { return adHocSig, nil }
	if err := checkKeychainTrust(ctx, env, names, warn); err != nil || len(warned) != 0 {
		t.Fatalf("envfile: %v, %q", err, warned)
	}
	goos = "linux"
	if err := checkKeychainTrust(ctx, s, names, warn); err != nil || len(warned) != 0 {
		t.Fatalf("linux: %v, %q", err, warned)
	}
}

// Work plan #95 D6: init --check reports stale items as drift and stops
// before reading them.
func TestT12_InitCheckReportsStaleKeychainItems(t *testing.T) {
	e := newInitEnv(t)
	s := useTrustStore(t, adHocSig, "github-app/widgets/coder/private_key")
	before := snapshot(t, e.dir)
	code, out, errOut := e.run(t, "", "--check")
	if code != 1 || errOut != "" {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	for _, w := range []string{"warning: this agent-loop build isn't signed with the agent-loop certificate",
		"the Keychain items trust a different agent-loop build", "+ re-trust the Keychain item github-app/widgets/coder/private_key",
		"1 change(s) to make", "--check stops here"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "3. Profile") || len(s.retrusted) != 0 {
		t.Errorf("--check went on or re-trusted:\n%s", out)
	}
	if after := snapshot(t, e.dir); len(after) != len(before) {
		t.Error("--check changed files")
	}
}

// Work plan #95 D6: init asks, then re-trusts each stale item; a declined
// one is reported and init goes on.
func TestT12_InitRetrustsStaleKeychainItems(t *testing.T) {
	e := newInitEnv(t)
	s := useTrustStore(t, stableSig, "github-app/widgets/coder/private_key", "github-app/widgets/loop/app_id")
	code, out, _ := e.run(t, "y\n", "--yes")
	if code != 0 || !slices.Equal(s.retrusted, []string{"github-app/widgets/coder/private_key", "github-app/widgets/loop/app_id"}) {
		t.Fatalf("code %d, retrusted %q\n%s", code, s.retrusted, out)
	}
	if !strings.Contains(out, "re-trusted github-app/widgets/loop/app_id") || strings.Contains(out, "warning: this agent-loop build") {
		t.Errorf("output:\n%s", out)
	}

	e = newInitEnv(t)
	s = useTrustStore(t, stableSig, "github-app/widgets/reviewer/app_id")
	s.failOn = "github-app/widgets/reviewer/app_id"
	code, out, _ = e.run(t, "", "--yes")
	if code != 1 || !strings.Contains(out, "warning: re-trusting github-app/widgets/reviewer/app_id: the owner clicked Deny") {
		t.Fatalf("a failed re-trust: code %d\n%s", code, out)
	}
}

func TestT12_SignCommand(t *testing.T) {
	oldSign, oldGOOS := signBinary, goos
	t.Cleanup(func() { signBinary, goos = oldSign, oldGOOS })
	var got []string
	signBinary = func(_ context.Context, s secrets.Signer, bin string) (secrets.CodeSignature, bool, error) {
		got = append(got, s.Identity+"|"+s.Keychain+"|"+bin)
		return stableSig, len(got) == 1, nil
	}

	goos = "linux"
	if code, _, errOut := runCLI("sign"); code != 1 || !strings.Contains(errOut, "signing is for macOS") || len(got) != 0 {
		t.Fatalf("linux: code %d %s", code, errOut)
	}

	goos = "darwin"
	code, out, errOut := runCLI("sign")
	self, _ := secrets.RunningBinary()
	if code != 0 || errOut != "" || got[0] != secrets.SigningIdentityName+"||"+self {
		t.Fatalf("code %d, calls %q, stderr %s", code, got, errOut)
	}
	if !strings.Contains(out, `Created the code-signing certificate "agent-loop code signing"`) || !strings.Contains(out, "as dev.agent-loop") || !strings.Contains(out, stableSig.Requirement) {
		t.Errorf("stdout %s", out)
	}
	code, out, _ = runCLI("sign", "--binary", "/opt/homebrew/Cellar/agent-loop/1.0/bin/agent-loop", "--identity", "mine", "--keychain", "/k")
	if code != 0 || got[1] != "mine|/k|/opt/homebrew/Cellar/agent-loop/1.0/bin/agent-loop" || strings.Contains(out, "Created") {
		t.Fatalf("code %d, calls %q, %s", code, got, out)
	}
	if code, _, _ := runCLI("sign", "extra"); code != 2 {
		t.Errorf("an argument: code %d", code)
	}
	signBinary = func(context.Context, secrets.Signer, string) (secrets.CodeSignature, bool, error) {
		return secrets.CodeSignature{}, false, errors.New("codesign: no identity found")
	}
	if code, _, errOut := runCLI("sign"); code != 1 || !strings.Contains(errOut, "no identity found") {
		t.Errorf("a failure: code %d %s", code, errOut)
	}
}
