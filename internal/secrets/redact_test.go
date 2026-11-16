package secrets

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ghs returns a well-formed installation token built from seed.
func ghs(seed string) string {
	s := "ghs_" + strings.Repeat(seed, 36)
	return s[:40]
}

func TestFR13AC3_RedactMasksKnownTokensAndGitHubShapes(t *testing.T) {
	r := NewRedactor()
	r.Add("opaque-secret-value", time.Time{})
	pat := "github_pat_" + strings.Repeat("A1_", 28)[:82]
	in := "a=opaque-secret-value b=" + ghs("x") + " c=" + "ghp_" + strings.Repeat("Z", 36) + " d=" + pat + " e=ghs_short"
	got := r.Redact(in)
	want := "a=" + Mask + " b=" + Mask + " c=" + Mask + " d=" + Mask + " e=ghs_short"
	if got != want {
		t.Errorf("Redact =\n%q\nwant\n%q", got, want)
	}
}

func TestFR13AC3_RedactLongestSecretFirst(t *testing.T) {
	r := NewRedactor()
	r.Add("abcdefgh", time.Time{})
	r.Add("abcdefgh-and-more", time.Time{})
	if got := r.Redact("[abcdefgh-and-more][abcdefgh]"); got != "["+Mask+"]["+Mask+"]" {
		t.Errorf("got %q", got)
	}
}

func TestFR13AC3_RedactorForgetsExpiredSecretsOnly(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	r := NewRedactor()
	r.now = func() time.Time { return now }
	r.Add("short-lived-secret", now.Add(time.Hour))
	r.Add("forever-secret", time.Time{})
	if got := r.Redact("short-lived-secret"); got != Mask {
		t.Errorf("live secret not masked: %q", got)
	}
	now = now.Add(2 * time.Hour)
	r.Add("another-secret", now.Add(time.Hour)) // prunes
	if got := r.Redact("short-lived-secret forever-secret another-secret"); got != "short-lived-secret "+Mask+" "+Mask {
		t.Errorf("got %q", got)
	}
	// Adding a secret again never shortens how long it is kept.
	r.Add("forever-secret", now.Add(time.Minute))
	now = now.Add(time.Hour)
	r.Add("x-secret", time.Time{})
	if got := r.Redact("forever-secret"); got != Mask {
		t.Errorf("forever secret forgotten: %q", got)
	}
	r.Add("", time.Time{})
	if got := r.Redact("plain"); got != "plain" {
		t.Errorf("empty secret masked text: %q", got)
	}
}

// The acceptance test of T12: a mixed stream of log lines, with tokens
// split across writes at every byte boundary.
func TestFR13AC3_WriterMasksTokensSplitAcrossWrites(t *testing.T) {
	r := NewRedactor()
	tok1, tok2 := ghs("a1"), "a-non-github-credential-0123"
	r.Add(tok1, time.Time{})
	r.Add(tok2, time.Time{})
	stream := "worker: starting run 7\n" +
		"git: remote: using " + tok1 + "\n" +
		"handler: token=" + tok2 + tok2 + " (twice)\n" +
		"handler: unrelated ghs_ prefix\n" +
		tok1
	want := "worker: starting run 7\n" +
		"git: remote: using " + Mask + "\n" +
		"handler: token=" + Mask + Mask + " (twice)\n" +
		"handler: unrelated ghs_ prefix\n" +
		Mask
	for size := 1; size <= len(stream); size++ {
		var out bytes.Buffer
		w := r.Writer(&out)
		for i := 0; i < len(stream); i += size {
			end := min(i+size, len(stream))
			if n, err := w.Write([]byte(stream[i:end])); err != nil || n != end-i {
				t.Fatalf("size %d: Write = %d, %v", size, n, err)
			}
			if strings.Contains(out.String(), tok1) || strings.Contains(out.String(), tok2) {
				t.Fatalf("size %d: a token reached the output", size)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("chunk size %d:\n%q\nwant\n%q", size, out.String(), want)
		}
	}
}

func TestFR13AC3_WriterSplitAtEveryPoint(t *testing.T) {
	r := NewRedactor()
	tok := ghs("q")
	r.Add(tok, time.Time{})
	line := "push with " + tok + " done\n"
	for cut := 0; cut <= len(line); cut++ {
		var out bytes.Buffer
		w := r.Writer(&out)
		w.Write([]byte(line[:cut]))
		w.Write([]byte(line[cut:]))
		w.Flush()
		if got := out.String(); got != "push with "+Mask+" done\n" {
			t.Fatalf("cut %d: %q", cut, got)
		}
	}
}

func TestFR13AC3_WriterPassesPlainTextThrough(t *testing.T) {
	r := NewRedactor()
	var out bytes.Buffer
	w := r.Writer(&out)
	text := strings.Repeat("plain log line\n", 50)
	w.Write([]byte(text))
	if out.Len() == 0 {
		t.Error("nothing written before Flush for a long write")
	}
	w.Flush()
	if out.String() != text {
		t.Errorf("text changed")
	}
}

func TestFR13AC3_WriterKnowsTokensAddedMidStream(t *testing.T) {
	r := NewRedactor()
	var out bytes.Buffer
	w := r.Writer(&out)
	w.Write([]byte("before "))
	r.Add("added-later-secret", time.Time{})
	w.Write([]byte("added-later-secret after"))
	w.Flush()
	if got := out.String(); got != "before "+Mask+" after" {
		t.Errorf("got %q", got)
	}
}

func TestFR13AC3_WriterConcurrentWrites(t *testing.T) {
	r := NewRedactor()
	tok := ghs("c")
	r.Add(tok, time.Time{})
	var out bytes.Buffer
	var mu sync.Mutex
	w := r.Writer(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	}))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Fprintf(w, "line %d %s\n", i, tok)
		}()
	}
	wg.Wait()
	w.Flush()
	if strings.Contains(out.String(), tok) || strings.Count(out.String(), Mask) != 8 {
		t.Errorf("got %q", out.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
