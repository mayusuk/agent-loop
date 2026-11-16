package secrets

import (
	"testing"

	"github.com/mayusuk/agent-loop/internal/profile"
)

func TestOpenSelectsTheConfiguredBackend(t *testing.T) {
	b, err := Open(profile.Secrets{Backend: profile.SecretsEnvFile, Path: "/x/secrets.env"})
	if e, ok := b.(*EnvFile); err != nil || !ok || e.Path != "/x/secrets.env" {
		t.Errorf("envfile: %#v, %v", b, err)
	}
	b, err = Open(profile.Secrets{Backend: profile.SecretsKeychain})
	if _, ok := b.(*Keychain); err != nil || !ok {
		t.Errorf("keychain: %#v, %v", b, err)
	}
	if _, err := Open(profile.Secrets{Backend: "vault"}); err == nil {
		t.Error("unknown backend accepted")
	}
}
