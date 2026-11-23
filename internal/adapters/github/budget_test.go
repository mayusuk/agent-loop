package github_test

import (
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// callCost is the calls made between two points of the stub's request log:
// REST calls that count against the limit (a 304 answered from the ETag
// cache doesn't), REST calls answered 304, and GraphQL queries.
type callCost struct{ rest, notModified, graphql int }

func (p *stubProject) costSince(n int) callCost {
	var c callCost
	for _, r := range p.srv.Requests()[n:] {
		switch {
		case r.Path == "/graphql":
			c.graphql++
		case r.Status == 304:
			c.notModified++
		default:
			c.rest++
		}
	}
	return c
}

// TestQ4_PollingBudget measures TECHNICAL-SPEC Q4 against the stub: the
// calls of one idle poll of both feeds with two claimed items (WIP 2), and
// of one merge check, which a merging item makes every poll interval while
// a gate is pending. It projects them over an hour at the default 60 s poll
// and checks they fit GitHub's 5,000 requests an hour per installation
// with room to spare (work plan #25 D8 records the numbers).
func TestQ4_PollingBudget(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	var changes []ports.ChangeRef
	for i := 0; i < 2; i++ {
		it := p.item(t, owner, ports.MarkerReady, ports.MarkerInProgress)
		ch, _ := p.open(t, it, "agent/"+string(it))
		changes = append(changes, ch)
	}
	_, tcur, err := p.tracker.Changes(ctx, "")
	must(t, err)
	_, hcur := drain(t, p.host, "")

	poll := func() callCost {
		n := len(p.srv.Requests())
		_, tc, err := p.tracker.Changes(ctx, tcur)
		must(t, err)
		_, hc, err := p.host.Changes(ctx, hcur)
		must(t, err)
		tcur, hcur = tc, hc
		return p.costSince(n)
	}
	check := func(ch ports.ChangeRef) callCost {
		n := len(p.srv.Requests())
		c, err := p.host.Change(ctx, ch)
		must(t, err)
		_, err = p.host.Trust(ctx, ch)
		must(t, err)
		_, err = p.host.ContainsBase(ctx, ch)
		must(t, err)
		_, err = p.host.Gates(ctx, ch, c.Head)
		must(t, err)
		_, err = p.host.ManualComments(ctx, ch)
		must(t, err)
		return p.costSince(n)
	}
	poll() // warm the ETag cache
	idle := poll()
	p.srv.Advance(time.Minute)
	merge := check(changes[0])
	p.srv.Advance(time.Minute)
	again := check(changes[0])

	rest, gql := 60*(idle.rest+2*again.rest), 60*(idle.graphql+2*again.graphql)
	t.Logf("Q4 budget (stub, WIP 2): idle poll %+v; first merge check %+v; repeated merge check %+v", idle, merge, again)
	t.Logf("Q4 budget: both items merging with a pending gate at a 60 s poll: %d counted REST calls and %d GraphQL queries an hour", rest, gql)
	// GraphQL is charged in points, at least one a query; the live run
	// reads the real charge from /rate_limit (docs/LIVE-SCENARIO.md).
	if rest > 2500 || gql > 2500 {
		t.Fatalf("an hour of polling costs %d REST calls and %d GraphQL queries, over half of GitHub's 5,000", rest, gql)
	}
	if idle.rest+idle.graphql == 0 && idle.notModified == 0 {
		t.Fatal("the idle poll made no call, so the measurement proves nothing")
	}
}
