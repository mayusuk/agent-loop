package ports

import "github.com/mayusuk/agent-loop/internal/ports/untrusted"

// UntrustedText is text from a human or an agent: item and change bodies,
// comments, reply and review text (INTEGRATION-SPEC §8, TECHNICAL-SPEC §4).
// It has no method that returns its content. Adapters create it with
// untrusted.New, and only the adapters and the worker read it, with
// untrusted.Read.
type UntrustedText = untrusted.Text
