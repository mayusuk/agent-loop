package secrets

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// Backend holds named secrets. A name is the path of a secret reference
// with a field appended, for example "github-app/coder/private_key".
type Backend interface {
	// Get returns the secret's value. A secret that isn't there is an
	// ErrNotFound error, which names the secret but never a value.
	Get(ctx context.Context, name string) (string, error)
}

// RefScheme is the prefix of every secret reference in the profile and the
// deployment config.
const RefScheme = "secret://"

var refName = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ParseRef returns the name of a secret reference such as
// "secret://github-app/coder", which is "github-app/coder".
func ParseRef(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, RefScheme)
	if !ok || !refName.MatchString(name) {
		return "", fmt.Errorf("%q is not a secret reference such as %q", ref, RefScheme+"github-app/coder")
	}
	return name, nil
}

func notFound(name, where string) error {
	return fmt.Errorf("secret %s not found in %s: %w", name, where, ports.ErrNotFound)
}
