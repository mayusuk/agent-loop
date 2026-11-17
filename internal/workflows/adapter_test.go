package workflows

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// texts records the text the worker writes through the tracker.
type texts struct {
	ports.WorkTracker
	comments []string
	items    []ports.NewItem
}

func (x *texts) Comment(ctx context.Context, ref ports.ItemRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) (ports.CommentRef, error) {
	x.comments = append(x.comments, untrusted.Read(body))
	return x.WorkTracker.Comment(ctx, ref, rec, body, key)
}

func (x *texts) CreateItem(ctx context.Context, n ports.NewItem, key ports.IdemKey) (ports.ItemRef, error) {
	x.items = append(x.items, n)
	return x.WorkTracker.CreateItem(ctx, n, key)
}

func statusRecords(t *testing.T, h *harness, item ports.ItemRef) []contract.Record {
	t.Helper()
	recs, err := h.f.Tracker().FindRecords(context.Background(), item, ports.RecordStatus)
	if err != nil {
		t.Fatal(err)
	}
	var out []contract.Record
	for _, r := range recs {
		var c contract.Record
		json.Unmarshal(r.Data, &c)
		out = append(out, c)
	}
	return out
}

// TestFR10AC1_EscalationMentionsTheOwnerOnce: an escalation comment
// mentions the owners, names the reason and IDs, carries the status record
// with the claim number, and a retry posts nothing more.
func TestFR10AC1_EscalationMentionsTheOwnerOnce(t *testing.T) {
	h := newHarness(t)
	x := &texts{WorkTracker: h.a.Tracker}
	h.a.Tracker = x
	item := h.item()
	req := StatusRequest{Item: string(item), Claim: 2, Action: core.PostStatus{Key: "item/p/1#2:escalate:9", Event: contract.EventEscalate,
		Reason: core.ReasonRoundLimit, Stage: contract.StageReview, FindingIDs: []string{"F3"}, CommentIDs: []string{"MC-9"}}}
	for i := 0; i < 2; i++ {
		if _, err := h.a.PostStatus(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	recs := statusRecords(t, h, item)
	if len(recs) != 1 || recs[0].Event != contract.EventEscalate || recs[0].Reason != core.ReasonRoundLimit || recs[0].Claim != 2 ||
		recs[0].Key != req.Action.Key || strings.Join(recs[0].FindingIDs, ",") != "F3" {
		t.Fatalf("records = %+v", recs)
	}
	body := x.comments[0]
	for _, want := range []string{"@owner", "round allowance", "F3", "MC-9", "needs-owner"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q: %s", want, body)
		}
	}
	// Other events don't mention the owners.
	h.a.PostStatus(context.Background(), StatusRequest{Item: string(item), Claim: 2, Action: core.PostStatus{Key: "k2", Event: contract.EventResume}})
	if strings.Contains(x.comments[len(x.comments)-1], "@owner") {
		t.Errorf("a resume mentions the owner: %s", x.comments[len(x.comments)-1])
	}
}

func TestSetMarkerIsIdempotent(t *testing.T) {
	h := newHarness(t)
	item := h.item()
	req := MarkerRequest{Item: string(item), Action: core.SetMarker{Key: "item/p/1#1:marker:needs_owner:true:3", Marker: core.MarkerNeedsOwner, On: true}}
	for i := 0; i < 2; i++ {
		if err := h.a.SetMarker(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	it, _ := h.f.Tracker().Item(context.Background(), item)
	n := 0
	for _, m := range it.Markers {
		if m == ports.MarkerNeedsOwner {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("markers = %v", it.Markers)
	}
}

type notes struct{ got []ports.Notification }

func (n *notes) Notify(_ context.Context, x ports.Notification) error {
	n.got = append(n.got, x)
	return nil
}

func TestFR11AC3_NotifyTheOwner(t *testing.T) {
	h := newHarness(t)
	n := &notes{}
	h.a.Notifier = n
	err := h.a.Notify(context.Background(), NotifyRequest{Item: "4", URL: "fake://issues/4", Action: core.Notify{Key: "k", Event: contract.EventEscalate, Reason: core.ReasonTimeLimit}})
	if err != nil || len(n.got) != 1 || n.got[0].Project != "p" || n.got[0].Reason != core.ReasonTimeLimit || !strings.Contains(n.got[0].Message, "time limit") {
		t.Fatalf("notifications = %+v, %v", n.got, err)
	}
}

// TestFR7_CheckMergeAndMerge reads the merge facts, then merges pinned to
// the head as the loop; a retry finds it merged.
func TestFR7_CheckMergeAndMerge(t *testing.T) {
	h := newHarness(t)
	item := h.item()
	ch := h.implemented(item)
	mc, err := h.a.CheckMerge(context.Background(), MergeCheckRequest{Action: core.CheckMerge{Change: string(ch.Ref), Head: string(ch.Head)}, Pusher: core.PusherCoder})
	if err != nil {
		t.Fatal(err)
	}
	if mc.Head != string(ch.Head) || !mc.ContainsBase || len(mc.Gates) != 1 || mc.Gates[0].State != core.GateMissing || mc.Pusher != core.PusherCoder {
		t.Fatalf("checked = %+v", mc)
	}
	h.f.SetGate(ch.Head, "ci", ports.GatePass)
	act := core.Merge{Key: "item/p/1#1:merge:8", Change: string(ch.Ref), Pinned: string(ch.Head)}
	for i := 0; i < 2; i++ {
		res, err := h.a.Merge(context.Background(), act)
		if err != nil || res.Outcome != core.MergeMerged {
			t.Fatalf("merge %d = %+v, %v", i, res, err)
		}
	}
	got, _ := h.f.Host().Change(context.Background(), ch.Ref)
	if got.State != ports.ChangeStateMerged || got.MergedBy == nil || got.MergedBy.ID != fake.Loop.ID {
		t.Fatalf("change = %+v", got)
	}
	if tr, err := h.a.TrustCheck(context.Background(), string(ch.Ref)); err != nil || len(tr.Causes) != 0 {
		t.Fatalf("trust = %+v, %v", tr, err)
	}
}

// TestFR8_FollowUpIsFiledOnceWithReferencesOnly: one follow-up with the
// profile's markers and a row per finding, no reviewer text, a comment on
// the original item, and a retry files nothing more.
func TestFR8_FollowUpIsFiledOnceWithReferencesOnly(t *testing.T) {
	h := newHarness(t)
	x := &texts{WorkTracker: h.a.Tracker}
	h.a.Tracker = x
	h.a.FollowUpType = ports.MarkerTypeBug
	item := h.item()
	ch := h.implemented(item)
	h.f.SetGate(ch.Head, "ci", ports.GatePass)
	h.a.Merge(context.Background(), core.Merge{Key: "m", Change: string(ch.Ref), Pinned: string(ch.Head)})
	act := core.FileFollowUp{Key: "followup:" + string(ch.Ref), Item: string(item), Change: string(ch.Ref),
		Rows: []core.FollowUpRow{{ID: "F5", Score: 3, Status: contract.StatusPartiallyAddressed, Comment: "T9"}}}
	var refs []string
	for i := 0; i < 2; i++ {
		ref, err := h.a.FileFollowUp(context.Background(), FollowUpRequest{Action: act})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if refs[0] != refs[1] {
		t.Fatalf("two follow-ups: %v", refs)
	}
	fu, err := h.f.Tracker().Item(context.Background(), ports.ItemRef(refs[0]))
	if err != nil {
		t.Fatal(err)
	}
	has := map[ports.Marker]bool{}
	for _, m := range fu.Markers {
		has[m] = true
	}
	if !has[ports.MarkerTypeBug] || !has[ports.MarkerFollowUp] || has[ports.MarkerReady] {
		t.Fatalf("markers = %v", fu.Markers)
	}
	body := untrusted.Read(x.items[0].Body)
	if !strings.Contains(body, "| F5 | 3 | partially_addressed |") || !strings.Contains(body, string(item)) || !strings.Contains(body, "fake://pulls/"+string(ch.Ref)) {
		t.Fatalf("body = %s", body)
	}
	var cs []string
	for _, c := range x.comments {
		if strings.Contains(c, refs[0]) {
			cs = append(cs, c)
		}
	}
	recs, _ := h.f.Tracker().FindRecords(context.Background(), item, ports.RecordFollowUp)
	if len(cs) == 0 || len(recs) != 1 {
		t.Fatalf("comments %v, followup records on the item %d", cs, len(recs))
	}
}

func TestFR13AC6_ApproveCommitsWritesTheRecord(t *testing.T) {
	h := newHarness(t)
	item := h.item()
	act := core.ApproveCommits{Key: "item/p/1#1:approve:4", Commits: []string{strings.Repeat("a", 40)}}
	if err := h.a.ApproveCommits(context.Background(), ApproveRequest{Item: string(item), Claim: 1, Action: act}); err != nil {
		t.Fatal(err)
	}
	recs, err := h.f.Tracker().FindRecords(context.Background(), item, ports.RecordApprovedCommits)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	var c contract.Record
	json.Unmarshal(recs[0].Data, &c)
	if c.Claim != 1 || len(c.Commits) != 1 {
		t.Fatalf("record = %+v", c)
	}
}

func TestRunStageRefusesStagesWithoutAHandler(t *testing.T) {
	h := newHarness(t)
	for _, rs := range []core.RunStage{
		{RunID: "r1", Stage: contract.StageMint},
		{RunID: "r2", Stage: contract.StageReview, Change: "1"},
		{RunID: "r3", Stage: contract.StageAddress},
	} {
		if _, err := h.a.RunStage(context.Background(), h.req("1", rs)); err == nil {
			t.Errorf("%s: no error", rs.Stage)
		}
	}
	if len(h.runs) != 0 {
		t.Fatal("a handler ran")
	}
}
