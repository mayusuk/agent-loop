//go:build !darwin

package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestKeychainFailsOffMacOS(t *testing.T) {
	k := &Keychain{}
	ctx := context.Background()
	if _, err := k.Get(ctx, "a"); !errors.Is(err, errKeychainUnsupported) {
		t.Errorf("Get: %v", err)
	}
	if err := k.Set(ctx, "a", "v"); !errors.Is(err, errKeychainUnsupported) {
		t.Errorf("Set: %v", err)
	}
	if err := k.Delete(ctx, "a"); !errors.Is(err, errKeychainUnsupported) {
		t.Errorf("Delete: %v", err)
	}
}
