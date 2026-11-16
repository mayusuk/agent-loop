package secrets

import (
	"fmt"
	"maps"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// Use is what a token is for. Each use has one row of INTEGRATION-SPEC §9.1,
// which fixes its identity and permissions.
type Use string

const (
	// UseCoderPush is the "Coder, handler" token, served to git by the
	// credential helper in implement, address and mint.
	UseCoderPush Use = "coder-push"
	// UseCoderRead is the coder stages' GH_TOKEN: the "Coder, handler"
	// permissions with contents read only (work plan D5).
	UseCoderRead Use = "coder-read"
	// UseCoderWorker is the "Coder, worker" token. Never given to a handler.
	UseCoderWorker Use = "coder-worker"
	// UseReviewerRead is the "Reviewer, handler" token, the review stage's
	// GH_TOKEN.
	UseReviewerRead Use = "reviewer-read"
	// UseReviewerWorker is the "Reviewer, worker" token.
	UseReviewerWorker Use = "reviewer-worker"
	// UseLoop is the loop identity's token, for adapter activities only.
	UseLoop Use = "loop"
)

type useSpec struct {
	identity ports.Identity
	perms    map[string]string
}

// uses is INTEGRATION-SPEC §9.1. Permission names are GitHub's.
var uses = map[Use]useSpec{
	UseCoderPush: {ports.IdentityCoder, map[string]string{
		"contents": "write", "issues": "read", "pull_requests": "read", "metadata": "read"}},
	UseCoderRead: {ports.IdentityCoder, map[string]string{
		"contents": "read", "issues": "read", "pull_requests": "read", "metadata": "read"}},
	UseCoderWorker: {ports.IdentityCoder, map[string]string{
		"pull_requests": "write", "issues": "read", "contents": "write"}},
	UseReviewerRead: {ports.IdentityReviewer, map[string]string{
		"contents": "read", "issues": "read", "pull_requests": "read"}},
	UseReviewerWorker: {ports.IdentityReviewer, map[string]string{
		"pull_requests": "write", "contents": "read"}},
	UseLoop: {ports.IdentityLoop, map[string]string{
		"issues": "write", "pull_requests": "write", "contents": "write",
		"checks": "read", "statuses": "read", "administration": "read"}},
}

// ScopeFor returns the identity and the narrowed scope of a token for use u
// on the repository repo ("owner/name").
func ScopeFor(u Use, repo string) (ports.Identity, ports.Scope, error) {
	s, ok := uses[u]
	if !ok {
		return "", ports.Scope{}, fmt.Errorf("unknown token use %q", u)
	}
	return s.identity, ports.Scope{Repos: []string{repo}, Permissions: maps.Clone(s.perms)}, nil
}

// StageUses returns the tokens a handler stage gets (INTEGRATION-SPEC §3.4):
// the push token for the credential helper, empty when the stage gets none,
// and the read token for GH_TOKEN.
func StageUses(stage string) (push, read Use, err error) {
	switch stage {
	case "implement", "address", "mint":
		return UseCoderPush, UseCoderRead, nil
	case "review":
		return "", UseReviewerRead, nil // FR-13 AC2: no push token
	}
	return "", "", fmt.Errorf("stage %q runs no handler", stage)
}
