package ports

import (
	"context"
	"errors"
	"time"
)

// The internal ports of TECHNICAL-SPEC §5. Their contract for handlers is
// INTEGRATION-SPEC §3. The tasks that implement them own their argument
// types and may add fields.

// Workspace prepares a working directory for a run.
type Workspace interface {
	Prepare(ctx context.Context, repo RepoRef, b Branch) (Dir, error) // cached clone + fresh worktree per run
	Release(ctx context.Context, d Dir) error
}

// RepoRef names the project repository.
type RepoRef struct {
	URL        string
	BaseBranch string
}

// Branch is what the worktree is checked out on (INTEGRATION-SPEC §3.3).
type Branch struct {
	Name string // the branch to check out, or to create from the base tip
	// Detach, when set, checks out this commit detached instead (review).
	Detach Commit
}

// Dir is a prepared worktree.
type Dir struct {
	Path   string
	Branch string
	Head   Commit
}

// Runner runs one handler.
type Runner interface {
	Run(ctx context.Context, s RunSpec) (RunOutcome, error) // command, workspace, env, limits, egress policy
}

// RunSpec is one handler run.
type RunSpec struct {
	RunID      string // INTEGRATION-SPEC §4.1 run_id
	Command    []string
	Dir        Dir
	RepoURL    string // the repository's URL; the credential helper serves only its host
	Env        map[string]string
	InputPath  string // AGENT_LOOP_INPUT
	ResultPath string // AGENT_LOOP_RESULT
	OutDir     string // AGENT_LOOP_OUT
	LogPath    string // where the redacted stdout and stderr go
	TimeLimit  time.Duration
	// Runner is the stage's runner from the profile: "local" (also when
	// empty) or "local-unsandboxed" (TECHNICAL-SPEC §7.2).
	Runner string
	Egress []string    // allowed domains
	Push   TokenSource // served by the credential helper; nil for review
	Read   TokenSource // GH_TOKEN
	// Author is the author and committer of the commits the handler makes:
	// the coder bot for implement and address, zero for review. When set,
	// the owner's global and system git config doesn't apply to the run.
	Author Signature
}

// Signature is the author and committer of a commit.
type Signature struct {
	Name, Email string
}

// TokenSource gives a run's current token, renewing it near its expiry
// (secrets.Lease).
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// StaticToken is a TokenSource that always gives the same token.
type StaticToken Token

// Token returns the token.
func (t StaticToken) Token(context.Context) (Token, error) { return Token(t), nil }

// String returns a description without the secret.
func (t StaticToken) String() string { return Token(t).String() }

// GoString returns a description without the secret.
func (t StaticToken) GoString() string { return Token(t).String() }

// MarshalJSON always fails with ErrTokenSerialize.
func (StaticToken) MarshalJSON() ([]byte, error) { return nil, ErrTokenSerialize }

// RunOutcome is how a run ended. The worker validates the result file itself.
type RunOutcome struct {
	ExitCode   int
	TimedOut   bool
	ResultPath string
	Started    time.Time
	Ended      time.Time
}

// SecretStore issues and revokes short-lived tokens.
type SecretStore interface {
	Token(ctx context.Context, id Identity, s Scope) (Token, error) // short-lived where the tracker allows
	Revoke(ctx context.Context, t Token) error                      // at the end of each run (TECHNICAL-SPEC §7.3)
}

// Scope narrows a token (INTEGRATION-SPEC §9.1).
type Scope struct {
	Repos       []string
	Permissions map[string]string // for example "contents": "write"
}

// Token is a short-lived credential. The secret is read only with Secret,
// and fmt prints the token redacted (FR-13 AC3). The secret sits behind a
// *string, which fmt prints as an address, not the secret, even when a Token
// is held in an unexported struct field (see untrusted.Text).
type Token struct {
	ID        string // the issuer's ID for the token, if any
	Identity  Identity
	Scope     Scope
	ExpiresAt time.Time
	secret    *string
}

// NewToken returns a token holding secret.
func NewToken(secret string, id Identity, s Scope, expiresAt time.Time) Token {
	return Token{Identity: id, Scope: s, ExpiresAt: expiresAt, secret: &secret}
}

// Secret returns the token's secret, or "" for the zero Token.
func (t Token) Secret() string {
	if t.secret == nil {
		return ""
	}
	return *t.secret
}

// String returns a description without the secret.
func (t Token) String() string { return "token(" + string(t.Identity) + ", redacted)" }

// GoString returns a description without the secret.
func (t Token) GoString() string { return t.String() }

// ErrTokenSerialize is returned when a Token is encoded or decoded as JSON.
// A token never crosses a serialisation boundary such as workflow history.
var ErrTokenSerialize = errors.New("token can't be serialised")

// MarshalJSON always fails with ErrTokenSerialize.
func (Token) MarshalJSON() ([]byte, error) { return nil, ErrTokenSerialize }

// UnmarshalJSON always fails with ErrTokenSerialize.
func (*Token) UnmarshalJSON([]byte) error { return ErrTokenSerialize }

// Notifier sends a notification to the owner (TECHNICAL-SPEC §12).
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Notification is one message to the owner, written by the loop.
type Notification struct {
	Project string
	Item    ItemRef
	Reason  string // for example an escalation reason code
	Message string
	URL     string
	Key     IdemKey
}
