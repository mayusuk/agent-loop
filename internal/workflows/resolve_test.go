package workflows

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// refusingResolve is a code host that refuses every thread resolve, as
// GitHub refused the reviewer app (#100).
type refusingResolve struct {
	ports.CodeHost
	tries *atomic.Int32
}

func (r refusingResolve) ResolveFinding(context.Context, ports.ChangeRef, ports.FindingID, ports.IdemKey) error {
	r.tries.Add(1)
	return ports.ErrPermission
}

// TestFR4AC5_FailedResolveNeverFailsTheReviewRun: a finding thread that
// can't be resolved after round 2 rules the finding addressed doesn't fail
// the review run (no worker_lost, no crash rerun). ResolveFindings is
// retried on its own, a bounded number of times, and the item still merges
// on the statuses in the review records (owner decision on #100, work plan
// #100 D2).
func TestFR4AC5_FailedResolveNeverFailsTheReviewRun(t *testing.T) {
	s := newScenario(t)
	tries := &atomic.Int32{}
	s.a.Host = refusingResolve{CodeHost: s.a.Host, tries: tries}
	b := s.newItem("B")
	s.on(b, contract.StageImplement, s.impl())
	s.on(b, contract.StageReview, s.rev(review{add: []newF{{score: 4}}}))
	s.on(b, contract.StageAddress, s.addr(address{push: true, finding: func(string) contract.ReplyKind { return contract.ReplyFixed }}))
	s.on(b, contract.StageReview, s.rev(review{findings: map[string]contract.FindingStatus{"F1": contract.StatusAddressed}}))
	s.runProject()

	if !slices.Contains(s.itemStates(b), core.StateMerged) {
		t.Fatalf("B states %v", s.itemStates(b))
	}
	if got := s.stagesOf(b); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress, contract.StageReview}) {
		t.Fatalf("B's stages %v: a failed resolve must not rerun the review", got)
	}
	_, results := s.rec.stages()
	if slices.Contains(results, contract.ReasonWorkerLost) {
		t.Fatalf("run results %v: a failed resolve must not fail the run", results)
	}
	if n := tries.Load(); n != resolveAttempts {
		t.Fatalf("resolve tried %d times, want %d", n, resolveAttempts)
	}
}

// refusingOne is a code host that refuses the resolve of one finding and
// records every finding it was asked to resolve.
type refusingOne struct {
	ports.CodeHost
	refuse ports.FindingID
	got    []ports.FindingID
}

func (r *refusingOne) ResolveFinding(_ context.Context, _ ports.ChangeRef, f ports.FindingID, _ ports.IdemKey) error {
	r.got = append(r.got, f)
	if f == r.refuse {
		return ports.ErrPermission
	}
	return nil
}

// TestFR4AC5_ResolveFindingsTriesEveryFinding: a refused resolve of one
// finding doesn't stop the others; ResolveFindings returns an error that
// names the failing finding and wraps the host's error, so Temporal
// retries the request (work plan #100 D2). With no failure it returns nil.
func TestFR4AC5_ResolveFindingsTriesEveryFinding(t *testing.T) {
	for _, c := range []struct {
		name   string
		refuse ports.FindingID
	}{{"first refused", "F1"}, {"last refused", "F2"}, {"none refused", ""}} {
		t.Run(c.name, func(t *testing.T) {
			h := &refusingOne{refuse: c.refuse}
			a := &Activities{Host: h}
			err := a.ResolveFindings(context.Background(), ResolveRequest{Change: "7", Findings: []ResolveFinding{{ID: "F1", Key: "k1"}, {ID: "F2", Key: "k2"}}})
			if got := fmt.Sprint(h.got); got != "[F1 F2]" {
				t.Fatalf("resolved %s, want [F1 F2]", got)
			}
			if c.refuse == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ports.ErrPermission) || !strings.Contains(err.Error(), "resolve "+string(c.refuse)+" on change 7") {
				t.Fatalf("err = %v, want ErrPermission naming %s on change 7", err, c.refuse)
			}
		})
	}
}
