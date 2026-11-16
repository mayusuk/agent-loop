package github_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

func statusRec(key ports.IdemKey) ports.Record {
	data, _ := json.Marshal(map[string]any{"type": "status", "v": 1, "event": "escalate", "key": key})
	return ports.Record{Type: ports.RecordStatus, Key: key, Data: data}
}

func TestNewTrackerChecksConfig(t *testing.T) {
	p := newStub(t, 100)
	ids := github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)}
	if _, err := github.NewTracker(p.client, github.TrackerConfig{Identities: github.Identities{Loop: ids.Loop}}); err == nil {
		t.Error("tracker without the coder and reviewer IDs accepted")
	}
	dup := map[ports.Marker]string{ports.MarkerPaused: "Ready"}
	if _, err := github.NewTracker(p.client, github.TrackerConfig{Identities: ids, Labels: dup}); err == nil {
		t.Error("two markers on one label accepted")
	}
	caps := p.tracker.Capabilities()
	if caps.Assign || !caps.NativeDependencies || caps.StatusMarkers || caps.MaxBody != github.MaxBody {
		t.Errorf("capabilities %+v", caps)
	}
}

// TestItemView: markers from labels (others ignored), close reasons, closer,
// assignees by numeric ID, version (§9.2, work plan D2, D6, D8).
func TestItemView(t *testing.T) {
	p := newStub(t, 100)
	owner, human := actorOf(p.owner), actorOf(p.human)
	it := p.item(t, owner, ports.MarkerReady, ports.MarkerTypeBug)
	must(t, p.driver.SetMarker(owner, it, ports.MarkerNeedsOwner, true))
	p.srv.CreateLabel("wontfix")
	must(t, p.driver.CommentItem(owner, it, "x", nil))
	must(t, p.driver.Assign(owner, it, &human))
	got, err := p.tracker.Item(ctx, it)
	must(t, err)
	want := []ports.Marker{ports.MarkerNeedsOwner, ports.MarkerReady, ports.MarkerTypeBug}
	if len(got.Markers) != 3 || got.Markers[0] != want[0] || got.Markers[1] != want[1] || got.Markers[2] != want[2] {
		t.Errorf("markers %v, want %v", got.Markers, want)
	}
	if got.CloseReason != ports.CloseOpen || got.ClosedBy != nil || len(got.Assignees) != 1 || got.Assignees[0] != human {
		t.Errorf("item %+v", got)
	}
	if !regexp.MustCompile(`^2026-11-16T09:00:\d\dZ\|bug,needs-owner,ready$`).MatchString(string(got.Version)) {
		t.Errorf("version %q", got.Version)
	}
	if !strings.HasSuffix(got.URL, "/acme/widgets/issues/"+string(it)) || got.CreatedAt.IsZero() {
		t.Errorf("url %q, created %v", got.URL, got.CreatedAt)
	}

	for _, c := range []struct {
		reason string
		want   ports.CloseReason
	}{{"completed", ports.CloseCompleted}, {"not_planned", ports.CloseNotDone}, {"duplicate", ports.CloseNotDone}} {
		ref := p.item(t, owner)
		var out struct{}
		must(t, p.client.Send(ctx, ports.IdentityLoop, "PATCH", p.client.RepoPath("/issues/%s", ref), map[string]any{"state": "closed", "state_reason": c.reason}, &out))
		got, err := p.tracker.Item(ctx, ref)
		must(t, err)
		if got.CloseReason != c.want || got.ClosedBy == nil || got.ClosedBy.ID != actorOf(p.loop).ID || got.ClosedBy.Kind != ports.ActorBot {
			t.Errorf("%s: %s by %+v", c.reason, got.CloseReason, got.ClosedBy)
		}
	}
}

func TestItemNotFound(t *testing.T) {
	p := newStub(t, 100)
	pr := p.srv.AddPull(p.coder, "agent/1", "")
	for _, ref := range []ports.ItemRef{"999", "abc", "0", ports.ItemRef(itoa(int64(pr)))} {
		if _, err := p.tracker.Item(ctx, ref); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("Item(%q): %v, want ErrNotFound", ref, err)
		}
	}
}

func TestCandidates(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	a := p.item(t, owner, ports.MarkerReady)
	p.item(t, owner)
	closed := p.item(t, owner, ports.MarkerReady)
	must(t, p.driver.CloseItem(owner, closed, ports.CloseCompleted))
	b := p.item(t, owner, ports.MarkerReady, ports.MarkerTypeFeature)
	p.srv.AddPull(p.coder, "agent/x", "")
	cs, err := p.tracker.Candidates(ctx)
	must(t, err)
	if len(cs) != 2 || cs[0].Ref != a || cs[1].Ref != b {
		t.Errorf("candidates %+v, want %s then %s", cs, a, b)
	}
}

func TestSetMarker(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	must(t, p.tracker.SetMarker(ctx, it, ports.MarkerBlocked, true, "k"))
	writes := func() int {
		n := 0
		for _, r := range p.srv.Requests() {
			if r.Method != "GET" {
				n++
			}
		}
		return n
	}
	w := writes()
	must(t, p.tracker.SetMarker(ctx, it, ports.MarkerBlocked, true, "k"))
	must(t, p.tracker.SetMarker(ctx, it, ports.MarkerPaused, false, "k2"))
	if writes() != w {
		t.Error("a marker already in its state was written again")
	}
	must(t, p.tracker.SetMarker(ctx, it, ports.MarkerBlocked, false, "k3"))
	got, _ := p.tracker.Item(ctx, it)
	if len(got.Markers) != 0 {
		t.Errorf("markers %v after clearing", got.Markers)
	}
	// A label removed between the read and the write is still success.
	must(t, p.tracker.SetMarker(ctx, it, ports.MarkerBlocked, true, "k4"))
	p.srv.Fail("DELETE", `/labels/blocked$`, 404, nil, `{"message":"Label does not exist"}`, 1)
	must(t, p.tracker.SetMarker(ctx, it, ports.MarkerBlocked, false, "k5"))
	if err := p.tracker.SetMarker(ctx, it, "nonsense", true, "k6"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("unknown marker: %v", err)
	}
}

// TestCommentIdempotentByAuthor: a repeat finds the loop's comment by key;
// someone else's comment with the key doesn't count (§6.1, I12).
func TestCommentIdempotentByAuthor(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	forged := statusRec("k")
	must(t, p.driver.CommentItem(actorOf(p.human), it, "forged", &forged))
	c1, err := p.tracker.Comment(ctx, it, statusRec("k"), untrusted.New("Escalated: <b>see</b>"), "k")
	must(t, err)
	c2, err := p.tracker.Comment(ctx, it, statusRec("k"), untrusted.New("again"), "k")
	must(t, err)
	if c1 != c2 {
		t.Errorf("refs %s and %s", c1, c2)
	}
	n, _ := itoaRef(it)
	bodies := p.srv.Comments(n)
	if len(bodies) != 2 {
		t.Fatalf("%d comments, want the forgery and one of the loop's", len(bodies))
	}
	if !strings.HasPrefix(bodies[1], "Escalated: <b>see</b>\n\n<!-- agent-loop:v1 {") || !strings.HasSuffix(bodies[1], " -->") {
		t.Errorf("body %q", bodies[1])
	}
	if !strings.Contains(bodies[1], `"key":"k"`) || !strings.Contains(bodies[1], `"type":"status"`) || !strings.Contains(bodies[1], `"event":"escalate"`) {
		t.Errorf("marker %q", bodies[1])
	}
	if _, err := p.tracker.Comment(ctx, it, ports.Record{Type: ports.RecordStatus, Data: json.RawMessage(`[1]`)}, untrusted.Text{}, "x"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("bad record data: %v", err)
	}
	if _, err := p.tracker.Comment(ctx, "999", statusRec("y"), untrusted.Text{}, "y"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
}

func itoaRef(ref ports.ItemRef) (int, error) { return strconv.Atoi(string(ref)) }

// TestMarkerCantCloseComment: record data can't end the HTML comment early.
func TestMarkerCantCloseComment(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	data := json.RawMessage(`{"type":"status","event":"escalate","reason":"x --> <script>"}`)
	_, err := p.tracker.Comment(ctx, it, ports.Record{Type: ports.RecordStatus, Data: data}, untrusted.Text{}, "k")
	must(t, err)
	n, _ := itoaRef(it)
	body := p.srv.Comments(n)[0]
	if strings.Count(body, "-->") != 1 || strings.Contains(body, "<script>") {
		t.Errorf("body %q", body)
	}
	recs, err := p.tracker.FindRecords(ctx, it, ports.RecordStatus)
	must(t, err)
	if len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	var d struct{ Reason string }
	must(t, json.Unmarshal(recs[0].Data, &d))
	if d.Reason != "x --> <script>" {
		t.Errorf("reason %q", d.Reason)
	}
}

// TestFindRecords: body and comment records, author-checked; only a marker
// on the last line counts (§6.1, C3, work plan D9).
func TestFindRecords(t *testing.T) {
	p := newStub(t, 100)
	owner, coder := actorOf(p.owner), actorOf(p.coder)
	k := ports.IdemKey("followup:57")
	data, _ := json.Marshal(map[string]any{"type": "followup", "change": "57", "key": k})
	ref, err := p.tracker.CreateItem(ctx, ports.NewItem{Record: ports.Record{Type: ports.RecordFollowUp, Data: data}}, k)
	must(t, err)
	// A human's item with a followup marker doesn't count.
	forged, err := p.driver.NewItem(owner, "t", "body\n\n"+githubtest.Marker(ports.Record{Type: ports.RecordFollowUp, Data: data}))
	must(t, err)

	notLast := statusRec("a")
	must(t, p.driver.CommentItem(actorOf(p.loop), ref, githubtest.Marker(notLast)+"\n\ntrailing text", nil))
	must(t, p.driver.CommentItem(actorOf(p.loop), ref, "<!-- agent-loop:v1 {not json} -->", nil))
	coderStatus := statusRec("b")
	must(t, p.driver.CommentItem(coder, ref, "x", &coderStatus))
	// A reply record from the coder counts as a reply.
	reply, _ := json.Marshal(map[string]any{"type": "reply", "finding": "F1", "kind": "fixed", "key": "r"})
	must(t, p.driver.CommentItem(coder, ref, "x", &ports.Record{Type: ports.RecordReply, Data: reply}))
	good := statusRec("c")
	must(t, p.driver.CommentItem(actorOf(p.loop), ref, "x", &good))

	recs, err := p.tracker.FindRecords(ctx, ref, ports.RecordFollowUp)
	must(t, err)
	if len(recs) != 1 || recs[0].Key != k || recs[0].Ref != ports.CommentRef(ref) || recs[0].Author.ID != actorOf(p.loop).ID || recs[0].At.IsZero() {
		t.Errorf("followup records %+v", recs)
	}
	if recs, _ := p.tracker.FindRecords(ctx, forged, ports.RecordFollowUp); len(recs) != 0 {
		t.Errorf("a human's body record counted: %+v", recs)
	}
	recs, err = p.tracker.FindRecords(ctx, ref, ports.RecordStatus)
	must(t, err)
	if len(recs) != 1 || recs[0].Key != "c" || recs[0].Ref == "" {
		t.Errorf("status records %+v, want only c", recs)
	}
	if recs, _ := p.tracker.FindRecords(ctx, ref, ports.RecordReply); len(recs) != 1 || recs[0].Author.ID != coder.ID {
		t.Errorf("reply records %+v", recs)
	}
	if _, err := p.tracker.FindRecords(ctx, "999", ports.RecordStatus); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
}

// TestCreateItem: lookup by creator, time and key; labels; default title
// (§6.1, work plan D10).
func TestCreateItem(t *testing.T) {
	p := newStub(t, 100)
	since := p.srv.Now()
	k := ports.IdemKey("plan/x:task:t1")
	data, _ := json.Marshal(map[string]any{"type": "task", "plan": "x", "task": "t1", "key": k})
	n := ports.NewItem{
		Markers: []ports.Marker{ports.MarkerReady, ports.MarkerTypeFeature},
		Title:   untrusted.New("Add login"), Body: untrusted.New("Do it."),
		Record: ports.Record{Type: ports.RecordTask, Data: data}, Since: since,
	}
	// The owner's issue carrying the same record is not the loop's.
	_, err := p.driver.NewItem(actorOf(p.owner), "copy", githubtest.Marker(ports.Record{Data: data}))
	must(t, err)
	a, err := p.tracker.CreateItem(ctx, n, k)
	must(t, err)
	b, err := p.tracker.CreateItem(ctx, n, k)
	must(t, err)
	if a != b {
		t.Errorf("repeat created %s and %s", a, b)
	}
	it, _ := p.tracker.Item(ctx, a)
	if len(it.Markers) != 2 {
		t.Errorf("markers %v", it.Markers)
	}
	num, _ := itoaRef(a)
	if body := p.srv.IssueBody(num); !strings.HasPrefix(body, "Do it.\n\n<!-- agent-loop:v1 ") {
		t.Errorf("body %q", body)
	}
	// An item from before Since isn't found: a new one is created.
	p.srv.Advance(3600e9)
	n.Since = p.srv.Now()
	c, err := p.tracker.CreateItem(ctx, n, k)
	must(t, err)
	if c == a {
		t.Error("an item created before Since was reused")
	}
	// No title: a default one, since GitHub needs one.
	data2, _ := json.Marshal(map[string]any{"type": "followup", "change": "57", "key": "followup:57"})
	d, err := p.tracker.CreateItem(ctx, ports.NewItem{Record: ports.Record{Type: ports.RecordFollowUp, Data: data2}}, "followup:57")
	must(t, err)
	var got struct{ Title string }
	must(t, p.client.Get(ctx, ports.IdentityLoop, p.client.RepoPath("/issues/%s", d), nil, &got))
	if got.Title != "agent-loop followup" {
		t.Errorf("default title %q", got.Title)
	}
	if _, err := p.tracker.CreateItem(ctx, ports.NewItem{Markers: []ports.Marker{"nonsense"}, Record: ports.Record{Type: ports.RecordTask}}, "z"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("unknown marker: %v", err)
	}
}

func TestClose(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	if err := p.tracker.Close(ctx, it, ports.CloseOpen, "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("close with reason open: %v", err)
	}
	must(t, p.tracker.Close(ctx, it, ports.CloseNotDone, "k"))
	must(t, p.tracker.Close(ctx, it, ports.CloseCompleted, "k")) // already closed: left alone
	got, _ := p.tracker.Item(ctx, it)
	if got.CloseReason != ports.CloseNotDone || got.ClosedBy.ID != actorOf(p.loop).ID {
		t.Errorf("closed %s by %+v", got.CloseReason, got.ClosedBy)
	}
	if err := p.tracker.Close(ctx, "999", ports.CloseCompleted, "k"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
}

func claimRecords(t *testing.T, p *stubProject, it ports.ItemRef) []map[string]any {
	t.Helper()
	recs, err := p.tracker.FindRecords(ctx, it, ports.RecordStatus)
	must(t, err)
	var out []map[string]any
	for _, r := range recs {
		var m map[string]any
		must(t, json.Unmarshal(r.Data, &m))
		if m["event"] == "claim" {
			out = append(out, m)
		}
	}
	return out
}

// TestClaimPostsRecord: the claim record comes after the labels, carries
// the claim number, and is the key's ledger (§9.3, work plan D1).
func TestClaimPostsRecord(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner), ports.MarkerReady)
	v := mustItem(t, p, it).Version
	v1, err := p.tracker.Claim(ctx, it, v, "item/p/1#1:claim")
	must(t, err)
	got := mustItem(t, p, it)
	if got.Version != v1 || len(got.Markers) != 1 || got.Markers[0] != ports.MarkerInProgress {
		t.Errorf("after claim: %+v (returned %s)", got, v1)
	}
	recs := claimRecords(t, p, it)
	if len(recs) != 1 || recs[0]["claim"] != 1.0 || recs[0]["item"] != string(it) || recs[0]["key"] != "item/p/1#1:claim" || recs[0]["v"] != 1.0 {
		t.Errorf("claim records %+v", recs)
	}
	n, _ := itoaRef(it)
	if c := p.srv.Comments(n); len(c) != 1 || !strings.HasPrefix(c[0], "agent-loop claimed this item (claim 1).") {
		t.Errorf("comments %q", c)
	}

	// Released and picked again: claim 2.
	owner := actorOf(p.owner)
	must(t, p.driver.SetMarker(owner, it, ports.MarkerInProgress, false))
	must(t, p.driver.SetMarker(owner, it, ports.MarkerReady, true))
	_, err = p.tracker.Claim(ctx, it, mustItem(t, p, it).Version, "item/p/1#2:claim")
	must(t, err)
	if recs := claimRecords(t, p, it); len(recs) != 2 || recs[1]["claim"] != 2.0 {
		t.Errorf("second claim records %+v", recs)
	}
}

func mustItem(t *testing.T, p *stubProject, ref ports.ItemRef) ports.Item {
	t.Helper()
	it, err := p.tracker.Item(ctx, ref)
	must(t, err)
	return it
}

func TestClaimRefusals(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	it := p.item(t, owner, ports.MarkerReady)
	if _, err := p.tracker.Claim(ctx, it, "old", "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("stale version: %v", err)
	}
	must(t, p.driver.CloseItem(owner, it, ports.CloseCompleted))
	if _, err := p.tracker.Claim(ctx, it, mustItem(t, p, it).Version, "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("closed item: %v", err)
	}
	if _, err := p.tracker.Claim(ctx, "999", "v", "k"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
	n, _ := itoaRef(it)
	if c := p.srv.Comments(n); len(c) != 0 {
		t.Errorf("a refused claim commented: %q", c)
	}
}

// TestClaimRollsBackAfterHumanChange is C2 at step 3 of §9.3: a human
// changes the item between the label writes and the re-read; the claim
// rolls back its labels, returns ErrConflict and leaves no claim record.
func TestClaimRollsBackAfterHumanChange(t *testing.T) {
	for _, change := range []struct {
		name string
		do   func(p *stubProject, it ports.ItemRef) error
	}{
		{"label", func(p *stubProject, it ports.ItemRef) error {
			return p.driver.SetMarker(actorOf(p.human), it, ports.MarkerTypeBug, true)
		}},
		{"assign", func(p *stubProject, it ports.ItemRef) error {
			h := actorOf(p.human)
			return p.driver.Assign(actorOf(p.owner), it, &h)
		}},
		{"close", func(p *stubProject, it ports.ItemRef) error {
			return p.driver.CloseItem(actorOf(p.owner), it, ports.CloseNotDone)
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			p := newStub(t, 100)
			it := p.item(t, actorOf(p.owner), ports.MarkerReady, ports.MarkerTypeFeature)
			v := mustItem(t, p, it).Version
			p.srv.After("DELETE", `/labels/ready$`, func() { must(t, change.do(p, it)) })
			_, err := p.tracker.Claim(ctx, it, v, "k")
			if !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("got %v, want ErrConflict", err)
			}
			got := mustItem(t, p, it)
			has := map[ports.Marker]bool{}
			for _, m := range got.Markers {
				has[m] = true
			}
			if has[ports.MarkerInProgress] || !has[ports.MarkerReady] {
				t.Errorf("not rolled back: %v", got.Markers)
			}
			if recs := claimRecords(t, p, it); len(recs) != 0 {
				t.Errorf("claim records after a rollback: %+v", recs)
			}
		})
	}
}

// TestClaimRollbackFailure: a failed rollback still reports ErrConflict and
// names the failure (work plan D3).
func TestClaimRollbackFailure(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner), ports.MarkerReady)
	v := mustItem(t, p, it).Version
	p.srv.After("DELETE", `/labels/ready$`, func() {
		must(t, p.driver.SetMarker(actorOf(p.human), it, ports.MarkerPaused, true))
		p.srv.Fail("DELETE", `/labels/in-progress$`, 502, nil, "", 1)
	})
	_, err := p.tracker.Claim(ctx, it, v, "k")
	if !errors.Is(err, ports.ErrConflict) || !strings.Contains(err.Error(), "rollback failed") {
		t.Errorf("got %v", err)
	}
}

// TestClaimWriteFailures: a failed label write returns its class; when the
// ready label can't be removed, in-progress is rolled back.
func TestClaimWriteFailures(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner), ports.MarkerReady)
	p.srv.Fail("POST", `/labels$`, 403, nil, `{"message":"no"}`, 1)
	if _, err := p.tracker.Claim(ctx, it, mustItem(t, p, it).Version, "k"); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("add refused: %v", err)
	}
	p.srv.Fail("DELETE", `/labels/ready$`, 500, nil, "", 1)
	if _, err := p.tracker.Claim(ctx, it, mustItem(t, p, it).Version, "k"); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("remove failed: %v", err)
	}
	if got := mustItem(t, p, it); len(got.Markers) != 1 || got.Markers[0] != ports.MarkerReady {
		t.Errorf("markers %v after a failed claim", got.Markers)
	}
	// The comment fails after the labels: a retry with the key posts it.
	p.srv.Fail("POST", `/comments$`, 500, nil, "", 1)
	v := mustItem(t, p, it).Version
	if _, err := p.tracker.Claim(ctx, it, v, "k"); !errors.Is(err, ports.ErrTransient) {
		t.Fatalf("comment failed: %v", err)
	}
	if _, err := p.tracker.Claim(ctx, it, v, "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("retry after the labels changed: %v, want ErrConflict (§9.3 step 4 leaves it to reconcile)", err)
	}
	_, err := p.tracker.Comment(ctx, it, ports.Record{Type: ports.RecordStatus, Data: json.RawMessage(`{"event":"claim","claim":1}`)}, untrusted.Text{}, "k")
	must(t, err)
	if _, err := p.tracker.Claim(ctx, it, "anything", "k"); err != nil {
		t.Errorf("claim with the posted record's key: %v", err)
	}
}

// TestEveryMethodClassifiesErrors: each method returns the class of a
// failed call (work plan D11).
func TestEveryMethodClassifiesErrors(t *testing.T) {
	type call func(p *stubProject, it ports.ItemRef) error
	methods := map[string]call{
		"Item":       func(p *stubProject, it ports.ItemRef) error { _, err := p.tracker.Item(ctx, it); return err },
		"Candidates": func(p *stubProject, it ports.ItemRef) error { _, err := p.tracker.Candidates(ctx); return err },
		"Changes":    func(p *stubProject, it ports.ItemRef) error { _, _, err := p.tracker.Changes(ctx, ""); return err },
		"Claim": func(p *stubProject, it ports.ItemRef) error {
			_, err := p.tracker.Claim(ctx, it, "v", "k")
			return err
		},
		"SetMarker": func(p *stubProject, it ports.ItemRef) error {
			return p.tracker.SetMarker(ctx, it, ports.MarkerBlocked, true, "k")
		},
		"Comment": func(p *stubProject, it ports.ItemRef) error {
			_, err := p.tracker.Comment(ctx, it, statusRec("k"), untrusted.Text{}, "k")
			return err
		},
		"FindRecords": func(p *stubProject, it ports.ItemRef) error {
			_, err := p.tracker.FindRecords(ctx, it, ports.RecordStatus)
			return err
		},
		"CreateItem": func(p *stubProject, it ports.ItemRef) error {
			_, err := p.tracker.CreateItem(ctx, ports.NewItem{Record: statusRec("k")}, "k")
			return err
		},
		"AddDependency": func(p *stubProject, it ports.ItemRef) error {
			return p.tracker.AddDependency(ctx, it, "1", "k")
		},
		"Dependencies": func(p *stubProject, it ports.ItemRef) error {
			_, err := p.tracker.Dependencies(ctx, it)
			return err
		},
		"Close": func(p *stubProject, it ports.ItemRef) error {
			return p.tracker.Close(ctx, it, ports.CloseCompleted, "k")
		},
	}
	classes := []struct {
		status int
		header http.Header
		want   error
	}{
		{404, nil, ports.ErrNotFound},
		{403, nil, ports.ErrPermission},
		{429, http.Header{"Retry-After": {"1"}}, ports.ErrRateLimited},
		{422, nil, ports.ErrConflict},
		{503, nil, ports.ErrTransient},
	}
	for name, m := range methods {
		for _, c := range classes {
			t.Run(name+"/"+itoa(int64(c.status)), func(t *testing.T) {
				p := newStub(t, 100)
				p.item(t, actorOf(p.owner))
				it := p.item(t, actorOf(p.owner))
				p.srv.Fail("GET", `.`, c.status, c.header, `{"message":"x"}`, 0)
				if err := m(p, it); !errors.Is(err, c.want) {
					t.Errorf("got %v, want %v", err, c.want)
				}
			})
		}
	}
}
