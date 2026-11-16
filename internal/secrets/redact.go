package secrets

import (
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mask replaces every credential the redactor finds.
const Mask = "[redacted]"

// githubTokenPatterns match GitHub token formats whatever their origin, so a
// GitHub token the redactor was never told about is masked too. Each pattern
// matches a fixed length, which the streaming writer relies on.
var githubTokenPatterns = []string{
	`gh[pousr]_[A-Za-z0-9]{36}`,
	`github_pat_[A-Za-z0-9_]{82}`,
}

// patternMaxLen is the length of the longest githubTokenPatterns match.
const patternMaxLen = len("github_pat_") + 82

// Redactor masks credentials in logs and stored output (FR-13 AC3, PRODUCT
// §8 item 7). It knows every live token it was given, and also masks
// anything that has the shape of a GitHub token. It is safe for concurrent
// use.
type Redactor struct {
	now func() time.Time

	mu      sync.Mutex
	secrets map[string]time.Time // secret -> when it may be forgotten
	re      *regexp.Regexp       // nil until built for the current set
	maxLen  int
}

// NewRedactor returns a redactor that knows no secrets yet.
func NewRedactor() *Redactor {
	return &Redactor{now: time.Now, secrets: map[string]time.Time{}}
}

// Add makes the redactor mask secret until the time until. A zero until
// means for the life of the redactor. Secrets whose time has passed are
// dropped the next time Add is called. An empty secret is ignored.
func (r *Redactor) Add(secret string, until time.Time) {
	if secret == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for s, u := range r.secrets {
		if !u.IsZero() && now.After(u) {
			delete(r.secrets, s)
		}
	}
	if old, ok := r.secrets[secret]; ok && (old.IsZero() || (!until.IsZero() && old.After(until))) {
		return
	}
	r.secrets[secret] = until
	r.re = nil
}

// matcher returns the regexp for the current secrets, and the length of the
// longest possible match.
func (r *Redactor) matcher() (*regexp.Regexp, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.re != nil {
		return r.re, r.maxLen
	}
	lits := make([]string, 0, len(r.secrets))
	for s := range r.secrets {
		lits = append(lits, s)
	}
	// Longest first, so a secret that is a prefix of another doesn't leave
	// the rest of the longer one in place.
	sort.Slice(lits, func(i, j int) bool {
		if len(lits[i]) != len(lits[j]) {
			return len(lits[i]) > len(lits[j])
		}
		return lits[i] < lits[j]
	})
	alts := make([]string, 0, len(lits)+len(githubTokenPatterns))
	maxLen := patternMaxLen
	for _, s := range lits {
		alts = append(alts, regexp.QuoteMeta(s))
		maxLen = max(maxLen, len(s))
	}
	alts = append(alts, githubTokenPatterns...)
	r.re = regexp.MustCompile(strings.Join(alts, "|"))
	r.maxLen = maxLen
	return r.re, r.maxLen
}

// Redact returns s with every credential masked.
func (r *Redactor) Redact(s string) string {
	re, _ := r.matcher()
	return re.ReplaceAllLiteralString(s, Mask)
}

// Writer returns a writer that masks credentials in everything written to it
// before passing it on to w. A credential split across several writes is
// still masked: the writer holds back the last few bytes of each write until
// it knows they can't be the start of one. Call Flush (or Close) at the end
// of the stream to write what is held back. It is safe for concurrent use.
func (r *Redactor) Writer(w io.Writer) *Writer {
	return &Writer{r: r, w: w}
}

// Writer is a redacting writer. See Redactor.Writer.
type Writer struct {
	r *Redactor
	w io.Writer

	mu      sync.Mutex
	pending []byte
}

// Write masks credentials in p and writes what is safe to w. It returns
// len(p) when w accepted the output.
func (x *Writer) Write(p []byte) (int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.pending = append(x.pending, p...)
	if err := x.emit(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes everything held back, masked.
func (x *Writer) Flush() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.emit(true)
}

// Close flushes the writer. It doesn't close the underlying writer.
func (x *Writer) Close() error { return x.Flush() }

// emit writes the part of pending that no later write can change. Every
// match is at most maxLen bytes long, so a match that starts before
// len-(maxLen-1) is already complete. A match that crosses that point is
// written whole.
func (x *Writer) emit(all bool) error {
	re, maxLen := x.r.matcher()
	buf := x.pending
	cut := len(buf)
	if !all {
		cut = len(buf) - (maxLen - 1)
		if cut <= 0 {
			return nil
		}
	}
	var out []byte
	last := 0
	for _, m := range re.FindAllIndex(buf, -1) {
		if m[0] >= cut {
			break
		}
		out = append(out, buf[last:m[0]]...)
		out = append(out, Mask...)
		last = m[1]
	}
	if last > cut {
		cut = last
	}
	out = append(out, buf[last:cut]...)
	x.pending = append(x.pending[:0], buf[cut:]...)
	if len(out) == 0 {
		return nil
	}
	_, err := x.w.Write(out)
	return err
}
