package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// checked is every name Untrusted was asked about.
	checked []string
	// locked makes Untrusted fail as the Keychain does when it is locked.
	locked bool
}

func (s *trustStore) Untrusted(_ context.Context, names []string) ([]string, error) {
	s.checked = append(s.checked, names...)
	if s.locked {
		return nil, fmt.Errorf("keychain: checking %s: %w", names[0], secrets.ErrKeychainLocked)
	}
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

// Work plan #95 D5: server start also checks the gateway token's item when
// the gateway is on, and leaves it out when it is off.
func TestT12_ServerStartChecksTheGatewayTokenOnlyWhenTheGatewayIsOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprint("gateway enabled=", on), func(t *testing.T) {
			cfg := writeDeployment(t, map[string]string{"widgets": testProfile})
			gw := fmt.Sprintf("\n[api.gateway]\nenabled = %v\ntoken = \"secret://gateway/token\"\n", on)
			f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(gw); err != nil {
				t.Fatal(err)
			}
			f.Close()
			// With the gateway off, a GitHub App item is stale too, so
			// server start stops before it would run.
			stale := []string{"gateway/token"}
			if !on {
				stale = append(stale, "github-app/reviewer/private_key")
			}
			s := useTrustStore(t, stableSig, stale...)
			code, _, errOut := runCLI("server", "start", "--config", cfg)
			if code != 1 || !strings.Contains(errOut, "the Keychain items trust a different agent-loop build") {
				t.Fatalf("code %d, stderr %s", code, errOut)
			}
			if got := strings.Contains(errOut, "gateway/token"); got != on {
				t.Errorf("stderr names gateway/token = %v, want %v: %s", got, on, errOut)
			}
			if got := slices.Contains(s.checked, "gateway/token"); got != on {
				t.Errorf("checked gateway/token = %v, want %v: %q", got, on, s.checked)
			}
			if !slices.Contains(s.checked, "github-app/reviewer/private_key") {
				t.Errorf("checked %q, want the GitHub App items too", s.checked)
			}
		})
	}
}

// Work plan #95 D9: server start and worker don't wait for macOS to ask
// for the password that unlocks the keychain; they stop and say how to
// unlock it.
func TestT12_ServerStartAndWorkerStopOnALockedKeychain(t *testing.T) {
	cfg := writeDeployment(t, map[string]string{"widgets": testProfile})
	for _, args := range [][]string{{"server", "start", "--config", cfg}, {"worker", "--capabilities", "agent", "--config", cfg}} {
		t.Run(args[0], func(t *testing.T) {
			s := useTrustStore(t, stableSig)
			s.locked = true
			code, _, errOut := runCLI(args...)
			if code != 1 || !strings.Contains(errOut, "the keychain is locked") || !strings.Contains(errOut, "security unlock-keychain") {
				t.Fatalf("code %d, stderr %s", code, errOut)
			}
			if strings.Contains(errOut, "trust a different agent-loop build") {
				t.Errorf("a locked keychain reported as stale items: %s", errOut)
			}
		})
	}
}

// Work plan #95 D9: an interactive init can't check a locked keychain, so
// it says so and goes on; macOS asks to unlock it when init reads the
// items. Nothing is re-trusted.
func TestT12_InitGoesOnWhenTheKeychainIsLocked(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--check"}} {
		t.Run(args[0], func(t *testing.T) {
			e := newInitEnv(t)
			s := useTrustStore(t, stableSig, "github-app/widgets/coder/private_key")
			s.locked = true
			code, out, errOut := e.run(t, "", args...)
			if !strings.Contains(out, "the keychain is locked, so whether its items trust this build can't be checked") || !strings.Contains(out, "3. Profile") {
				t.Fatalf("code %d\n%s\n%s", code, out, errOut)
			}
			if args[0] == "--yes" && code != 0 {
				t.Fatalf("code %d\n%s\n%s", code, out, errOut)
			}
			if len(s.retrusted) != 0 || strings.Contains(out, "re-trust the Keychain item") {
				t.Errorf("re-trusted %q on a locked keychain:\n%s", s.retrusted, out)
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

// Work plan #95 D6: init asks, then re-trusts each stale item; one that
// fails to re-trust is a warning and a failed step
// (TestT12_InitReportsADeclinedRetrust covers answering no).
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

// Work plan #95 D6: when the owner answers no at "Re-trust them?", nothing
// is re-trusted, the step is reported as skipped and init exits 1 so the
// owner knows to run it again.
func TestT12_InitReportsADeclinedRetrust(t *testing.T) {
	e := newInitEnv(t)
	s := useTrustStore(t, stableSig, "github-app/widgets/coder/private_key")
	code, out, _ := e.run(t, "n\n")
	if code != 1 || len(s.retrusted) != 0 {
		t.Fatalf("code %d, retrusted %q\n%s", code, s.retrusted, out)
	}
	i := strings.Index(out, "Re-trust them? [y/N] ")
	if i < 0 || !strings.HasPrefix(strings.TrimLeft(out[i+len("Re-trust them? [y/N] "):], " \n"), "skipped") {
		t.Fatalf("no skipped after the question:\n%s", out)
	}
	if !strings.Contains(out, "were skipped: run agent-loop init again to finish") {
		t.Errorf("the summary doesn't count the skipped step:\n%s", out)
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
