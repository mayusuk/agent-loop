package github_test

import (
	"testing"

	"github.com/mayusuk/agent-loop/conformance"
)

func (p *stubProject) env() conformance.Env {
	return conformance.Env{
		Tracker: p.tracker, Driver: p.driver,
		Owner: actorOf(p.owner), Human: actorOf(p.human), Bot: actorOf(p.bot),
		Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop),
		BaseBranch: "main", BranchPrefix: "agent/", Gates: []string{"ci"}, FeedPage: 5,
	}
}

// TestConformance runs the tracker cases against the stub GitHub, with a
// feed page of 5 so C12 crosses pages (work plan D13, D14).
func TestConformance(t *testing.T) {
	conformance.RunTracker(t, func(t *testing.T) conformance.Env { return newStub(t, 5).env() })
}
