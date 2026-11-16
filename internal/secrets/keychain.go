package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// KeychainService is the service of every Keychain item agent-loop stores.
// The account is the secret's name.
const KeychainService = "agent-loop"

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

func (k *Keychain) trustedApp() (string, error) {
	if k.TrustedApp != "" {
		return k.TrustedApp, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("keychain: finding the agent-loop binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", fmt.Errorf("keychain: finding the agent-loop binary: %w", err)
	}
	return exe, nil
}
