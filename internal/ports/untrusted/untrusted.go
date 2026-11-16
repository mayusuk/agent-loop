// Package untrusted is the only way to read the content of untrusted text
// (TECHNICAL-SPEC §4, NFR-4 AC3). Tracker bodies, comments and agent output
// travel as Text, which has no method that returns its content. Only the
// adapters, the worker (internal/workflows) and the conformance suite may
// import this package; a test enforces that, and the core's import guard keeps
// internal/core out of it.
package untrusted

// placeholder is what fmt prints instead of the content.
const placeholder = "[untrusted text]"

// Text is text from a human or an agent. Its content is reachable only through
// Read. The zero value is empty text. encoding/json encodes it as {}, so its
// content never reaches logs or workflow history.
type Text struct {
	s string
}

// New wraps s as untrusted text.
func New(s string) Text { return Text{s: s} }

// Read returns the content of t.
func Read(t Text) string { return t.s }

// String returns a placeholder, never the content.
func (Text) String() string { return placeholder }

// GoString returns a placeholder, never the content.
func (Text) GoString() string { return placeholder }
