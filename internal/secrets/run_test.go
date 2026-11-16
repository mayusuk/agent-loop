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

// blockingStore's Token waits for release before it issues.
type blockingStore struct {
	*fakeStore
	entered, release chan struct{}
}

func (s *blockingStore) Token(ctx context.Context, id ports.Identity, sc ports.Scope) (ports.Token, error) {
	s.entered <- struct{}{}
	<-s.release
	return s.fakeStore.Token(ctx, id, sc)
}

// Review R1, R6: a token that arrives after Close started is revoked at
// once, with a context the run's cancellation doesn't end, and kept for the
// next Close when that revoke fails (D13).
func TestRunCloseWhileATokenIsIssued(t *testing.T) {
	for _, failLate := range []bool{false, true} {
		fs := &fakeStore{now: time.Now, failRev: map[string]bool{}}
		if failLate {
			fs.failRev["tok-1"] = true
		}
		st := &blockingStore{fakeStore: fs, entered: make(chan struct{}), release: make(chan struct{})}
		r := NewRun(st)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error)
		go func() {
			_, err := r.Issue(ctx, ports.IdentityCoder, ports.Scope{})
			done <- err
		}()
		<-st.entered
		cancel() // the run is cancelled, then closed
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(st.release)
		err := <-done
		if !errors.Is(err, ErrRunClosed) {
			t.Fatalf("failLate=%v: Issue = %v, want ErrRunClosed", failLate, err)
		}
		if !failLate {
			if fmt.Sprint(fs.revoked) != "[tok-1]" {
				t.Errorf("late token not revoked: %v", fs.revoked)
			}
			continue
		}
		if !errors.Is(err, ports.ErrTransient) || len(fs.revoked) != 0 {
			t.Fatalf("Issue = %v, revoked %v", err, fs.revoked)
		}
		fs.mu.Lock()
		fs.failRev = nil
		fs.mu.Unlock()
		if err := r.Close(context.Background()); err != nil || fmt.Sprint(fs.revoked) != "[tok-1]" {
			t.Errorf("retry Close = %v, revoked %v", err, fs.revoked)
		}
	}
}

// ctxStore fails Revoke on a cancelled context, as an HTTP call would.
type ctxStore struct{ *fakeStore }

func (s ctxStore) Revoke(ctx context.Context, t ports.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fakeStore.Revoke(ctx, t)
}

func TestRunLateRevokeIgnoresTheRunsCancellation(t *testing.T) {
	fs := &fakeStore{now: time.Now}
	st := &blockingStore{fakeStore: fs, entered: make(chan struct{}), release: make(chan struct{})}
	r := NewRun(ctxStoreWrap{st})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := r.Issue(ctx, ports.IdentityCoder, ports.Scope{})
		done <- err
	}()
	<-st.entered
	cancel()
	r.Close(context.Background())
	close(st.release)
	if err := <-done; !errors.Is(err, ErrRunClosed) || errors.Is(err, context.Canceled) {
		t.Fatalf("Issue = %v", err)
	}
	if fmt.Sprint(fs.revoked) != "[tok-1]" {
		t.Errorf("revoked %v", fs.revoked)
	}
}

// ctxStoreWrap is a blocking store whose Revoke honours cancellation.
type ctxStoreWrap struct{ *blockingStore }

func (s ctxStoreWrap) Revoke(ctx context.Context, t ports.Token) error {
	return ctxStore{s.fakeStore}.Revoke(ctx, t)
}

// Review R8: a failed refresh keeps the current token while it is valid.
func TestLeaseKeepsTheCurrentTokenWhenARefreshFails(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := &fakeStore{now: clock}
	r := NewRun(st)
	r.Now = clock
	ctx := context.Background()
	l, err := r.Issue(ctx, ports.IdentityCoder, ports.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(55 * time.Minute)
	st.failIssue = ports.ErrTransient
	if tk, err := l.Token(ctx); err != nil || tk.Secret() != "tok-1" {
		t.Errorf("refresh failure with 5 minutes left: %q, %v; want tok-1", tk.Secret(), err)
	}
	st.failIssue = nil
	if tk, err := l.Token(ctx); err != nil || tk.Secret() != "tok-2" {
		t.Errorf("next call: %q, %v; want a new token", tk.Secret(), err)
	}
	now = now.Add(time.Hour) // tok-2 expired
	st.failIssue = ports.ErrTransient
	if _, err := l.Token(ctx); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("refresh failure after expiry: %v", err)
	}
	r.Close(ctx)
	if _, err := l.Token(ctx); !errors.Is(err, ErrRunClosed) {
		t.Errorf("after Close: %v", err)
	}
}
