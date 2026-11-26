package secrets

import (
	"context"
	"errors"
	"fmt"
)

// KeychainService is the service of every Keychain item agent-loop stores.
// The account is the secret's name.
const KeychainService = "agent-loop"

// ErrKeychainLocked is returned by Untrusted when the keychain is locked:
// whether an item trusts this build can't be told until it is unlocked
// (work plan #95 D9).
var ErrKeychainLocked = errors.New("the keychain is locked")

// Keychain is the macOS Keychain backend. Each secret is a generic-password
// item whose access list trusts only the agent-loop binary, so another
// process that asks for it gets a prompt, not the secret (TECHNICAL-SPEC
// §7.2). On other systems every call fails.
type Keychain struct {
	// Path is a keychain file. Empty means the user's default keychain.
	Path string
	// TrustedApp is the only application allowed to read items that Set
	// writes. Empty means the running executable.
	TrustedApp string
}

// Get implements Backend.
func (k *Keychain) Get(_ context.Context, name string) (string, error) {
	v, found, err := keychainGet(k.Path, KeychainService, name)
	if err != nil {
		return "", fmt.Errorf("keychain: reading %s: %w", name, err)
	}
	if !found {
		return "", notFound(name, "the macOS Keychain")
	}
	return v, nil
}

// Set stores the secret, replacing any item with the same name. The new
// item's access list trusts only the agent-loop binary; an older item's
// access list is never kept.
func (k *Keychain) Set(_ context.Context, name, value string) error {
	if _, err := ParseRef(RefScheme + name); err != nil {
		return fmt.Errorf("keychain: %q is not a secret name", name)
	}
	app, err := k.trustedApp()
	if err != nil {
		return err
	}
	if err := keychainSet(k.Path, KeychainService, name, KeychainService+" "+name, value, app); err != nil {
		return fmt.Errorf("keychain: writing %s: %w", name, err)
	}
	return nil
}

// Delete removes the secret. Deleting a secret that isn't there succeeds.
func (k *Keychain) Delete(_ context.Context, name string) error {
	if err := keychainDelete(k.Path, KeychainService, name); err != nil {
		return fmt.Errorf("keychain: deleting %s: %w", name, err)
	}
	return nil
}

// Untrusted returns the named items that exist but that the running binary
// can't read without macOS asking: their access lists trust a different
// agent-loop build (work plan #95 D7). Items that aren't there are left out.
// It never returns a value, and never asks.
func (k *Keychain) Untrusted(_ context.Context, names []string) ([]string, error) {
	var stale []string
	for _, name := range names {
		found, readable, err := keychainReadable(k.Path, KeychainService, name)
		if err != nil {
			return nil, fmt.Errorf("keychain: checking %s: %w", name, err)
		}
		if found && !readable {
			stale = append(stale, name)
		}
	}
	return stale, nil
}

// Retrust rewrites the item so its access list trusts the running binary.
// Reading it first makes macOS ask once, unless the binary is already
// trusted (work plan #95 D6).
func (k *Keychain) Retrust(ctx context.Context, name string) error {
	v, err := k.Get(ctx, name)
	if err != nil {
		return err
	}
	return k.Set(ctx, name, v)
}

func (k *Keychain) trustedApp() (string, error) {
	if k.TrustedApp != "" {
		return k.TrustedApp, nil
	}
	exe, err := RunningBinary()
	if err != nil {
		return "", fmt.Errorf("keychain: %w", err)
	}
	return exe, nil
}
