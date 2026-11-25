package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
	"github.com/mayusuk/agent-loop/internal/secrets"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// GitOps are the git operations the worker makes itself, outside any
// handler (INTEGRATION-SPEC §5.2, §5.3). workspace.Git implements them.
type GitOps interface {
	RemoteHead(ctx context.Context, repo ports.RepoRef, branch string) (ports.Commit, error)
	FetchedHead(ctx context.Context, d ports.Dir, branch string) (ports.Commit, error)
	MergeTree(ctx context.Context, d ports.Dir, ours, theirs ports.Commit) (tree string, conflicts []string, err error)
	CommitMerge(ctx context.Context, d ports.Dir, tree string, parents []ports.Commit, msg string, sig workspace.Signature) (ports.Commit, error)
	Push(ctx context.Context, d ports.Dir, repo ports.RepoRef, commit ports.Commit, branch string, tok ports.Token) error
	Parents(ctx context.Context, d ports.Dir, c ports.Commit) ([]ports.Commit, error)
	IsAncestor(ctx context.Context, d ports.Dir, a, b ports.Commit) (bool, error)
	FileLines(ctx context.Context, d ports.Dir, c ports.Commit, path string) (int, error)
	ResolvedPaths(ctx context.Context, d ports.Dir, c ports.Commit) ([]string, error)
}

var _ GitOps = (*workspace.Git)(nil)

// CoderRuns records each coder run (implement, address, and the worker's
// own base merge): its item, start and end, and the head it left. The trust
// check works out who pushed a head from these records (INTEGRATION-SPEC
// §9.8 step 6, I11).
type CoderRuns interface {
	Started(ctx context.Context, item ports.ItemRef, runID string, at time.Time) error
	Ended(ctx context.Context, item ports.ItemRef, runID string, at time.Time, head ports.Commit) error
}

// StageConfig is one handler stage's settings from the profile.
type StageConfig struct {
	Command   []string
	Runner    string // "local" (also when empty) or "local-unsandboxed"
	TimeLimit time.Duration
}

// Config is what the worker's activities need from the project profile and
// the deployment.
type Config struct {
	Project         string
	TrackerAdapter  string // "github"
	CodeHostAdapter string // "github"
	Repo            ports.RepoRef
	// RepoName is the repository as token scopes name it: "owner/name".
	RepoName          string
	BranchPrefix      string // "agent/"
	BlockingThreshold int
	// RequireApproval: branch protection requires an approving review, so a
	// review with no blocking finding approves (FR-4 AC7).
	RequireApproval bool
	MergeMethod     ports.MergeMethod
	Stages          map[contract.Stage]StageConfig
	Egress          []string
	Owners          []ports.OwnerAccount
	// Identities are the account IDs the supervisor matches actors with
	// (FR-13 AC7). Empty Owners means the IDs of Owners above.
	Identities core.Identities
	// CoderSignature is the author and committer of the worker's base merge
	// commits: the coder identity (INTEGRATION-SPEC §5.3).
	CoderSignature workspace.Signature
	// FollowUpType is the follow-up's type marker (FR-8 AC5), and
	// FollowUpReady whether it gets the ready marker (FR-8 AC2).
	FollowUpType  ports.Marker
	FollowUpReady bool
	// RunsDir holds one directory per run: input.json, result.json, the out
	// directory and the redacted log. Keep it outside the workspace.
	RunsDir string
}

// Activities are the worker's activities. The workflows (T16) register its
// exported methods; every argument and result is plain data, and no token
// or untrusted text crosses that boundary.
type Activities struct {
	Config
	Tracker   ports.WorkTracker
	Host      ports.CodeHost
	Workspace ports.Workspace
	Runner    ports.Runner
	Secrets   ports.SecretStore
	Git       GitOps
	Notifier  ports.Notifier
	// CoderRuns records coder runs; projection.CoderRuns implements it.
	CoderRuns CoderRuns
	// Projection is the read model. Nil writes nothing.
	Projection Projection
	// HostName is the host this worker runs on. RunStage records it on
	// the run, for the board (FR-11 AC1, work plan #105 D3). Empty
	// records none.
	HostName string
	// Redactor masks the run's tokens in posted text. Nil makes one.
	Redactor *secrets.Redactor
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Heartbeat, when set, is called every HeartbeatEvery while a handler
	// runs (TECHNICAL-SPEC §6.5): the workflows set it to Temporal's
	// activity heartbeat.
	Heartbeat      func(ctx context.Context)
	HeartbeatEvery time.Duration
	// Retries and RetryDelay bound the retries of one record write after a
	// transient error inside RunStage, which itself is never retried
	// (TECHNICAL-SPEC §6.5). Zero means 3 tries, 1 s apart.
	Retries    int
	RetryDelay time.Duration
}

func (a *Activities) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Activities) redactor() *secrets.Redactor {
	if a.Redactor == nil {
		a.Redactor = secrets.NewRedactor()
	}
	return a.Redactor
}

// retry runs a write until it succeeds, fails with an error that isn't
// transient, or runs out of tries. Every write it retries has an
// idempotency key, so a retry after a write that did land leaves one
// result (NFR-2).
func (a *Activities) retry(ctx context.Context, f func() error) error {
	tries, delay := a.Retries, a.RetryDelay
	if tries <= 0 {
		tries = 3
	}
	if delay <= 0 {
		delay = time.Second
	}
	var err error
	for i := 0; i < tries; i++ {
		if err = f(); err == nil || !errors.Is(err, ports.ErrTransient) {
			return err
		}
		if i < tries-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return err
}

// The token scopes (INTEGRATION-SPEC §3.4, §9.1, TECHNICAL-SPEC §7.3).
func (a *Activities) scope(perms map[string]string) ports.Scope {
	return ports.Scope{Repos: []string{a.RepoName}, Permissions: perms}
}

// handlerPushScope is the coder handler's push token: contents write on the
// project repository, read access to the rest.
func (a *Activities) handlerPushScope() ports.Scope {
	return a.scope(map[string]string{"contents": "write", "metadata": "read", "issues": "read", "pull_requests": "read"})
}

// handlerReadScope is GH_TOKEN: read-only.
func (a *Activities) handlerReadScope() ports.Scope {
	return a.scope(map[string]string{"contents": "read", "metadata": "read", "issues": "read", "pull_requests": "read"})
}

// coderWorkerScope is the coder worker token, which never reaches a
// handler; the base merge pushes with it.
func (a *Activities) coderWorkerScope() ports.Scope {
	return a.scope(map[string]string{"contents": "write", "metadata": "read", "issues": "read", "pull_requests": "write"})
}

// record encodes rec as a port record.
func record(rec contract.Record) (ports.Record, error) {
	if rec.V == 0 {
		rec.V = 1
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return ports.Record{}, err
	}
	return ports.Record{Type: ports.RecordType(rec.Type), Key: ports.IdemKey(rec.Key), Data: data}, nil
}

// text redacts s and wraps it as untrusted text for a port.
func (a *Activities) text(s string) ports.UntrustedText {
	return untrusted.New(a.redactor().Redact(s))
}

// readOut reads a result's text file, already checked by ValidateResult.
func readOut(outDir, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(outDir, rel))
	if err != nil {
		return "", fmt.Errorf("result file %s: %w", rel, err)
	}
	return string(b), nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// runDir is the run's own directory under RunsDir.
func (a *Activities) runDir(runID string) (string, error) {
	if a.RunsDir == "" {
		return "", errors.New("workflows: no runs directory")
	}
	d := filepath.Join(a.RunsDir, unsafeName.ReplaceAllString(runID, "_"))
	if err := os.MkdirAll(filepath.Join(d, "out"), 0o700); err != nil {
		return "", err
	}
	return d, nil
}
