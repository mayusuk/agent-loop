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
// Read. The zero value is empty text.
//
// The content sits behind a *string. fmt calls String only on values it can
// reach as an interface; inside an unexported struct field it walks the value
// by reflection instead. There it prints a pointer to a string as an address,
// for every verb, and never follows it (it follows pointers only to structs,
// arrays, slices and maps). Because of the pointer, == compares identity,
// not content: compare with Read.
type Text struct {
	p *string
}

// New wraps s as untrusted text.
func New(s string) Text { return Text{p: &s} }

// Read returns the content of t.
func Read(t Text) string {
	if t.p == nil {
		return ""
	}
	return *t.p
}

// String returns a placeholder, never the content.
func (Text) String() string { return placeholder }

// GoString returns a placeholder, never the content.
func (Text) GoString() string { return placeholder }
