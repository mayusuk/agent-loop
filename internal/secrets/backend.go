package secrets

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

// Backend holds named secrets. A name is the path of a secret reference
// with a field appended, for example "github-app/coder/private_key".
type Backend interface {
	// Get returns the secret's value. A secret that isn't there is an
	// ErrNotFound error, which names the secret but never a value.
	Get(ctx context.Context, name string) (string, error)
}

// RefScheme is the prefix of every secret reference in the profile and the
// deployment config.
const RefScheme = "secret://"

var refName = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ParseRef returns the name of a secret reference such as
// "secret://github-app/coder", which is "github-app/coder".
func ParseRef(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, RefScheme)
	if !ok || !refName.MatchString(name) {
		return "", fmt.Errorf("%q is not a secret reference such as %q", ref, RefScheme+"github-app/coder")
	}
	return name, nil
}

func notFound(name, where string) error {
	return fmt.Errorf("secret %s not found in %s: %w", name, where, ports.ErrNotFound)
}

// Open returns the backend the deployment config names.
func Open(s profile.Secrets) (Backend, error) {
	switch s.Backend {
	case profile.SecretsKeychain:
		return &Keychain{}, nil
	case profile.SecretsEnvFile:
		return &EnvFile{Path: s.Path}, nil
	}
	return nil, fmt.Errorf("secrets.backend %q is not supported", s.Backend)
}

// WritableBackend is a Backend that can store secrets, which agent-loop init uses
// for the GitHub Apps' keys (INTEGRATION-SPEC §12.2 step 2).
type WritableBackend interface {
	Backend
	// Set stores the secret, replacing any value with the same name.
	Set(ctx context.Context, name, value string) error
}

// TrustChecker is a Backend whose items trust particular builds of
// agent-loop: the macOS Keychain (TECHNICAL-SPEC §7.2, work plan #95).
type TrustChecker interface {
	WritableBackend
	// Untrusted returns the named items that exist but that the running
	// binary can't read without the system asking. It never asks.
	Untrusted(ctx context.Context, names []string) ([]string, error)
	// Retrust rewrites the item so it trusts the running binary. The
	// system may ask once.
	Retrust(ctx context.Context, name string) error
}

var (
	_ TrustChecker    = (*Keychain)(nil)
	_ WritableBackend = (*Keychain)(nil)
	_ WritableBackend = (*EnvFile)(nil)
)
