//go:build darwin && cgo

package secrets

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// probeBuild differs between the builds the live test makes, so their
// bytes and ad-hoc hashes differ.
var probeBuild = "test"

const probeItem = "probe/value"

// TestT12_KeychainProbeHelper is not a test on its own: the live test runs
// builds of this test binary with AGENT_LOOP_KC_PROBE=<set|get>:<keychain>.
// "set" writes an item that trusts the running build; "get" reads it with
// prompts off.
func TestT12_KeychainProbeHelper(t *testing.T) {
	mode, path, ok := strings.Cut(os.Getenv("AGENT_LOOP_KC_PROBE"), ":")
	if !ok {
		t.Skip("run by TestT12_KeychainTrustSurvivesResign")
	}
	if err := setKeychainInteraction(false); err != nil {
		t.Fatal(err)
	}
	k := &Keychain{Path: path}
	switch mode {
	case "set":
		if err := k.Set(context.Background(), probeItem, "not-a-secret-"+probeBuild); err != nil {
			t.Fatal(err)
		}
	case "get":
		if _, err := k.Get(context.Background(), probeItem); err != nil {
			t.Fatal(err)
		}
	}
}

// TestT12_KeychainTrustSurvivesResign is the experiment behind work plan
// #95: an item written by one build signed with the self-signed certificate
// is read without a prompt by a rebuild signed with the same certificate,
// and not by an ad-hoc rebuild or one signed with another certificate. It
// uses a temporary keychain only. codesign finds an identity only on the
// user's keychain search list, so the test adds the temporary keychain to
// it and puts the list back afterwards.
func TestT12_KeychainTrustSurvivesResign(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("set AGENT_LOOP_LIVE=1: signs test builds and changes the keychain search list for the test")
	}
	ctx := context.Background()
	dir := t.TempDir()
	kc := filepath.Join(dir, "probe.keychain-db")
	if err := createKeychain(kc, "probe-password"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeKeychain(kc) })
	out, err := exec.Command("security", "list-keychains", "-d", "user").Output()
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(string(out), -1) {
		list = append(list, m[1])
	}
	if err := exec.Command("security", append([]string{"list-keychains", "-d", "user", "-s", kc}, list...)...).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := exec.Command("security", append([]string{"list-keychains", "-d", "user", "-s"}, list...)...).Run(); err != nil {
			t.Errorf("restoring the keychain search list %q: %v", list, err)
		}
	})

	a := Signer{Identity: "agent-loop probe A", Keychain: kc}
	b := Signer{Identity: "agent-loop probe B", Keychain: kc}
	build := func(name string) string {
		t.Helper()
		bin := filepath.Join(dir, name)
		cmd := exec.Command("go", "test", "-c", "-o", bin, "-ldflags", "-X github.com/mayusuk/agent-loop/internal/secrets.probeBuild="+name, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", name, err, out)
		}
		return bin
	}
	sign := func(s Signer, bin string) {
		t.Helper()
		sig, _, err := s.Sign(ctx, bin)
		if err != nil {
			t.Fatal(err)
		}
		if !sig.Stable() {
			t.Fatalf("%s: signature %+v isn't stable", bin, sig)
		}
	}
	probe := func(bin, mode string) error {
		cmd := exec.Command(bin, "-test.run", "^TestT12_KeychainProbeHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "AGENT_LOOP_KC_PROBE="+mode+":"+kc)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	install := filepath.Join(dir, "bin", "agent-loop")
	if err := os.MkdirAll(filepath.Dir(install), 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(bin string) string {
		t.Helper()
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(install) // a new inode, as an upgrade makes
		if err := os.WriteFile(install, b, 0o755); err != nil {
			t.Fatal(err)
		}
		return install
	}

	v1, v2 := build("v1"), build("v2")
	adhoc := build("adhoc")
	v2b := build("v2b")
	sign(a, v1)
	sign(a, v2)
	sign(b, v2b)
	if err := probe(put(v1), "set"); err != nil {
		t.Fatalf("v1 writing the item: %v", err)
	}
	if err := probe(put(v2), "get"); err != nil {
		t.Errorf("a rebuild signed with the same certificate, same path, can't read the item without a prompt: %v", err)
	}
	if err := probe(v2, "get"); err != nil {
		t.Errorf("a rebuild signed with the same certificate, another path, can't read the item without a prompt: %v", err)
	}
	if err := probe(put(adhoc), "get"); err == nil {
		t.Error("an ad-hoc rebuild read the item: the test doesn't tell trusted from untrusted")
	}
	if err := probe(put(v2b), "get"); err == nil {
		t.Error("a rebuild signed with another certificate read the item")
	}
}
