package core

import "testing"

// TestNFR4AC3_CoreCannotImportUntrustedText proves the import half of
// NFR-4 AC3: internal/core can't import internal/ports, where UntrustedText
// is declared, nor internal/ports/untrusted, the only package that reads its
// content. The other half, that the type itself has no accessor, is proved in
// internal/ports/untrusted.
func TestNFR4AC3_CoreCannotImportUntrustedText(t *testing.T) {
	for _, path := range []string{
		"github.com/mayusuk/agent-loop/internal/ports",
		"github.com/mayusuk/agent-loop/internal/ports/untrusted",
	} {
		if allowedImport(path) {
			t.Errorf("the core import guard allows %s", path)
		}
	}
}
