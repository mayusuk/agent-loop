package secrets

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeTools replaces runTool for the test and records every call.
type fakeTools struct {
	calls [][]string
	reply func(name string, args []string) ([]byte, error)
}

func useFakeTools(t *testing.T, reply func(name string, args []string) ([]byte, error)) *fakeTools {
	t.Helper()
	f := &fakeTools{reply: reply}
	old := runTool
	runTool = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		return f.reply(name, args)
	}
	t.Cleanup(func() { runTool = old })
	return f
}

func (f *fakeTools) called(prefix ...string) bool {
	for _, c := range f.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

const identities = `
Policy: Code Signing
  Matching identities
  1) 1111111111111111111111111111111111111111 "agent-loop code signing 2" (CSSMERR_TP_NOT_TRUSTED)
  2) 5EFCE69120FA31F8A209A58F5125FFF7C7D7E67A "agent-loop code signing" (CSSMERR_TP_NOT_TRUSTED)
     2 identities found

  Valid identities only
     0 valid identities found
`

func TestT12_FindIdentityMatchesTheExactName(t *testing.T) {
	f := useFakeTools(t, func(string, []string) ([]byte, error) { return []byte(identities), nil })
	hash, found, err := Signer{Keychain: "/tmp/k.keychain-db"}.FindIdentity(context.Background())
	if err != nil || !found || hash != "5EFCE69120FA31F8A209A58F5125FFF7C7D7E67A" {
		t.Fatalf("FindIdentity = %q, %v, %v", hash, found, err)
	}
	if !f.called("security", "find-identity", "-p", "codesigning", "/tmp/k.keychain-db") {
		t.Fatalf("calls = %v", f.calls)
	}
	if _, found, _ := (Signer{Identity: "someone else"}).FindIdentity(context.Background()); found {
		t.Fatal("found an identity that isn't there")
	}
}

func TestT12_SignCreatesTheCertificateOnceAndReplacesTheBinary(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "agent-loop")
	if err := os.WriteFile(bin, []byte("unsigned build"), 0o755); err != nil {
		t.Fatal(err)
	}
	have := false
	var signed string
	f := useFakeTools(t, func(name string, args []string) ([]byte, error) {
		switch {
		case name == "security" && args[0] == "find-identity":
			if have {
				return []byte(identities), nil
			}
			return []byte("0 identities found\n"), nil
		case name == "security" && args[0] == "import":
			if args[2] == "cert" || args[3] == "cert" {
				have = true
			}
			return nil, nil
		case name == "codesign" && args[0] == "--force":
			signed = args[len(args)-1]
			return nil, os.WriteFile(signed, []byte("signed build"), 0o600)
		case name == "codesign" && args[0] == "--verify":
			return nil, nil
		case name == "codesign" && args[0] == "-d":
			return []byte(`designated => identifier "dev.agent-loop" and certificate leaf = H"5efce69120fa31f8a209a58f5125fff7c7d7e67a"` + "\n"), nil
		}
		return nil, errors.New("unexpected call")
	})
	sig, created, err := Signer{}.Sign(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if !created || !sig.Stable() || !strings.Contains(sig.Requirement, `identifier "dev.agent-loop"`) {
		t.Fatalf("Sign = %+v, created %v", sig, created)
	}
	if !f.called("security", "import") || !slices.ContainsFunc(f.calls, func(c []string) bool {
		return c[0] == "security" && slices.Contains(c, "-T") && slices.Contains(c, "/usr/bin/codesign")
	}) {
		t.Fatalf("the key isn't imported for codesign: %v", f.calls)
	}
	if signed == bin || filepath.Dir(signed) != dir {
		t.Fatalf("codesign signed %s in place of a copy next to %s", signed, bin)
	}
	if !slices.ContainsFunc(f.calls, func(c []string) bool {
		return c[0] == "codesign" && slices.Contains(c, "--identifier") && slices.Contains(c, CodeIdentifier)
	}) {
		t.Fatalf("not signed with identifier %s: %v", CodeIdentifier, f.calls)
	}
	if b, _ := os.ReadFile(bin); string(b) != "signed build" {
		t.Fatalf("binary = %q, want the signed copy", b)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}

	// A second Sign reuses the certificate.
	f.calls = nil
	if _, created, err := (Signer{}).Sign(context.Background(), bin); err != nil || created {
		t.Fatalf("second Sign: created %v, %v", created, err)
	}
	if f.called("security", "import") {
		t.Fatal("the second Sign created another certificate")
	}
}

func TestT12_SignLeavesTheBinaryWhenSigningFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent-loop")
	if err := os.WriteFile(bin, []byte("old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	useFakeTools(t, func(name string, args []string) ([]byte, error) {
		if name == "security" {
			return []byte(identities), nil
		}
		return nil, errors.New("codesign: no identity found")
	})
	if _, _, err := (Signer{}).Sign(context.Background(), bin); err == nil {
		t.Fatal("Sign succeeded")
	}
	if b, _ := os.ReadFile(bin); string(b) != "old build" {
		t.Fatalf("binary = %q, want it unchanged", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

// Work plan #95 D8: signing through a symlink (a Homebrew link into the
// Cellar) signs the file it points to, beside that file, and keeps the link.
func TestT12_SignResolvesASymlinkedBinary(t *testing.T) {
	cellar := filepath.Join(t.TempDir(), "Cellar", "agent-loop", "1.0", "bin")
	bindir := filepath.Join(t.TempDir(), "bin")
	for _, d := range []string{cellar, bindir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(cellar, "agent-loop")
	if err := os.WriteFile(real, []byte("unsigned build"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bindir, "agent-loop")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	var signed, read string
	useFakeTools(t, func(name string, args []string) ([]byte, error) {
		switch {
		case name == "security":
			return []byte(identities), nil
		case name == "codesign" && args[0] == "--force":
			signed = args[len(args)-1]
			return nil, os.WriteFile(signed, []byte("signed build"), 0o600)
		case name == "codesign" && args[0] == "--verify":
			return nil, nil
		case name == "codesign" && args[0] == "-d":
			read = args[len(args)-1]
			return []byte(`designated => identifier "dev.agent-loop" and certificate leaf = H"5efce69120fa31f8a209a58f5125fff7c7d7e67a"` + "\n"), nil
		}
		return nil, errors.New("unexpected call")
	})
	if _, _, err := (Signer{}).Sign(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	realDir, _ := filepath.EvalSymlinks(cellar)
	if filepath.Dir(signed) != realDir {
		t.Fatalf("signed a copy in %s, want one next to the Cellar binary in %s", filepath.Dir(signed), realDir)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v, %v", fi.Mode(), err)
	}
	if target, _ := os.Readlink(link); target != real {
		t.Fatalf("the symlink points to %s, want %s", target, real)
	}
	if b, _ := os.ReadFile(real); string(b) != "signed build" {
		t.Fatalf("Cellar binary = %q, want the signed copy", b)
	}
	if read != filepath.Join(realDir, "agent-loop") {
		t.Fatalf("read the signature of %s", read)
	}
	if left, _ := filepath.Glob(filepath.Join(bindir, ".*")); len(left) != 0 {
		t.Fatalf("left behind %v next to the link", left)
	}
}

// A binary that isn't there fails before anything is signed.
func TestT12_SignFailsOnAMissingBinary(t *testing.T) {
	f := useFakeTools(t, func(string, []string) ([]byte, error) { return []byte(identities), nil })
	if _, _, err := (Signer{}).Sign(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Sign succeeded")
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestT12_SignatureTellsAdHocFromStable(t *testing.T) {
	for _, c := range []struct {
		name, out string
		err       error
		want      CodeSignature
	}{
		{"certificate", `designated => identifier "dev.agent-loop" and certificate leaf = H"ab"` + "\nExecutable=/x\n", nil,
			CodeSignature{Signed: true, Requirement: `identifier "dev.agent-loop" and certificate leaf = H"ab"`}},
		{"ad hoc", `# designated => cdhash H"fb3c"` + "\n", nil, CodeSignature{Signed: true, AdHoc: true, Requirement: `cdhash H"fb3c"`}},
		{"unsigned", "", errors.New("codesign: exit status 1: /x: code object is not signed at all"), CodeSignature{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			useFakeTools(t, func(string, []string) ([]byte, error) { return []byte(c.out), c.err })
			got, err := Signature(context.Background(), "/x")
			if err != nil || got != c.want {
				t.Fatalf("Signature = %+v, %v; want %+v", got, err, c.want)
			}
			if got.Stable() != (c.name == "certificate") {
				t.Fatalf("Stable = %v", got.Stable())
			}
		})
	}
	useFakeTools(t, func(string, []string) ([]byte, error) { return nil, errors.New("codesign: no such file") })
	if _, err := Signature(context.Background(), "/x"); err == nil {
		t.Fatal("a codesign failure isn't reported")
	}
}

func TestT12_SigningCertificateIsForCodeSigning(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	_, der, err := newSigningCertificate(SigningIdentityName, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject.CommonName != SigningIdentityName || c.Issuer.CommonName != SigningIdentityName {
		t.Fatalf("subject %q issuer %q", c.Subject, c.Issuer)
	}
	if !slices.Contains(c.ExtKeyUsage, x509.ExtKeyUsageCodeSigning) || c.KeyUsage&x509.KeyUsageDigitalSignature == 0 || c.IsCA {
		t.Fatalf("usage %v %v CA %v", c.ExtKeyUsage, c.KeyUsage, c.IsCA)
	}
	if !c.NotAfter.After(now.AddDate(9, 11, 0)) {
		t.Fatalf("expires %v", c.NotAfter)
	}
}
