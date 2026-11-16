package profile

import (
	"testing"
)

// TestD4_LabelCollisionIgnoresCase is review R4: GitHub label names are
// case-insensitive, so "Ready" and "ready" are the same label.
func TestD4_LabelCollisionIgnoresCase(t *testing.T) {
	_, err := load(t, "", "[markers]\nblocked = { label = \"Ready\" }\n")
	wantProblem(t, err, testFile, "markers.ready", "which marker blocked also uses")
	// Statuses keep exact comparison.
	mustLoad(t, "", "[markers]\nready = { status = \"Todo\" }\nblocked = { status = \"todo\" }\n")
}
