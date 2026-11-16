package fake_test

import (
	"testing"

	"github.com/mayusuk/agent-loop/conformance"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
)

// newEnv returns a fresh fake project as a conformance Env, with a small
// feed page so C12 crosses pages.
func newEnv(t *testing.T) (*fake.Fake, conformance.Env) {
	cfg := fake.DefaultConfig()
	cfg.PageSize = 5
	f := fake.New(cfg)
	return f, conformance.Env{
		Tracker: f.Tracker(), Host: f.Host(), Driver: f,
		Owner: fake.Owner, Human: fake.Human, Bot: fake.Bot,
		Coder: cfg.Coder, Reviewer: cfg.Reviewer, Loop: cfg.Loop,
		BaseBranch: cfg.BaseBranch, BranchPrefix: cfg.BranchPrefix,
		Gates: cfg.Gates, FeedPage: cfg.PageSize,
	}
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Env {
		_, e := newEnv(t)
		return e
	})
}
