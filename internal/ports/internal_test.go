package ports

import (
	"encoding/json"
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
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("json leaks the secret: %s", b)
	}
}

func TestZeroTokenHasEmptySecret(t *testing.T) {
	if s := (Token{}).Secret(); s != "" {
		t.Fatalf("zero Token Secret() = %q", s)
	}
}
