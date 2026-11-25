package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// macOS identifies an application in a Keychain access list by its
// designated requirement. A build that `go build` signs ad hoc requires its
// own exact bytes, so every rebuild is a different application. A build
// signed with a stable certificate and a fixed identifier requires only
// that identifier and that certificate, which every later build signed the
// same way meets (TECHNICAL-SPEC §7.2, work plan #95).

const (
	// CodeIdentifier is the code-signing identifier of every agent-loop
	// build (work plan #95 D1).
	CodeIdentifier = "dev.agent-loop"
	// SigningIdentityName is the common name of the self-signed
	// certificate agent-loop is signed with (work plan #95 D1).
	SigningIdentityName = "agent-loop code signing"
	// signingValidity is how long a new certificate is valid (work plan
	// #95 D2).
	signingValidity = 10 * 365 * 24 * time.Hour
)

// runTool runs one of macOS's command-line tools and returns its standard
// output. A failure includes the tool's error output, which never holds a
// secret. Tests replace it.
var runTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	if stderr.Len() > 0 {
		out = append(out, stderr.Bytes()...)
	}
	return out, nil
}

// Signer signs agent-loop binaries with the self-signed certificate.
type Signer struct {
	// Identity is the certificate's common name. Empty means
	// SigningIdentityName.
	Identity string
	// Keychain is the keychain that holds the certificate and its key.
	// Empty means the user's default keychain (normally the login
	// keychain). codesign finds the identity only in a keychain on the
	// user's search list.
	Keychain string
}

func (s Signer) name() string {
	if s.Identity != "" {
		return s.Identity
	}
	return SigningIdentityName
}

func (s Signer) keychainArgs() []string {
	if s.Keychain == "" {
		return nil
	}
	return []string{s.Keychain}
}

var identityLine = regexp.MustCompile(`^\s*\d+\) ([0-9A-F]{40}) "(.*)"`)

// FindIdentity returns the SHA-1 hash of the signing certificate, if the
// keychain holds it together with its private key.
func (s Signer) FindIdentity(ctx context.Context) (hash string, found bool, err error) {
	out, err := runTool(ctx, "security", append([]string{"find-identity", "-p", "codesigning"}, s.keychainArgs()...)...)
	if err != nil {
		return "", false, fmt.Errorf("listing code-signing identities: %w", err)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if m := identityLine.FindStringSubmatch(l); m != nil && m[2] == s.name() {
			return m[1], true, nil
		}
	}
	return "", false, nil
}

// CreateIdentity creates the self-signed code-signing certificate and its
// key in the keychain, and returns the certificate's SHA-1 hash. The key may
// be used by codesign without asking (work plan #95 D2).
func (s Signer) CreateIdentity(ctx context.Context) (string, error) {
	key, cert, err := newSigningCertificate(s.name(), time.Now())
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "agent-loop-sign-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	keyFile, certFile := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0o600); err != nil {
		return "", err
	}
	kc := []string{}
	if s.Keychain != "" {
		kc = []string{"-k", s.Keychain}
	}
	if _, err := runTool(ctx, "security", append(append([]string{"import", keyFile, "-t", "priv", "-f", "openssl"}, kc...), "-T", "/usr/bin/codesign")...); err != nil {
		return "", fmt.Errorf("importing the signing key: %w", err)
	}
	if _, err := runTool(ctx, "security", append([]string{"import", certFile, "-t", "cert", "-f", "pemseq"}, kc...)...); err != nil {
		return "", fmt.Errorf("importing the signing certificate: %w", err)
	}
	sum := sha1.Sum(cert)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

// newSigningCertificate returns a new RSA key and a self-signed DER
// certificate for code signing.
func newSigningCertificate(name string, now time.Time) (*rsa.PrivateKey, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(signingValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return key, der, nil
}

// Sign signs the binary with the certificate and CodeIdentifier, creating
// the certificate first when the keychain doesn't hold it. It signs a copy
// next to the binary and renames the copy over it, so a running binary is
// never rewritten in place (work plan #95 D4). It returns the new
// signature and whether it created the certificate.
func (s Signer) Sign(ctx context.Context, binary string) (CodeSignature, bool, error) {
	hash, found, err := s.FindIdentity(ctx)
	if err != nil {
		return CodeSignature{}, false, err
	}
	created := false
	if !found {
		if hash, err = s.CreateIdentity(ctx); err != nil {
			return CodeSignature{}, false, err
		}
		created = true
	}
	fi, err := os.Stat(binary)
	if err != nil {
		return CodeSignature{}, created, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(binary), "."+filepath.Base(binary)+".sign-")
	if err != nil {
		return CodeSignature{}, created, err
	}
	defer os.Remove(tmp.Name())
	src, err := os.Open(binary)
	if err != nil {
		tmp.Close()
		return CodeSignature{}, created, err
	}
	_, err = io.Copy(tmp, src)
	src.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), fi.Mode().Perm())
	}
	if err != nil {
		return CodeSignature{}, created, err
	}
	if _, err := runTool(ctx, "codesign", "--force", "--sign", hash, "--identifier", CodeIdentifier, tmp.Name()); err != nil {
		return CodeSignature{}, created, fmt.Errorf("signing %s: %w", binary, err)
	}
	if _, err := runTool(ctx, "codesign", "--verify", "--strict", tmp.Name()); err != nil {
		return CodeSignature{}, created, fmt.Errorf("verifying the signed copy of %s: %w", binary, err)
	}
	if err := os.Rename(tmp.Name(), binary); err != nil {
		return CodeSignature{}, created, err
	}
	sig, err := Signature(ctx, binary)
	return sig, created, err
}

// CodeSignature is what macOS uses to recognise a binary in a Keychain
// access list.
type CodeSignature struct {
	// Signed is false for a binary with no signature at all.
	Signed bool
	// AdHoc means the designated requirement is the binary's own hash, so
	// a rebuild is a different application.
	AdHoc bool
	// Requirement is the designated requirement.
	Requirement string
}

// Stable reports whether a rebuild signed the same way keeps the
// binary's Keychain trust.
func (c CodeSignature) Stable() bool { return c.Signed && !c.AdHoc }

// Signature reads the binary's designated requirement with codesign.
func Signature(ctx context.Context, binary string) (CodeSignature, error) {
	out, err := runTool(ctx, "codesign", "-d", "-r-", binary)
	if err != nil {
		if strings.Contains(err.Error(), "not signed at all") {
			return CodeSignature{}, nil
		}
		return CodeSignature{}, fmt.Errorf("reading the code signature of %s: %w", binary, err)
	}
	for _, l := range strings.Split(string(out), "\n") {
		_, req, ok := strings.Cut(l, "designated => ")
		if !ok {
			continue
		}
		req = strings.TrimSpace(req)
		// codesign marks an implicit requirement with "#"; for an ad-hoc
		// signature it is the code's hash.
		adhoc := strings.HasPrefix(strings.TrimSpace(l), "#") || strings.HasPrefix(req, "cdhash ")
		return CodeSignature{Signed: true, AdHoc: adhoc, Requirement: req}, nil
	}
	return CodeSignature{}, errors.New("codesign printed no designated requirement for " + binary)
}

// RunningBinary returns the running agent-loop binary, with symlinks
// resolved (a Homebrew install runs from its Cellar).
func RunningBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding the agent-loop binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", fmt.Errorf("finding the agent-loop binary: %w", err)
	}
	return exe, nil
}
