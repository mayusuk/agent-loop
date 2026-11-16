package secrets

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// RefreshBefore is how long before its expiry a lease replaces its token
// (work plan D7).
const RefreshBefore = 10 * time.Minute

// ErrRunClosed is returned for tokens asked of a run that has ended.
var ErrRunClosed = errors.New("the run's tokens are revoked")

// Run is the set of tokens issued for one run (or one worker activity).
// Close revokes every one of them (TECHNICAL-SPEC §7.3, INTEGRATION-SPEC
// §9.8 step 6). It is safe for concurrent use.
type Run struct {
	store ports.SecretStore
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	issued []ports.Token
	closed bool
}

// NewRun starts a run's token set on store.
func NewRun(store ports.SecretStore) *Run { return &Run{store: store} }

func (r *Run) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Issue gets a token for identity id narrowed to s, as a lease that renews
// it near its expiry.
func (r *Run) Issue(ctx context.Context, id ports.Identity, s ports.Scope) (*Lease, error) {
	l := &Lease{run: r, id: id, scope: s}
	if _, err := l.Token(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

func (r *Run) issue(ctx context.Context, id ports.Identity, s ports.Scope) (ports.Token, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ports.Token{}, ErrRunClosed
	}
	t, err := r.store.Token(ctx, id, s)
	if err != nil {
		// A store may hand back a token it couldn't revoke together with
		// the error (GitHubApps on a grant beyond the request). Keep it so
		// Close revokes it.
		if t.Secret() != "" {
			r.mu.Lock()
			r.issued = append(r.issued, t)
			r.mu.Unlock()
		}
		return ports.Token{}, err
	}
	r.mu.Lock()
	closed = r.closed
	if !closed {
		r.issued = append(r.issued, t)
	}
	r.mu.Unlock()
	if !closed {
		return t, nil
	}
	// Closed while the token was being issued: revoke it now, with a
	// context the run's cancellation doesn't end. If that fails, keep it,
	// so the next Close retries it.
	rerr := r.store.Revoke(context.WithoutCancel(ctx), t)
	if rerr != nil {
		r.mu.Lock()
		r.issued = append(r.issued, t)
		r.mu.Unlock()
	}
	return ports.Token{}, errors.Join(ErrRunClosed, rerr)
}

// Close revokes every token the run was issued, including ones a lease has
// replaced, and refuses new ones. It reports every failed revocation.
// Pass a context that outlives the run's own, so a cancelled run still
// revokes. Calling it again retries the failed revocations.
func (r *Run) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	toks := r.issued
	r.issued = nil
	r.mu.Unlock()
	var errs []error
	var failed []ports.Token
	for _, t := range toks {
		if err := r.store.Revoke(ctx, t); err != nil {
			errs = append(errs, err)
			failed = append(failed, t)
		}
	}
	r.mu.Lock()
	r.issued = append(r.issued, failed...)
	r.mu.Unlock()
	return errors.Join(errs...)
}

// Lease is one token of a run that renews itself near its expiry, for the
// credential helper (TECHNICAL-SPEC §7.3).
type Lease struct {
	run   *Run
	id    ports.Identity
	scope ports.Scope

	mu  sync.Mutex
	cur ports.Token
}

// Token returns the lease's token, first getting a new one when the
// current one expires within RefreshBefore. The old token stays live until
// the run closes, because a git operation may still be using it. If getting
// the new token fails while the current one hasn't expired, the current
// one is returned and the next call tries again.
func (l *Lease) Token(ctx context.Context) (ports.Token, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur.Secret() != "" && l.run.now().Add(RefreshBefore).Before(l.cur.ExpiresAt) {
		l.run.mu.Lock()
		closed := l.run.closed
		l.run.mu.Unlock()
		if closed {
			return ports.Token{}, ErrRunClosed
		}
		return l.cur, nil
	}
	t, err := l.run.issue(ctx, l.id, l.scope)
	if err != nil {
		if l.cur.Secret() != "" && !errors.Is(err, ErrRunClosed) && l.run.now().Before(l.cur.ExpiresAt) {
			return l.cur, nil
		}
		return ports.Token{}, err
	}
	l.cur = t
	return t, nil
}
