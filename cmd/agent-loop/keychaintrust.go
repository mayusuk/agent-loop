package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// Seams the tests replace.
var (
	// runningSignature reads the running binary's code signature.
	runningSignature = func(ctx context.Context) (secrets.CodeSignature, error) {
		bin, err := secrets.RunningBinary()
		if err != nil {
			return secrets.CodeSignature{}, err
		}
		return secrets.Signature(ctx, bin)
	}
	// openBackend opens the deployment's secret store.
	openBackend = secrets.Open
)

// identitySecretNames returns the names of the Keychain items the GitHub
// App identities use.
func identitySecretNames(ids profile.Identities) []string {
	var names []string
	for _, ref := range []string{ids.Coder, ids.Reviewer, ids.Loop} {
		name, err := secrets.ParseRef(ref)
		if err != nil {
			continue
		}
		for _, f := range []string{secrets.FieldAppID, secrets.FieldPrivateKey, secrets.FieldInstallationID} {
			names = append(names, name+"/"+f)
		}
	}
	return names
}

// adHocWarning returns a warning when the deployment keeps its secrets in
// the Keychain and the running build is signed ad hoc, so the Keychain
// stops trusting it at the next rebuild (work plan #95 D6).
func adHocWarning(ctx context.Context, b secrets.Backend) string {
	if _, ok := b.(secrets.TrustChecker); !ok || goos != "darwin" {
		return ""
	}
	sig, err := runningSignature(ctx)
	if err != nil {
		return fmt.Sprintf("can't read this build's code signature: %v", err)
	}
	if sig.Stable() {
		return ""
	}
	return "this agent-loop build isn't signed with the agent-loop certificate, so the Keychain stops trusting it at the next rebuild or upgrade (fix: run agent-loop sign, then agent-loop init)"
}

// staleKeychainItems returns the named items that trust a different
// agent-loop build, when the deployment keeps its secrets in the Keychain.
func staleKeychainItems(ctx context.Context, b secrets.Backend, names []string) ([]string, error) {
	tc, ok := b.(secrets.TrustChecker)
	if !ok || goos != "darwin" {
		return nil, nil
	}
	return tc.Untrusted(ctx, names)
}

// staleTrustError describes stale items for server start and worker, which
// stop rather than wait at a password prompt (work plan #95 D5).
func staleTrustError(stale []string) error {
	return fmt.Errorf("the Keychain items trust a different agent-loop build, so reading them would wait for your password: %s (fix: run agent-loop sign, then agent-loop init in the project's repository to re-trust them)", strings.Join(stale, ", "))
}

// checkKeychainTrust is the startup check of server start and worker: it
// warns about an ad-hoc build and fails on items that trust another build.
func checkKeychainTrust(ctx context.Context, b secrets.Backend, names []string, warn func(string)) error {
	if w := adHocWarning(ctx, b); w != "" {
		warn(w)
	}
	stale, err := staleKeychainItems(ctx, b, names)
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		return staleTrustError(stale)
	}
	return nil
}
