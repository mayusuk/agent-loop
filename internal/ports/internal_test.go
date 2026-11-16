package ports

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFR13AC3_TokenDoesNotPrintSecret(t *testing.T) {
	const secret = "ghs_0123456789abcdef"
	tok := NewToken(secret, IdentityCoder, Scope{Repos: []string{"o/r"}}, time.Unix(0, 0))
	if tok.Secret() != secret {
		t.Fatalf("Secret() = %q", tok.Secret())
	}
	spec := RunSpec{PushToken: &tok}
	// An unexported field is printed by reflection, not through String.
	type hidden struct {
		tok Token
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		for _, arg := range []any{tok, &tok, spec, hidden{tok}, &hidden{tok}, []hidden{{tok}}} {
			if s := fmt.Sprintf(verb, arg); strings.Contains(s, secret) || strings.Contains(s, fmt.Sprintf("%x", secret)) {
				t.Errorf("fmt.Sprintf(%q, %T) leaks the secret: %s", verb, arg, s)
			}
		}
	}
}

// TestFR13AC3_TokenRefusesJSON: a token fails to encode instead of being
// written or silently emptied (review R3).
func TestFR13AC3_TokenRefusesJSON(t *testing.T) {
	const secret = "ghs_0123456789abcdef"
	tok := NewToken(secret, IdentityCoder, Scope{}, time.Unix(0, 0))
	for _, arg := range []any{tok, &tok, RunSpec{PushToken: &tok}} {
		b, err := json.Marshal(arg)
		if !errors.Is(err, ErrTokenSerialize) {
			t.Errorf("json.Marshal(%T) = %s, %v; want ErrTokenSerialize", arg, b, err)
		}
		if strings.Contains(string(b), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
			t.Errorf("json.Marshal(%T) leaks the secret", arg)
		}
	}
	var dst struct{ T Token }
	if err := json.Unmarshal([]byte(`{"T":{}}`), &dst); !errors.Is(err, ErrTokenSerialize) {
		t.Errorf("json.Unmarshal into Token = %v, want ErrTokenSerialize", err)
	}
	if b, err := json.Marshal(RunSpec{}); err != nil {
		t.Errorf("RunSpec without tokens must encode: %s, %v", b, err)
	}
}

// TestNFR4AC3_PayloadsWithTextRefuseJSON: every port type that carries
// untrusted text fails to encode, so it can't pass through workflow history
// with its text silently emptied (D3, review R3).
func TestNFR4AC3_PayloadsWithTextRefuseJSON(t *testing.T) {
	if errUntrustedSerialize == nil {
		t.Fatal("UntrustedText.MarshalJSON succeeded")
	}
	var text UntrustedText // even empty text refuses to encode
	for _, arg := range []any{
		NewItem{Title: text}, NewItem{Body: text},
		NewChange{Title: text}, NewChange{Body: text},
		ReviewPost{Summary: text}, ReviewPost{Findings: []FindingPost{{Body: text}}},
		ManualComment{Body: text}, []ManualComment{{Body: text}},
	} {
		if _, err := json.Marshal(arg); !errors.Is(err, errUntrustedSerialize) {
			t.Errorf("json.Marshal(%T) = %v, want errUntrustedSerialize", arg, err)
		}
	}
	for _, arg := range []any{Item{}, Change{}, Record{}, TrustReport{}, GateResult{}, ItemEvent{}, ChangeEvent{}, ReviewRecord{}} {
		if _, err := json.Marshal(arg); err != nil {
			t.Errorf("json.Marshal(%T) = %v; types without text must encode", arg, err)
		}
	}
}

func TestZeroTokenHasEmptySecret(t *testing.T) {
	if s := (Token{}).Secret(); s != "" {
		t.Fatalf("zero Token Secret() = %q", s)
	}
}

// errUntrustedSerialize is untrusted.ErrSerialize, read through the type's
// own JSON method so this package's tests don't import the accessor package.
var errUntrustedSerialize = func() error { _, err := UntrustedText{}.MarshalJSON(); return err }()
