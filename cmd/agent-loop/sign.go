package main

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/mayusuk/agent-loop/internal/secrets"
)

// Seams the tests replace.
var (
	// goos is the system agent-loop runs on.
	goos = runtime.GOOS
	// signBinary signs a binary with the self-signed certificate.
	signBinary = func(ctx context.Context, s secrets.Signer, bin string) (secrets.CodeSignature, bool, error) {
		return s.Sign(ctx, bin)
	}
)

// runSign signs agent-loop with the stable self-signed certificate and the
// fixed identifier, so the Keychain items keep trusting it across rebuilds
// and upgrades (TECHNICAL-SPEC §7.2, work plan #95). Run it after every
// build or upgrade; the Homebrew formula's caveats tell the owner to run it
// after install and upgrade (#124, D1).
func runSign(args []string, stdout, stderr io.Writer) int {
	fs, _ := newFlags("sign", stderr)
	bin := fs.String("binary", "", "the agent-loop binary to sign (default: this one)")
	identity := fs.String("identity", secrets.SigningIdentityName, "the common name of the signing certificate, created when missing")
	keychain := fs.String("keychain", "", "the keychain that holds the certificate (default: your default keychain); it must be on your keychain search list")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return usageErr(stderr, "sign [--binary <path>] [--identity <name>] [--keychain <path>]")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "agent-loop sign: %v\n", err)
		return 1
	}
	if goos != "darwin" {
		return fail(fmt.Errorf("signing is for macOS, where the Keychain trusts a binary by its signature; on %s there is nothing to sign", goos))
	}
	path := *bin
	if path == "" {
		var err error
		if path, err = secrets.RunningBinary(); err != nil {
			return fail(err)
		}
	}
	sig, created, err := signBinary(context.Background(), secrets.Signer{Identity: *identity, Keychain: *keychain}, path)
	if err != nil {
		return fail(err)
	}
	if created {
		fmt.Fprintf(stdout, "Created the code-signing certificate %q in your keychain.\n", *identity)
	}
	fmt.Fprintf(stdout, "Signed %s as %s.\nThe Keychain trusts every build signed this way: %s\n", path, secrets.CodeIdentifier, sig.Requirement)
	return 0
}
