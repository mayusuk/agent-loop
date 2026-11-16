package conformance

import (
	"github.com/mayusuk/agent-loop/internal/ports"
)

// c1Host is the code-host half of C1.
func c1Host(s *suite, it ports.ItemRef) {
	e := s.env()
	ctx := s.ctx()
	branch := e.BranchPrefix + string(it)
	var head ports.Commit
	s.coderRun(it, func() {
		var err error
		head, err = e.Driver.Push(e.Coder, branch)
		s.must(err)
	})
	_, cur := s.hostFeed("")
	count := func(pred func(ports.ChangeEvent) bool) int {
		evs, next := s.hostFeed(cur)
		cur = next
		n := 0
		for _, ev := range evs {
			if pred(ev) {
				n++
			}
		}
		return n
	}

	k := s.key("open")
	nc := ports.NewChange{Item: it, Branch: branch, Base: e.BaseBranch, Record: rec(ports.RecordChange, k)}
	ch1, err := e.Host.OpenChange(ctx, nc, ports.IdentityCoder, k)
	s.must(err)
	ch2, err := e.Host.OpenChange(ctx, nc, ports.IdentityCoder, k)
	s.must(err)
	if ch1 != ch2 {
		s.t.Errorf("OpenChange repeated: %s and %s", ch1, ch2)
	}
	ch := ch1
	count(func(ports.ChangeEvent) bool { return false })

	k = s.key("label")
	s.must(e.Host.SetLabel(ctx, ch, ports.MarkerAborted, true, k))
	s.must(e.Host.SetLabel(ctx, ch, ports.MarkerAborted, true, k))
	n := 0
	for _, m := range s.getChange(ch).Markers {
		if m == ports.MarkerAborted {
			n++
		}
	}
	if n != 1 {
		s.t.Errorf("SetLabel repeated: the change has the marker %d times", n)
	}

	k = s.key("review")
	post := ports.ReviewPost{Commit: head, Record: rec(ports.RecordReview, k), Findings: []ports.FindingPost{
		{ID: "F1", Score: 3, Status: "open", Path: ChangedFile, Line: 1, Record: rec(ports.RecordFinding, s.key("f1"))},
	}}
	r1, err := e.Host.PostReview(ctx, ch, post, k)
	s.must(err)
	r2, err := e.Host.PostReview(ctx, ch, post, k)
	s.must(err)
	rrs, err := e.Host.ReviewRecords(ctx, ch)
	s.must(err)
	if r1 != r2 || len(rrs) != 1 {
		s.t.Fatalf("PostReview repeated: refs %s, %s; %d review records", r1, r2, len(rrs))
	}
	thread := rrs[0].Threads["F1"]
	if thread == "" {
		s.t.Fatal("the finding with a path has no thread (I8)")
	}

	isReplyBy := func(ev ports.ChangeEvent) bool {
		rs, ok := ev.Detail.(ports.ReviewSubmitted)
		return ok && rs.Actor.ID == e.Coder.ID
	}
	k = s.key("reply")
	s.must(e.Host.PostReply(ctx, ch, thread, rec(ports.RecordReply, k), ports.UntrustedText{}, k))
	s.must(e.Host.PostReply(ctx, ch, thread, rec(ports.RecordReply, k), ports.UntrustedText{}, k))
	if n := count(isReplyBy); n > 1 {
		s.t.Errorf("PostReply repeated: %d coder replies, want 1", n)
	}
	k = s.key("reply-change")
	s.must(e.Host.PostReply(ctx, ch, ports.ThreadRef(ch), rec(ports.RecordReply, k), ports.UntrustedText{}, k))
	s.must(e.Host.PostReply(ctx, ch, ports.ThreadRef(ch), rec(ports.RecordReply, k), ports.UntrustedText{}, k))

	k = s.key("resolve")
	s.must(e.Host.ResolveFinding(ctx, ch, "F1", k))
	s.must(e.Host.ResolveFinding(ctx, ch, "F1", k))
	if n := count(func(ev ports.ChangeEvent) bool { _, ok := ev.Detail.(ports.ThreadResolved); return ok }); n != 1 {
		s.t.Errorf("ResolveFinding repeated: %d resolve events, want 1", n)
	}

	s.passGates(head)
	k = s.key("merge")
	for i := 0; i < 2; i++ {
		out, err := e.Host.Merge(ctx, ch, head, ports.MergeMethodSquash, k)
		if err != nil || out != ports.MergeMerged {
			s.t.Fatalf("Merge call %d: %v, %v", i+1, out, err)
		}
	}
	if n := count(func(ev ports.ChangeEvent) bool { _, ok := ev.Detail.(ports.Merged); return ok }); n != 1 {
		s.t.Errorf("Merge repeated: %d merge events, want 1", n)
	}
}

func (s *suite) find(mcs []ports.ManualComment, kind ports.ManualCommentKind, author ports.Actor) []ports.ManualComment {
	var out []ports.ManualComment
	for _, mc := range mcs {
		if mc.Kind == kind && mc.Author.ID == author.ID {
			out = append(out, mc)
		}
	}
	return out
}

// one returns the only manual comment of kind by author.
func (s *suite) one(ch ports.ChangeRef, kind ports.ManualCommentKind, author ports.Actor) ports.ManualComment {
	s.t.Helper()
	got := s.find(s.manual(ch), kind, author)
	if len(got) != 1 {
		s.t.Fatalf("got %d manual comments of kind %s by %s, want 1", len(got), kind, author.Login)
	}
	return got[0]
}

func (s *suite) cleared(what string, mc ports.ManualComment, want bool, by ports.ClearedBy) {
	s.t.Helper()
	if mc.Cleared != want || (want && mc.ClearedBy != by) || (!want && mc.ClearedBy != ports.ClearedByNone) {
		s.t.Errorf("%s: cleared=%v by %q, want cleared=%v by %q", what, mc.Cleared, mc.ClearedBy, want, by)
	}
}

// c6: a human resolve clears, an identity's resolve doesn't, unresolve
// blocks again, and dismissal works the same way.
func c6(s *suite) {
	e := s.env()
	it := s.item()
	ch, _ := s.loopChange(it)
	th, err := e.Driver.ReviewComment(e.Human, ch, ChangedFile, 1, "please rename this")
	s.must(err)
	mc := s.one(ch, ports.ManualCommentThread, e.Human)
	s.cleared("new comment", mc, false, "")
	if mc.Thread != th {
		s.t.Errorf("Thread = %s, want %s", mc.Thread, th)
	}
	for _, id := range []ports.Actor{e.Coder, e.Reviewer, e.Loop, e.Bot} {
		s.must(e.Driver.ResolveThread(id, ch, th))
		s.cleared("resolved by "+id.Login, s.one(ch, ports.ManualCommentThread, e.Human), false, "")
		s.must(e.Driver.UnresolveThread(id, ch, th))
	}
	s.must(e.Driver.ResolveThread(e.Human, ch, th))
	s.cleared("resolved by a human", s.one(ch, ports.ManualCommentThread, e.Human), true, ports.ClearedByHumanResolved)
	s.must(e.Driver.UnresolveThread(e.Owner, ch, th))
	s.cleared("unresolved", s.one(ch, ports.ManualCommentThread, e.Human), false, "")
	s.must(e.Driver.ResolveThread(e.Owner, ch, th))
	s.must(e.Driver.ReplyInThread(e.Owner, ch, th, "one more thing"))
	s.cleared("comment before the resolve", s.one(ch, ports.ManualCommentThread, e.Human), true, ports.ClearedByHumanResolved)
	s.cleared("comment after the resolve", s.one(ch, ports.ManualCommentThread, e.Owner), false, "")

	// A listed bot's comment is a manual comment too (I7).
	_, err = e.Driver.ReviewComment(e.Bot, ch, ChangedFile, 1, "possible injection")
	s.must(err)
	s.cleared("bot comment", s.one(ch, ports.ManualCommentThread, e.Bot), false, "")

	byLoop, err := e.Driver.SubmitReview(e.Human, ch, ReviewComment, "first review body", nil)
	s.must(err)
	s.must(e.Driver.DismissReview(e.Loop, ch, byLoop))
	byOwner, err := e.Driver.SubmitReview(e.Human, ch, ReviewChangesRequested, "second review body", nil)
	s.must(err)
	s.must(e.Driver.DismissReview(e.Owner, ch, byOwner))
	bodies := s.find(s.manual(ch), ports.ManualCommentReviewBody, e.Human)
	if len(bodies) != 2 {
		s.t.Fatalf("got %d review-body manual comments, want 2", len(bodies))
	}
	s.cleared("review dismissed by the loop identity", bodies[0], false, "")
	s.cleared("review dismissed by the owner", bodies[1], true, ports.ClearedByHumanDismissed)
}

// c7: Merge with an old pinned commit returns HeadMoved and doesn't merge.
func c7(s *suite) {
	e := s.env()
	it := s.item()
	ch, old := s.loopChange(it)
	s.passGates(old)
	head := s.coderPush(it, ch)
	s.passGates(head)
	out, err := e.Host.Merge(s.ctx(), ch, old, ports.MergeMethodSquash, s.key("merge"))
	s.must(err)
	if out != ports.MergeHeadMoved {
		s.t.Fatalf("Merge with an old pin = %s, want head_moved", out)
	}
	if c := s.getChange(ch); c.State != ports.ChangeStateOpen || c.Head != head {
		s.t.Fatalf("after a refused merge the change is %s at %s", c.State, c.Head)
	}
	out, err = e.Host.Merge(s.ctx(), ch, head, ports.MergeMethodSquash, s.key("merge"))
	s.must(err)
	if c := s.getChange(ch); out != ports.MergeMerged || c.State != ports.ChangeStateMerged || c.MergedBy == nil || c.MergedBy.ID != e.Loop.ID {
		s.t.Errorf("Merge at the head = %s; change %s merged by %v", out, c.State, c.MergedBy)
	}
}

// c8: ContainsBase is false after the base moves and true after the base is
// merged into the branch.
func c8(s *suite) {
	e := s.env()
	it := s.item()
	ch, _ := s.loopChange(it)
	contains := func() bool {
		ok, err := e.Host.ContainsBase(s.ctx(), ch)
		s.must(err)
		return ok
	}
	if !contains() {
		s.t.Error("a branch made from the base tip doesn't contain it")
	}
	_, err := e.Driver.Push(e.Owner, e.BaseBranch)
	s.must(err)
	if contains() {
		s.t.Error("ContainsBase is true after the base moved")
	}
	s.coderRun(it, func() {
		_, err := e.Driver.MergeBaseInto(e.Coder, s.getChange(ch).Branch)
		s.must(err)
	})
	if !contains() {
		s.t.Error("ContainsBase is false after the base was merged into the branch")
	}
}

// c9: Gates reports Pending, Missing, Pass and Fail for the head commit
// only, and only required gates.
func c9(s *suite) {
	e := s.env()
	it := s.item()
	ch, h1 := s.loopChange(it)
	gate := e.Gates[0]
	state := func(c ports.Commit) ports.GateState {
		s.t.Helper()
		res, err := e.Host.Gates(s.ctx(), ch, c)
		s.must(err)
		if len(res) != len(e.Gates) {
			s.t.Fatalf("Gates returned %d results, want one per required gate (%d): %+v", len(res), len(e.Gates), res)
		}
		for _, r := range res {
			if r.Commit != c {
				s.t.Errorf("gate %s reported for commit %s, asked for %s", r.Name, r.Commit, c)
			}
			if r.Name == gate {
				return r.State
			}
		}
		s.t.Fatalf("required gate %s not reported", gate)
		return ""
	}
	if got := state(h1); got != ports.GateMissing {
		s.t.Errorf("no run: %s, want missing", got)
	}
	s.must(e.Driver.SetGate(h1, "conformance-not-required", ports.GateFail))
	for _, want := range []ports.GateState{ports.GatePending, ports.GateFail, ports.GatePass} {
		s.must(e.Driver.SetGate(h1, gate, want))
		if got := state(h1); got != want {
			s.t.Errorf("gate set to %s, reported %s", want, got)
		}
	}
	h2 := s.coderPush(it, ch)
	if got := state(h2); got != ports.GateMissing {
		s.t.Errorf("new head with a pass only on the old one: %s, want missing", got)
	}
}

// c14: on a base branch that requires one approval, the reviewer's APPROVE
// review lets the loop merge (FR-4 AC7, IQ4).
func c14(s *suite) {
	e := s.env()
	s.must(e.Driver.RequireApprovals(1))
	it := s.item()
	ch, head := s.loopChange(it)
	s.passGates(head)
	out, err := e.Host.Merge(s.ctx(), ch, head, ports.MergeMethodSquash, s.key("merge"))
	s.must(err)
	if out != ports.MergeNotMergeable {
		s.t.Fatalf("Merge with no approval = %s, want not_mergeable", out)
	}
	k := s.key("approve")
	_, err = e.Host.PostReview(s.ctx(), ch, ports.ReviewPost{Commit: head, Approve: true, Record: rec(ports.RecordReview, k)}, k)
	s.must(err)
	out, err = e.Host.Merge(s.ctx(), ch, head, ports.MergeMethodSquash, s.key("merge"))
	s.must(err)
	if out != ports.MergeMerged {
		s.t.Fatalf("Merge after the reviewer's approval = %s, want merged", out)
	}
}

// c16: a human's reply in a reviewer finding thread blocks, also after the
// reviewer resolves the thread. An empty CHANGES_REQUESTED review blocks,
// has kind EmptyChangesRequested, and only a human's dismissal clears it.
func c16(s *suite) {
	e := s.env()
	ctx := s.ctx()
	it := s.item()
	ch, head := s.loopChange(it)
	k := s.key("review")
	_, err := e.Host.PostReview(ctx, ch, ports.ReviewPost{Commit: head, Record: rec(ports.RecordReview, k), Findings: []ports.FindingPost{
		{ID: "F1", Score: 4, Status: "open", Path: ChangedFile, Line: 1, Record: rec(ports.RecordFinding, s.key("f1"))},
	}}, k)
	s.must(err)
	rrs, err := e.Host.ReviewRecords(ctx, ch)
	s.must(err)
	th := rrs[0].Threads["F1"]
	if len(s.manual(ch)) != 0 {
		s.t.Fatalf("the reviewer's finding is a manual comment: %+v", s.manual(ch))
	}
	s.must(e.Driver.ReplyInThread(e.Human, ch, th, "I disagree"))
	s.cleared("human reply in a finding thread", s.one(ch, ports.ManualCommentThread, e.Human), false, "")
	s.must(e.Host.ResolveFinding(ctx, ch, "F1", s.key("resolve")))
	s.cleared("after the reviewer resolved the thread", s.one(ch, ports.ManualCommentThread, e.Human), false, "")

	_, err = e.Driver.SubmitReview(e.Human, ch, ReviewComment, "", nil)
	s.must(err)
	_, err = e.Driver.SubmitReview(e.Human, ch, ReviewApprove, "", nil)
	s.must(err)
	if got := s.find(s.manual(ch), ports.ManualCommentReviewBody, e.Human); len(got) != 0 {
		s.t.Errorf("empty comment and approval reviews are manual comments: %+v", got)
	}
	r1, err := e.Driver.SubmitReview(e.Human, ch, ReviewChangesRequested, "", nil)
	s.must(err)
	empty := s.find(s.manual(ch), ports.ManualCommentEmptyChangesRequested, e.Human)
	if len(empty) != 1 {
		s.t.Fatalf("got %d empty changes-requested manual comments, want 1", len(empty))
	}
	s.cleared("empty changes requested", empty[0], false, "")
	s.must(e.Driver.DismissReview(e.Loop, ch, r1))
	s.cleared("dismissed by the loop identity", s.one(ch, ports.ManualCommentEmptyChangesRequested, e.Human), false, "")
	r2, err := e.Driver.SubmitReview(e.Owner, ch, ReviewChangesRequested, "", nil)
	s.must(err)
	s.must(e.Driver.DismissReview(e.Human, ch, r2))
	s.cleared("dismissed by a human", s.one(ch, ports.ManualCommentEmptyChangesRequested, e.Owner), true, ports.ClearedByHumanDismissed)
}
