//go:build darwin

package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// tempKeychain creates an unlocked keychain file that is deleted after the
// test. The tests never touch the login keychain.
func tempKeychain(t *testing.T) *Keychain {
	t.Helper()
	if err := setKeychainInteraction(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setKeychainInteraction(true) })
	path := filepath.Join(t.TempDir(), "test.keychain-db")
	if err := createKeychain(path, "test-password"); err != nil {
		t.Fatalf("creating a keychain: %v", err)
	}
	t.Cleanup(func() { _ = removeKeychain(path) })
	return &Keychain{Path: path}
}

func TestKeychainSetGetDelete(t *testing.T) {
	k := tempKeychain(t)
	ctx := context.Background()
	if _, err := k.Get(ctx, "github-app/coder/app_id"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get before Set: %v, want ErrNotFound", err)
	}
	pem := "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"
	if err := k.Set(ctx, "github-app/coder/private_key", pem); err != nil {
		t.Fatal(err)
	}
	if v, err := k.Get(ctx, "github-app/coder/private_key"); err != nil || v != pem {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := k.Set(ctx, "github-app/coder/private_key", "second"); err != nil {
		t.Fatal(err)
	}
	if v, err := k.Get(ctx, "github-app/coder/private_key"); err != nil || v != "second" {
		t.Fatalf("Get after replace = %q, %v", v, err)
	}
	if err := k.Delete(ctx, "github-app/coder/private_key"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(ctx, "github-app/coder/private_key"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := k.Delete(ctx, "github-app/coder/private_key"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := k.Set(ctx, "bad name/", "x"); err == nil {
		t.Error("Set accepted a bad name")
	}
}

// TECHNICAL-SPEC §7.2: each item's access list names only the agent-loop
// binary (here the test binary, which is the running executable).
func TestKeychainAccessListNamesOnlyTheBinary(t *testing.T) {
	k := tempKeychain(t)
	ctx := context.Background()
	if err := k.Set(ctx, "github-app/loop/private_key", "v"); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	apps, anyApp, err := keychainTrusted(k.Path, KeychainService, "github-app/loop/private_key")
	if err != nil {
		t.Fatal(err)
	}
	if anyApp {
		t.Error("the access list lets any application read the item")
	}
	if len(apps) != 1 || apps[0] != exe {
		t.Errorf("trusted applications = %q, want only %q", apps, exe)
	}

	// Writing the item again with another trusted app replaces the list.
	k.TrustedApp = "/usr/bin/true"
	if err := k.Set(ctx, "github-app/loop/private_key", "v2"); err != nil {
		t.Fatal(err)
	}
	apps, _, err = keychainTrusted(k.Path, KeychainService, "github-app/loop/private_key")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0] != "/usr/bin/true" {
		t.Errorf("after rewrite, trusted applications = %q, want only /usr/bin/true", apps)
	}
}
