//go:build darwin && cgo

package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A keychain file that doesn't exist holds no secrets.
func TestKeychainMissingFileReadsAsNotFound(t *testing.T) {
	if err := setKeychainInteraction(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setKeychainInteraction(true) })
	k := &Keychain{Path: filepath.Join(t.TempDir(), "missing.keychain-db")}
	if _, err := k.Get(context.Background(), "a"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("Get on a missing keychain: %v", err)
	}
}

func TestOSStatusErrorNamesTheStatus(t *testing.T) {
	if err := osStatusError(-25300); !strings.Contains(err.Error(), "OSStatus -25300") || !strings.Contains(err.Error(), "could not be found") {
		t.Errorf("osStatusError(-25300) = %v", err)
	}
	if err := osStatusError(1); !strings.Contains(err.Error(), "OSStatus 1") {
		t.Errorf("osStatusError(1) = %v", err)
	}
}

// Work plan #95 D7: an item whose access list trusts another binary is
// reported, without asking; a trusted item and a missing one are not.
func TestT12_UntrustedListsItemsThatTrustAnotherBuild(t *testing.T) {
	k := tempKeychain(t)
	ctx := context.Background()
	if err := k.Set(ctx, "github-app/p/coder/app_id", "1"); err != nil {
		t.Fatal(err)
	}
	other := &Keychain{Path: k.Path, TrustedApp: "/usr/bin/true"}
	if err := other.Set(ctx, "github-app/p/coder/private_key", "not-a-key"); err != nil {
		t.Fatal(err)
	}
	stale, err := k.Untrusted(ctx, []string{"github-app/p/coder/app_id", "github-app/p/coder/private_key", "github-app/p/coder/installation_id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "github-app/p/coder/private_key" {
		t.Fatalf("Untrusted = %q, want only the item that trusts /usr/bin/true", stale)
	}
	// Probing leaves prompts off, as tempKeychain set them, so a Get of
	// the stale item still fails instead of asking.
	if _, err := k.Get(ctx, "github-app/p/coder/private_key"); err == nil {
		t.Fatal("read an item that trusts another binary")
	}
	missing := &Keychain{Path: filepath.Join(t.TempDir(), "missing.keychain-db")}
	if stale, err := missing.Untrusted(ctx, []string{"a"}); err != nil || len(stale) != 0 {
		t.Fatalf("Untrusted on a missing keychain = %q, %v", stale, err)
	}
}

// Work plan #95 D6: Retrust rewrites a readable item for the running binary
// and keeps its value; an item it can't read is left as it is.
func TestT12_RetrustRewritesForTheRunningBinary(t *testing.T) {
	k := tempKeychain(t)
	ctx := context.Background()
	if err := k.Set(ctx, "x/app_id", "42"); err != nil {
		t.Fatal(err)
	}
	if err := k.Retrust(ctx, "x/app_id"); err != nil {
		t.Fatal(err)
	}
	if v, err := k.Get(ctx, "x/app_id"); err != nil || v != "42" {
		t.Fatalf("after Retrust, Get = %q, %v", v, err)
	}
	other := &Keychain{Path: k.Path, TrustedApp: "/usr/bin/true"}
	if err := other.Set(ctx, "x/private_key", "not-a-key"); err != nil {
		t.Fatal(err)
	}
	// With prompts off the read fails, as when the owner declines.
	if err := k.Retrust(ctx, "x/private_key"); err == nil {
		t.Fatal("Retrust succeeded without reading the item")
	}
	if apps, _, err := keychainTrusted(k.Path, KeychainService, "x/private_key"); err != nil || len(apps) != 1 || apps[0] != "/usr/bin/true" {
		t.Fatalf("a failed Retrust changed the item: %q, %v", apps, err)
	}
}
