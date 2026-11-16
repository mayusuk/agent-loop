package secrets

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// fakeStore issues numbered tokens that live for an hour.
type fakeStore struct {
	now func() time.Time

	mu        sync.Mutex
	n         int
	revoked   []string
	failRev   map[string]bool
	failIssue error
}

func (s *fakeStore) Token(_ context.Context, id ports.Identity, sc ports.Scope) (ports.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failIssue != nil {
		return ports.Token{}, s.failIssue
	}
	s.n++
	return ports.NewToken(fmt.Sprintf("tok-%d", s.n), id, sc, s.now().Add(time.Hour)), nil
}

func (s *fakeStore) Revoke(_ context.Context, t ports.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRev[t.Secret()] {
		return fmt.Errorf("revoke %s: %w", t.Secret(), ports.ErrTransient)
	}
	s.revoked = append(s.revoked, t.Secret())
	return nil
}

func TestRunRefreshesNearExpiryAndRevokesEverythingAtTheEnd(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := &fakeStore{now: clock}
	r := NewRun(st)
	r.Now = clock
	ctx := context.Background()
	push, err := r.Issue(ctx, ports.IdentityCoder, ports.Scope{Repos: []string{"o/r"}})
	if err != nil {
		t.Fatal(err)
	}
	read, err := r.Issue(ctx, ports.IdentityCoder, ports.Scope{Repos: []string{"o/r"}})
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := push.Token(ctx)
	if tk.Secret() != "tok-1" {
		t.Fatalf("first token %q", tk.Secret())
	}
	now = now.Add(49 * time.Minute) // 11 minutes left: kept
	if tk, _ = push.Token(ctx); tk.Secret() != "tok-1" {
		t.Errorf("token replaced with 11 minutes left: %q", tk.Secret())
	}
	now = now.Add(time.Minute) // 10 minutes left: replaced
	if tk, _ = push.Token(ctx); tk.Secret() != "tok-3" {
		t.Errorf("token not replaced with 10 minutes left: %q", tk.Secret())
	}
	if len(st.revoked) != 0 {
		t.Errorf("revoked during the run: %v", st.revoked)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(st.revoked) != "[tok-1 tok-2 tok-3]" {
		t.Errorf("revoked %v, want every token of the run", st.revoked)
	}
	if _, err := push.Token(ctx); !errors.Is(err, ErrRunClosed) {
		t.Errorf("lease after Close: %v", err)
	}
	if _, err := read.Token(ctx); !errors.Is(err, ErrRunClosed) {
		t.Errorf("lease after Close: %v", err)
	}
	if _, err := r.Issue(ctx, ports.IdentityCoder, ports.Scope{}); !errors.Is(err, ErrRunClosed) {
		t.Errorf("Issue after Close: %v", err)
	}
}

func TestRunCloseReportsAndRetriesFailedRevocations(t *testing.T) {
	st := &fakeStore{now: time.Now, failRev: map[string]bool{"tok-2": true}}
	r := NewRun(st)
	ctx := context.Background()
	for range 3 {
		if _, err := r.Issue(ctx, ports.IdentityLoop, ports.Scope{}); err != nil {
			t.Fatal(err)
		}
	}
	err := r.Close(ctx)
	if !errors.Is(err, ports.ErrTransient) || fmt.Sprint(st.revoked) != "[tok-1 tok-3]" {
		t.Fatalf("Close = %v, revoked %v", err, st.revoked)
	}
	st.failRev = nil
	if err := r.Close(ctx); err != nil || fmt.Sprint(st.revoked) != "[tok-1 tok-3 tok-2]" {
		t.Errorf("retry: %v, revoked %v", err, st.revoked)
	}
	if err := r.Close(ctx); err != nil || len(st.revoked) != 3 {
		t.Errorf("third Close revoked again: %v", st.revoked)
	}
}

func TestRunIssueError(t *testing.T) {
	st := &fakeStore{now: time.Now, failIssue: ports.ErrPermission}
	if _, err := NewRun(st).Issue(context.Background(), ports.IdentityCoder, ports.Scope{}); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("err = %v", err)
	}
}

// A run's tokens from the GitHub store end up revoked at GitHub.
func TestRunWithGitHubApps(t *testing.T) {
	g, f := newStore(t)
	r := NewRun(g)
	r.Now = g.Now
	ctx := context.Background()
	for _, u := range []Use{UseCoderPush, UseCoderRead} {
		id, sc, _ := ScopeFor(u, "owner/repo")
		if _, err := r.Issue(ctx, id, sc); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.revoked) != 2 {
		t.Errorf("GitHub saw %d revocations, want 2", len(f.revoked))
	}
}
