package recording_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/adapters/recording"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// snapshot is everything a write could change in the fake.
type snapshot struct {
	item     ports.Item
	tEvents  int
	hEvents  int
	change   ports.Change
	comments int
}

func snap(t *testing.T, f *fake.Fake, item ports.ItemRef, ch ports.ChangeRef) snapshot {
	t.Helper()
	ctx := context.Background()
	it, err := f.Tracker().Item(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	te, _, err := f.Tracker().Changes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	he, _, err := f.Host().Changes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.Host().Change(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := f.Tracker().FindRecords(ctx, item, ports.RecordStatus)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot{item: it, tEvents: len(te), hEvents: len(he), change: c, comments: len(recs)}
}

// TestFR1AC6_RecordingAdaptersSendNoWrite: every write method of both
// ports is recorded, and none of them reaches the fake.
func TestFR1AC6_RecordingAdaptersSendNoWrite(t *testing.T) {
	ctx := context.Background()
	f := fake.New(fake.DefaultConfig())
	item, err := f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeFeature)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.NewItem(fake.Owner, "t", "b", ports.MarkerReady)
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + string(item)
	end, err := f.StartCoderRun(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Push(fake.Coder, branch); err != nil {
		t.Fatal(err)
	}
	end()
	ch, err := f.Host().OpenChange(context.Background(), ports.NewChange{Item: item, Branch: branch, Base: "main",
		Record: ports.Record{Type: ports.RecordChange, Key: "open"}}, ports.IdentityCoder, "open")
	if err != nil {
		t.Fatal(err)
	}
	before := snap(t, f, item, ch)

	log := &recording.Log{}
	tr := recording.Tracker{WorkTracker: f.Tracker(), Log: log}
	h := recording.Host{CodeHost: f.Host(), Log: log}
	rec := ports.Record{Type: ports.RecordStatus, Key: "k"}
	if v, err := tr.Claim(ctx, item, before.item.Version, "k1"); err != nil || v != before.item.Version {
		t.Errorf("Claim = %q, %v", v, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tr.SetMarker(ctx, item, ports.MarkerNeedsOwner, true, "k2"))
	_, err = tr.Comment(ctx, item, rec, ports.UntrustedText{}, "k3")
	must(err)
	_, err = tr.CreateItem(ctx, ports.NewItem{Record: rec}, "k4")
	must(err)
	must(tr.AddDependency(ctx, item, other, "k5"))
	must(tr.Close(ctx, item, ports.CloseCompleted, "k6"))
	_, err = h.OpenChange(ctx, ports.NewChange{Item: other, Branch: "agent/2"}, ports.IdentityCoder, "k7")
	must(err)
	must(h.SetLabel(ctx, ch, ports.MarkerPaused, true, "k8"))
	_, err = h.PostReview(ctx, ch, ports.ReviewPost{Approve: true}, "k9")
	must(err)
	must(h.ResolveFinding(ctx, ch, "F1", "k10"))
	must(h.PostReply(ctx, ch, "t1", rec, ports.UntrustedText{}, "k11"))
	if out, err := h.Merge(ctx, ch, before.change.Head, ports.MergeMethodSquash, "k12"); err != nil || out == ports.MergeMerged {
		t.Errorf("Merge = %q, %v", out, err)
	}

	if got := len(log.Writes()); got != 12 {
		t.Errorf("recorded %d writes, want 12: %v", got, log.Writes())
	}
	for i, w := range log.Writes() {
		if w.Key == "" || w.Method == "" || w.String() == "" {
			t.Errorf("write %d is incomplete: %+v", i, w)
		}
	}
	if after := snap(t, f, item, ch); !reflect.DeepEqual(before, after) {
		t.Errorf("the fake changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestFR1AC6_RecordingAdaptersPassReadsThrough: reads go to the real
// adapter.
func TestFR1AC6_RecordingAdaptersPassReadsThrough(t *testing.T) {
	f := fake.New(fake.Config{})
	if _, err := f.NewItem(fake.Owner, "t", "b", ports.MarkerReady); err != nil {
		t.Fatal(err)
	}
	tr := recording.Tracker{WorkTracker: f.Tracker(), Log: &recording.Log{}}
	cs, err := tr.Candidates(context.Background())
	if err != nil || len(cs) != 1 {
		t.Errorf("Candidates = %v, %v", cs, err)
	}
}
