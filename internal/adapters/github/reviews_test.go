package github_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// TestPostReviewPlacement: inline, file-level and review-text findings,
// finding IDs in their records, the status table, and the idempotent
// repeat (§6.3, I8, I16, work plan D3–D5).
func TestPostReviewPlacement(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	ch, head := p.open(t, it, "agent/"+string(it))
	n, _ := strconv.Atoi(string(ch))
	post := ports.ReviewPost{Commit: head, Summary: untrusted.New("Looks close."),
		Record: reviewRec("r1", []map[string]any{{"id": "F1", "score": 4, "status": "open"}, {"id": "F2", "score": 3, "status": "open"}}),
		Findings: []ports.FindingPost{
			{ID: "F1", Score: 4, Status: "open", Path: "conformance.txt", Line: 1, Body: untrusted.New("inline"), Record: findingRec("f1")},
			{ID: "F2", Score: 3, Status: "open", Path: "conformance.txt", Line: 40, Body: untrusted.New("outside"), Record: findingRec("f2")},
			{ID: "F3", Score: 2, Status: "open", Path: "conformance.txt", Body: untrusted.New("file"), Record: findingRec("f3")},
			{ID: "F4", Score: 2, Status: "open", Path: "README.md", Line: 1, Body: untrusted.New("not in diff"), Record: findingRec("f4")},
			{ID: "F5", Score: 1, Status: "open", Body: untrusted.New("no file"), Record: findingRec("f5")},
		}}
	r1, err := p.host.PostReview(ctx, ch, post, "r1")
	must(t, err)
	r2, err := p.host.PostReview(ctx, ch, post, "r1")
	must(t, err)
	if r1 != r2 {
		t.Fatalf("repeat posted again: %s, %s", r1, r2)
	}
	rrs, err := p.host.ReviewRecords(ctx, ch)
	must(t, err)
	if len(rrs) != 1 || len(rrs[0].Threads) != 3 || rrs[0].Threads["F4"] != "" || rrs[0].Threads["F5"] != "" || rrs[0].Author.ID != itoa(p.reviewer.ID) {
		t.Fatalf("review records %+v", rrs)
	}
	var body string
	reviews := p.srv.Reviews(n)
	if len(reviews) != 1 {
		t.Fatalf("%d reviews", len(reviews))
	}
	body = reviews[0]
	for _, want := range []string{"Looks close.", "**F4** (score 2, open) · README.md:1", "**F5** (score 1, open) · no file",
		"| Finding | Score | Status | Where |", "| F1 | 4 | open | conformance.txt:1 |", "| F2 | 3 | open | conformance.txt (file) |",
		"| F3 | 2 | open | conformance.txt (file) |", "| F5 | 1 | open | this review (no file) |"} {
		if !strings.Contains(body, want) {
			t.Errorf("review body lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "| F1 |") > strings.Index(body, "| F3 |") {
		t.Error("table rows out of record order")
	}
	comments := p.srv.ReviewComments(n)
	if len(comments) != 3 || !strings.HasPrefix(comments[1], "`conformance.txt:40` **F2**") || !strings.Contains(comments[0], `"id":"F1"`) {
		t.Errorf("comments %q", comments)
	}

	// A later review links earlier findings' threads.
	_, err = p.host.PostReview(ctx, ch, ports.ReviewPost{Commit: head, Record: reviewRec("r2", []map[string]any{{"id": "F1", "score": 4, "status": "addressed"}, {"id": "F5", "score": 1, "status": "open"}})}, "r2")
	must(t, err)
	body = p.srv.Reviews(n)[1]
	if !strings.Contains(body, "| F1 | 4 | addressed | [conformance.txt:1](") || !strings.Contains(body, "| F5 | 1 | open | earlier review (no file) |") {
		t.Errorf("second review body:\n%s", body)
	}

	bad := ports.ReviewPost{Commit: head, Record: reviewRec("r3", nil), Findings: []ports.FindingPost{
		{ID: "F9", Path: "conformance.txt", Line: 1, Record: findingRecID("f9", "F8")}}}
	if _, err := p.host.PostReview(ctx, ch, bad, "r3"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("finding record with another id: %v", err)
	}
	if _, err := p.host.PostReview(ctx, ch, ports.ReviewPost{Commit: head, Approve: true, Record: reviewRec("r4", nil)}, "r4"); err != nil {
		t.Errorf("approve: %v", err)
	}
}

// TestResolveFindingAndReplies: resolve as the loop identity once, never over a
// human's resolve; no-thread and unknown findings; replies in threads and
// on the change (work plan D6, D7).
func TestResolveFindingAndReplies(t *testing.T) {
	p := newStub(t, 100)
	human := actorOf(p.human)
	it := p.item(t, actorOf(p.owner))
	ch, head := p.open(t, it, "agent/"+string(it))
	_, err := p.host.PostReview(ctx, ch, ports.ReviewPost{Commit: head,
		Record: reviewRec("r1", []map[string]any{{"id": "F1", "score": 4, "status": "open"}, {"id": "F2", "score": 2, "status": "open"}}),
		Findings: []ports.FindingPost{
			{ID: "F1", Score: 4, Status: "open", Path: "conformance.txt", Line: 1, Record: findingRec("f1")},
			{ID: "F2", Score: 2, Status: "open", Record: findingRec("f2")},
		}}, "r1")
	must(t, err)
	if err := p.host.ResolveFinding(ctx, ch, "F7", "k"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("unknown finding: %v", err)
	}
	must(t, p.host.ResolveFinding(ctx, ch, "F2", "k")) // no thread: nothing to do
	rrs, err := p.host.ReviewRecords(ctx, ch)
	must(t, err)
	th := rrs[0].Threads["F1"]
	must(t, p.driver.ResolveThread(human, ch, th))
	must(t, p.host.ResolveFinding(ctx, ch, "F1", "k"))
	if by := p.srv.ResolvedBy(string(th)); by != p.human.Login {
		t.Errorf("a human's resolve was replaced by %q", by)
	}

	for _, to := range []ports.ThreadRef{th, ports.ThreadRef(ch)} {
		for i := 0; i < 2; i++ {
			must(t, p.host.PostReply(ctx, ch, to, replyRec("reply:"+ports.IdemKey(to)), untrusted.New("fixed"), "reply:"+ports.IdemKey(to)))
		}
		all, err := p.driver.Replies(ch, to)
		must(t, err)
		var recs []ports.Record
		for _, r := range all {
			if r.Type == ports.RecordReply {
				recs = append(recs, r)
			}
		}
		if len(recs) != 1 || recs[0].Author.ID != itoa(p.coder.ID) {
			t.Errorf("replies in %s: %+v", to, recs)
		}
	}
	other := p.item(t, actorOf(p.owner))
	ch2, _ := p.open(t, other, "agent/"+string(other))
	if err := p.host.PostReply(ctx, ch2, th, replyRec("x"), untrusted.Text{}, "x"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("thread on another change: %v", err)
	}
}

// TestFR7AC9_ManualCommentClearing: a human resolve clears, an identity's
// or a listed bot's doesn't, unresolve blocks again, dismissals by a human
// clear, an empty CHANGES_REQUESTED review is its own kind, and unlisted
// bots are ignored (§9.6, D43, work plan D8–D10).
func TestFR7AC9_ManualCommentClearing(t *testing.T) {
	p := newStub(t, 100)
	human, owner, bot := actorOf(p.human), actorOf(p.owner), actorOf(p.bot)
	other := p.srv.AddUser(4001, "dependabot[bot]", "Bot")
	p.driver.Tokens[itoa(other.ID)] = other.Token
	it := p.item(t, owner)
	ch, _ := p.open(t, it, "agent/"+string(it))
	th, err := p.driver.ReviewComment(human, ch, "conformance.txt", 1, "rename this")
	must(t, err)
	_, err = p.driver.ReviewComment(actorOf(other), ch, "conformance.txt", 0, "bump deps")
	must(t, err)
	state := func() (cleared bool, by ports.ClearedBy, n int) {
		mcs, err := p.host.ManualComments(ctx, ch)
		must(t, err)
		for _, mc := range mcs {
			if mc.Author.ID == itoa(p.human.ID) && mc.Kind == ports.ManualCommentThread {
				cleared, by = mc.Cleared, mc.ClearedBy
				if !strings.HasPrefix(mc.ID, "MC-") || mc.Thread != th || untrusted.Read(mc.Body) != "rename this" {
					t.Errorf("manual comment %+v", mc)
				}
			}
			n++
		}
		return
	}
	if c, _, n := state(); c || n != 1 {
		t.Fatalf("new comment: cleared %v, %d manual comments (an unlisted bot's counts?)", c, n)
	}
	// GitHub refuses the reviewer app, installed without contents: write,
	// any thread resolve, whatever its token (#100).
	if err := p.driver.ResolveThread(actorOf(p.reviewer), ch, th); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("resolve by the reviewer app: %v, want ErrPermission", err)
	}
	for _, a := range []ports.Actor{actorOf(p.coder), actorOf(p.loop), bot} {
		must(t, p.driver.ResolveThread(a, ch, th))
		if c, _, _ := state(); c {
			t.Errorf("resolve by %s cleared it", a.Login)
		}
		must(t, p.driver.UnresolveThread(a, ch, th))
	}
	must(t, p.driver.ResolveThread(human, ch, th))
	if c, by, _ := state(); !c || by != ports.ClearedByHumanResolved {
		t.Errorf("human resolve: %v %q", c, by)
	}
	must(t, p.driver.UnresolveThread(owner, ch, th))
	if c, _, _ := state(); c {
		t.Error("unresolve didn't block again")
	}

	r1, err := p.driver.SubmitReview(human, ch, "CHANGES_REQUESTED", "please split", nil)
	must(t, err)
	r2, err := p.driver.SubmitReview(owner, ch, "CHANGES_REQUESTED", "", nil)
	must(t, err)
	_, err = p.driver.SubmitReview(human, ch, "APPROVE", "", nil)
	must(t, err)
	must(t, p.driver.DismissReview(actorOf(p.loop), ch, r1))
	kinds := func() map[string]ports.ManualComment {
		mcs, err := p.host.ManualComments(ctx, ch)
		must(t, err)
		out := map[string]ports.ManualComment{}
		for _, mc := range mcs {
			if mc.Kind != ports.ManualCommentThread {
				out[mc.ID] = mc
			}
		}
		return out
	}
	got := kinds()
	b1, b2 := got["MC-R"+string(r1)], got["MC-R"+string(r2)]
	if len(got) != 2 || b1.Kind != ports.ManualCommentReviewBody || b1.Cleared || b2.Kind != ports.ManualCommentEmptyChangesRequested || b2.Cleared {
		t.Fatalf("review bodies %+v", got)
	}
	must(t, p.driver.DismissReview(human, ch, r2))
	if mc := kinds()["MC-R"+string(r2)]; !mc.Cleared || mc.ClearedBy != ports.ClearedByHumanDismissed || mc.Kind != ports.ManualCommentEmptyChangesRequested {
		t.Errorf("empty changes requested dismissed by a human: %+v", mc)
	}

	// GitHub can't dismiss a commented review, so a human's non-empty one
	// stays blocking (work plan D11).
	r3, err := p.driver.SubmitReview(human, ch, "COMMENT", "please explain", nil)
	must(t, err)
	if mc, ok := kinds()["MC-R"+string(r3)]; !ok || mc.Kind != ports.ManualCommentReviewBody || mc.Cleared || mc.Author.ID != human.ID {
		t.Fatalf("commented review body: %v %+v", ok, mc)
	}
	if err := p.driver.DismissReview(owner, ch, r3); err == nil {
		t.Error("dismissing a commented review succeeded")
	}
	if mc := kinds()["MC-R"+string(r3)]; mc.Cleared {
		t.Errorf("commented review cleared by a refused dismissal: %+v", mc)
	}
}

// TestManualCommentResolveTiming: GitHub has no resolve time, so a comment
// made between two observations and then resolved stays blocking until the
// thread is resolved again; a thread first seen resolved clears nothing;
// the feed cursor carries observations across a restart (work plan D9).
func TestManualCommentResolveTiming(t *testing.T) {
	p := newStub(t, 100)
	human := actorOf(p.human)
	it := p.item(t, actorOf(p.owner))
	ch, _ := p.open(t, it, "agent/"+string(it))
	th, err := p.driver.ReviewComment(human, ch, "conformance.txt", 1, "first")
	must(t, err)
	must(t, p.driver.ResolveThread(human, ch, th))
	cleared := func(h *github.Host) bool {
		mcs, err := h.ManualComments(ctx, ch)
		must(t, err)
		return len(mcs) == 1 && mcs[0].Cleared
	}
	if cleared(p.host) {
		t.Error("a thread first seen resolved cleared its comment")
	}
	_, cur, err := p.host.Changes(ctx, "") // the feed observes it too
	must(t, err)
	must(t, p.driver.UnresolveThread(human, ch, th))
	if cleared(p.host) {
		t.Error("unresolved thread cleared")
	}
	_, cur = drain(t, p.host, cur)
	must(t, p.driver.ResolveThread(human, ch, th))
	if !cleared(p.host) {
		t.Error("resolve after an observed unresolve didn't clear")
	}

	// A restarted worker: a new host, the feed cursor restores what was
	// seen before the resolve.
	fresh := func() *github.Host {
		h, err := github.NewHost(p.client, github.HostConfig{
			Identities:        github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)},
			ManualCommentBots: []string{p.bot.Login}, PageSize: 100})
		must(t, err)
		return h
	}
	h2 := fresh()
	drain(t, h2, cur)
	if !cleared(h2) {
		t.Error("restored observation didn't clear after the restart")
	}
	if cleared(fresh()) {
		t.Error("a host with no observations cleared a thread first seen resolved")
	}

	// A restarted worker whose merge check runs before the feed: the
	// first-sight observation must not hide the cursor's restored one.
	h3 := fresh()
	if cleared(h3) {
		t.Error("a host with no observations cleared a thread first seen resolved")
	}
	if cleared(h3) {
		t.Error("a second first-sight read cleared a thread first seen resolved")
	}
	drain(t, h3, cur)
	if !cleared(h3) {
		t.Error("ManualComments before Changes(cur) hid the restored observation")
	}
}

func drain(t *testing.T, h *github.Host, cur ports.Cursor) ([]ports.ChangeEvent, ports.Cursor) {
	t.Helper()
	var all []ports.ChangeEvent
	for {
		evs, next, err := h.Changes(ctx, cur)
		must(t, err)
		cur = next
		if len(evs) == 0 {
			return all, cur
		}
		all = append(all, evs...)
	}
}

// TestStubEmptyReviewBody: GitHub documents a body as required for COMMENT
// and REQUEST_CHANGES, so the stub refuses an empty one unless the review
// has inline comments, and the driver adds one (work plan D14).
func TestStubEmptyReviewBody(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	ch, _ := p.open(t, it, "agent/"+string(it))
	post := func(in map[string]any) int {
		b, err := json.Marshal(in)
		must(t, err)
		req, err := http.NewRequest("POST", p.srv.URL+"/repos/acme/widgets/pulls/"+string(ch)+"/reviews", bytes.NewReader(b))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+p.human.Token)
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		res.Body.Close()
		return res.StatusCode
	}
	for _, ev := range []string{"COMMENT", "REQUEST_CHANGES"} {
		if got := post(map[string]any{"event": ev, "body": ""}); got != http.StatusUnprocessableEntity {
			t.Errorf("empty %s review with no comments: status %d, want 422", ev, got)
		}
		inline := []any{map[string]any{"path": "conformance.txt", "line": 1, "body": "here"}}
		if got := post(map[string]any{"event": ev, "body": "", "comments": inline}); got != http.StatusOK {
			t.Errorf("empty %s review with an inline comment: status %d, want 200", ev, got)
		}
	}
	if got := post(map[string]any{"event": "APPROVE", "body": ""}); got != http.StatusOK {
		t.Errorf("empty approval: status %d, want 200", got)
	}
	r, err := p.driver.SubmitReview(actorOf(p.human), ch, "CHANGES_REQUESTED", "", nil)
	must(t, err)
	mcs, err := p.host.ManualComments(ctx, ch)
	must(t, err)
	found := false
	for _, mc := range mcs {
		found = found || (mc.ID == "MC-R"+string(r) && mc.Kind == ports.ManualCommentEmptyChangesRequested)
	}
	if !found {
		t.Errorf("driver's empty changes-requested review isn't an EmptyChangesRequested manual comment: %+v", mcs)
	}
}
