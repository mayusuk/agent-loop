package github_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

func describe(evs []ports.ChangeEvent) []string {
	var out []string
	for _, e := range evs {
		s := fmt.Sprintf("%s %T", e.Change, e.Detail)
		switch d := e.Detail.(type) {
		case ports.Opened:
			s += " " + string(d.Item) + " " + d.Actor.Login
		case ports.HeadMoved:
			s += " " + string(d.Pusher)
		case ports.ReviewSubmitted:
			s += " " + d.Actor.Login
		case ports.ReviewDismissed:
			s += " " + d.Actor.Login
		case ports.ThreadResolved:
			s += " " + d.Actor.Login
		case ports.Merged:
			s += " " + d.Actor.Login
		case ports.ChangeClosed:
			s += " " + d.Actor.Login
		}
		out = append(out, s)
	}
	return out
}

func sameList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// TestHostChangesMapsEvents: each source of the change feed (work plan
// D16).
func TestHostChangesMapsEvents(t *testing.T) {
	p := newStub(t, 100)
	owner, human := actorOf(p.owner), actorOf(p.human)
	_, cur, err := p.host.Changes(ctx, "")
	must(t, err)
	it := p.item(t, owner)
	ch, head := p.open(t, it, "agent/"+string(it))
	c := string(ch)
	evs, cur := drain(t, p.host, cur)
	sameList(t, "open", describe(evs), []string{c + " ports.Opened " + string(it) + " project-coder[bot]"})

	_, err = p.driver.Push(owner, "agent/"+string(it))
	must(t, err)
	th, err := p.driver.ReviewComment(human, ch, "conformance.txt", 1, "hm")
	must(t, err)
	must(t, p.driver.ResolveThread(human, ch, th))
	r, err := p.driver.SubmitReview(human, ch, "CHANGES_REQUESTED", "no", nil)
	must(t, err)
	must(t, p.driver.DismissReview(owner, ch, r))
	must(t, p.driver.SetGate(head, "ci", ports.GatePending))
	evs, cur = drain(t, p.host, cur)
	sameList(t, "activity", describe(evs), []string{
		c + " ports.HeadMoved owner", c + " ports.ReviewSubmitted alice", c + " ports.ReviewSubmitted alice",
		c + " ports.ReviewDismissed owner", c + " ports.ThreadResolved alice"})

	newHead, err := p.driver.Push(owner, "agent/"+string(it))
	must(t, err)
	_, cur = drain(t, p.host, cur)
	must(t, p.driver.SetGate(newHead, "ci", ports.GatePass))
	must(t, p.driver.UnresolveThread(owner, ch, th))
	evs, cur = drain(t, p.host, cur)
	sameList(t, "gate and unresolve", describe(evs), []string{c + " ports.ThreadUnresolved", c + " ports.GateChanged"})

	must(t, p.driver.CloseChange(owner, ch))
	must(t, p.driver.ReopenChange(owner, ch))
	evs, cur = drain(t, p.host, cur)
	sameList(t, "close and reopen", describe(evs), []string{c + " ports.ChangeClosed owner", c + " ports.ChangeReopened"})

	out, err := p.host.Merge(ctx, ch, newHead, ports.MergeMethodSquash, "k")
	if err != nil || out != ports.MergeMerged {
		t.Fatalf("merge: %v %v", out, err)
	}
	evs, cur = drain(t, p.host, cur)
	sameList(t, "merge", describe(evs), []string{c + " ports.Merged project-loop[bot]"})
	if evs, _ = drain(t, p.host, cur); len(evs) != 0 {
		t.Errorf("events repeated: %v", describe(evs))
	}
	for _, bad := range []ports.Cursor{"x", "gh1h:!!", "gh1h:e30"} {
		if _, _, err := p.host.Changes(ctx, bad); !errors.Is(err, ports.ErrConflict) {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
}

// TestHostChangesPagesAcrossPulls: one page per call across two pull
// requests, oldest first, nothing skipped or repeated, and a closed pull
// request that left the cursor is read again when it is reopened.
func TestHostChangesPagesAcrossPulls(t *testing.T) {
	p := newStub(t, 2)
	owner := actorOf(p.owner)
	a, b := p.item(t, owner), p.item(t, owner)
	cha, _ := p.open(t, a, "agent/"+string(a))
	chb, _ := p.open(t, b, "agent/"+string(b))
	_, cur, err := p.host.Changes(ctx, "")
	must(t, err)
	var want []string
	for i := 0; i < 3; i++ {
		for _, x := range []struct {
			ch ports.ChangeRef
			it ports.ItemRef
		}{{cha, a}, {chb, b}} {
			_, err := p.driver.Push(owner, "agent/"+string(x.it))
			must(t, err)
			want = append(want, string(x.ch)+" ports.HeadMoved owner")
		}
	}
	first, next, err := p.host.Changes(ctx, cur)
	must(t, err)
	if len(first) != 2 {
		t.Fatalf("first page: %d events", len(first))
	}
	rest, cur := drain(t, p.host, next)
	sameList(t, "pages", describe(append(first, rest...)), want)

	must(t, p.driver.CloseChange(owner, cha))
	_, cur = drain(t, p.host, cur)
	p.srv.Advance(3600e9)
	_, err = p.driver.Push(owner, "agent/"+string(b)) // moves the window past cha's close
	must(t, err)
	_, cur = drain(t, p.host, cur)
	must(t, p.driver.ReopenChange(owner, cha))
	evs, _ := drain(t, p.host, cur)
	sameList(t, "reopen after leaving the cursor", describe(evs), []string{string(cha) + " ports.ChangeReopened"})
}
