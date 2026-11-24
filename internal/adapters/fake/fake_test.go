package fake_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func rec(rt ports.RecordType, k ports.IdemKey) ports.Record {
	return ports.Record{Type: rt, Key: k, Data: []byte(`{"type":"` + string(rt) + `"}`)}
}

// project is a fake with one ready item and its open loop change.
type project struct {
	f    *fake.Fake
	item ports.ItemRef
	ch   ports.ChangeRef
	head ports.Commit
	tc   ports.Cursor // tracker cursor
	hc   ports.Cursor // host cursor
	dep  ports.ItemRef
}

func newProject(t *testing.T) *project {
	t.Helper()
	f := fake.New(fake.DefaultConfig())
	p := &project{f: f}
	var err error
	p.item, err = f.NewItem(fake.Owner, "title", "body", ports.MarkerReady, ports.MarkerTypeFeature)
	must(t, err)
	end, err := f.StartCoderRun(p.item)
	must(t, err)
	p.head, err = f.Push(fake.Coder, "agent/"+string(p.item))
	must(t, err)
	end()
	p.ch, err = f.Host().OpenChange(ctx, ports.NewChange{Item: p.item, Branch: "agent/" + string(p.item), Base: "main", Record: rec(ports.RecordChange, "open")}, ports.IdentityCoder, "open")
	must(t, err)
	_, p.tc, err = f.Tracker().Changes(ctx, "")
	must(t, err)
	_, p.hc, err = f.Host().Changes(ctx, "")
	must(t, err)
	return p
}

func (p *project) itemEvents(t *testing.T) []ports.ItemEvent {
	t.Helper()
	var all []ports.ItemEvent
	for {
		evs, next, err := p.f.Tracker().Changes(ctx, p.tc)
		must(t, err)
		p.tc = next
		if len(evs) == 0 {
			return all
		}
		all = append(all, evs...)
	}
}

func (p *project) changeEvents(t *testing.T) []ports.ChangeEvent {
	t.Helper()
	var all []ports.ChangeEvent
	for {
		evs, next, err := p.f.Host().Changes(ctx, p.hc)
		must(t, err)
		p.hc = next
		if len(evs) == 0 {
			return all
		}
		all = append(all, evs...)
	}
}

// only returns the single event, failing otherwise.
func only[T any](t *testing.T, evs []T) T {
	t.Helper()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	return evs[0]
}

func (p *project) manual(t *testing.T) []ports.ManualComment {
	t.Helper()
	mcs, err := p.f.Host().ManualComments(ctx, p.ch)
	must(t, err)
	return mcs
}

// TestFR12AC4_ManualActionsCanBeScripted scripts each v0.1 row of FR-12
// AC4 and checks the fake reports what the "detects" column says.
func TestFR12AC4_ManualActionsCanBeScripted(t *testing.T) {
	t.Run("OwnerMergesChange", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.MergeChange(fake.Owner, p.ch))
		ev := only(t, p.changeEvents(t))
		if m, ok := ev.Detail.(ports.Merged); !ok || m.Actor.ID != fake.Owner.ID {
			t.Fatalf("event %+v, want Merged by the owner", ev.Detail)
		}
		c, _ := p.f.Host().Change(ctx, p.ch)
		if c.State != ports.ChangeStateMerged || c.MergedBy.ID != fake.Owner.ID {
			t.Errorf("change %s merged by %v", c.State, c.MergedBy)
		}
	})
	t.Run("OwnerClosesItem", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.CloseItem(fake.Owner, p.item, ports.CloseNotDone))
		ev := only(t, p.itemEvents(t))
		if c, ok := ev.Detail.(ports.ItemClosed); !ok || c.Actor.ID != fake.Owner.ID || c.Reason != ports.CloseNotDone {
			t.Fatalf("event %+v", ev.Detail)
		}
		if c, _ := p.f.Host().Change(ctx, p.ch); c.State != ports.ChangeStateOpen {
			t.Errorf("closing the item changed the change to %s", c.State)
		}
	})
	t.Run("OwnerClosesAndReopensChange", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.CloseChange(fake.Owner, p.ch))
		if c, _ := p.f.Host().ChangeForItem(ctx, p.item); c != nil {
			t.Error("a closed change is still the item's change")
		}
		must(t, p.f.ReopenChange(fake.Owner, p.ch))
		evs := p.changeEvents(t)
		if _, ok := evs[0].Detail.(ports.ChangeClosed); !ok || len(evs) != 2 {
			t.Fatalf("events %+v", evs)
		}
		if _, ok := evs[1].Detail.(ports.ChangeReopened); !ok {
			t.Fatalf("event %+v, want ChangeReopened", evs[1].Detail)
		}
		if c, _ := p.f.Host().ChangeForItem(ctx, p.item); c == nil || c.Ref != p.ch {
			t.Error("a reopened change isn't adopted")
		}
	})
	t.Run("OwnerReopensItem", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.CloseItem(fake.Owner, p.item, ports.CloseCompleted))
		must(t, p.f.ReopenItem(fake.Owner, p.item))
		evs := p.itemEvents(t)
		if _, ok := evs[len(evs)-1].Detail.(ports.ItemReopened); !ok {
			t.Fatalf("events %+v", evs)
		}
		if it, _ := p.f.Tracker().Item(ctx, p.item); it.CloseReason != ports.CloseOpen || it.ClosedBy != nil {
			t.Errorf("reopened item: %s by %v", it.CloseReason, it.ClosedBy)
		}
	})
	// Every label row: ready, in-progress, needs-owner, paused, aborted,
	// blocked, type, follow-up and plan, by the owner and by another human.
	for _, m := range []ports.Marker{ports.MarkerReady, ports.MarkerInProgress, ports.MarkerNeedsOwner, ports.MarkerPaused,
		ports.MarkerAborted, ports.MarkerBlocked, ports.MarkerTypeBug, ports.MarkerFollowUp, ports.MarkerPlan} {
		for _, who := range []ports.Actor{fake.Owner, fake.Human} {
			t.Run("Label_"+string(m)+"_by_"+who.Login, func(t *testing.T) {
				p := newProject(t)
				it, _ := p.f.Tracker().Item(ctx, p.item)
				had := false
				for _, x := range it.Markers {
					had = had || x == m
				}
				must(t, p.f.SetMarker(who, p.item, m, !had))
				must(t, p.f.SetMarker(who, p.item, m, had))
				evs := p.itemEvents(t)
				if len(evs) != 2 {
					t.Fatalf("got %d events, want 2", len(evs))
				}
				for i, ev := range evs {
					mc, ok := ev.Detail.(ports.MarkerChanged)
					if !ok || mc.Actor.ID != who.ID || mc.Marker != m || mc.On != (i == 0) != had {
						t.Errorf("event %d = %+v", i, ev.Detail)
					}
				}
			})
		}
	}
	t.Run("OwnerRemovesReadyFromUnclaimedItem", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.SetMarker(fake.Owner, p.item, ports.MarkerReady, false))
		if c, _ := p.f.Tracker().Candidates(ctx); len(c) != 0 {
			t.Errorf("candidates %+v, want none", c)
		}
	})
	t.Run("AssignHuman", func(t *testing.T) {
		p := newProject(t)
		h := fake.Human
		must(t, p.f.Assign(fake.Owner, p.item, &h))
		ev := only(t, p.itemEvents(t))
		if a, ok := ev.Detail.(ports.AssigneeChanged); !ok || !a.Human || a.Actor.ID != fake.Owner.ID {
			t.Fatalf("event %+v", ev.Detail)
		}
		if it, _ := p.f.Tracker().Item(ctx, p.item); len(it.Assignees) != 1 || it.Assignees[0].ID != h.ID {
			t.Errorf("assignees %+v", it.Assignees)
		}
		must(t, p.f.Assign(fake.Owner, p.item, nil))
		if a := only(t, p.itemEvents(t)).Detail.(ports.AssigneeChanged); a.Human {
			t.Error("unassigning reports a human assignee")
		}
	})
	t.Run("OwnerPushesToBranch", func(t *testing.T) {
		p := newProject(t)
		h, err := p.f.Push(fake.Owner, "agent/"+string(p.item))
		must(t, err)
		ev := only(t, p.changeEvents(t))
		if hm, ok := ev.Detail.(ports.HeadMoved); !ok || hm.Commit != h || hm.Pusher != ports.PusherOwner {
			t.Fatalf("event %+v, want HeadMoved by the owner", ev.Detail)
		}
	})
	t.Run("UntrustedPush", func(t *testing.T) {
		p := newProject(t)
		if _, err := p.f.Push(fake.Human, "agent/"+string(p.item)); !errors.Is(err, ports.ErrPermission) {
			t.Errorf("human push to a loop branch: %v, want ErrPermission", err)
		}
		// Outside the prefix the push goes through, and Trust names it.
		_, err := p.f.Push(fake.Owner, "feature-x")
		must(t, err)
		ch, err := p.f.OpenChange(fake.Owner, p.item, "feature-x")
		must(t, err)
		h, err := p.f.Push(fake.Human, "feature-x")
		must(t, err)
		r, err := p.f.Host().Trust(ctx, ch)
		must(t, err)
		if len(r.Causes) != 1 || r.Causes[0] != ports.TrustBranchOutsidePrefix || r.Commits[len(r.Commits)-1] != h {
			t.Errorf("trust %+v", r)
		}
	})
	t.Run("HumanManualReview", func(t *testing.T) {
		p := newProject(t)
		th, err := p.f.ReviewComment(fake.Human, p.ch, "a.go", 3, "comment")
		must(t, err)
		_, err = p.f.SubmitReview(fake.Human, p.ch, fake.ReviewChangesRequested, "please fix", nil)
		must(t, err)
		_, err = p.f.SubmitReview(fake.Human, p.ch, fake.ReviewApprove, "looks good", nil)
		must(t, err)
		for _, ev := range p.changeEvents(t) {
			if rs, ok := ev.Detail.(ports.ReviewSubmitted); !ok || rs.Actor.ID != fake.Human.ID {
				t.Errorf("event %+v, want ReviewSubmitted by the human", ev.Detail)
			}
		}
		mcs := p.manual(t)
		if len(mcs) != 3 || mcs[0].Kind != ports.ManualCommentThread || mcs[0].Thread != th ||
			mcs[1].Kind != ports.ManualCommentReviewBody || mcs[2].Kind != ports.ManualCommentReviewBody {
			t.Fatalf("manual comments %+v", mcs)
		}
		for _, mc := range mcs {
			if mc.Cleared || !strings.HasPrefix(mc.ID, "MC-") {
				t.Errorf("manual comment %+v", mc)
			}
		}
	})
	t.Run("HumanResolvesOrDismisses", func(t *testing.T) {
		p := newProject(t)
		th, _ := p.f.ReviewComment(fake.Owner, p.ch, "a.go", 0, "comment")
		r, _ := p.f.SubmitReview(fake.Owner, p.ch, fake.ReviewComment, "body", nil)
		must(t, p.f.ResolveThread(fake.Human, p.ch, th))
		must(t, p.f.DismissReview(fake.Human, p.ch, r))
		for _, mc := range p.manual(t) {
			if !mc.Cleared {
				t.Errorf("not cleared: %+v", mc)
			}
		}
	})
	t.Run("IdentityResolves", func(t *testing.T) {
		p := newProject(t)
		th, _ := p.f.ReviewComment(fake.Owner, p.ch, "a.go", 1, "comment")
		must(t, p.f.ResolveThread(fake.Coder, p.ch, th))
		if ev := only(t, p.changeEvents(t)[1:]); ev.Detail.(ports.ThreadResolved).Actor.ID != fake.Coder.ID {
			t.Errorf("event %+v", ev.Detail)
		}
		if mc := p.manual(t)[0]; mc.Cleared {
			t.Error("an identity's resolve cleared a manual comment")
		}
	})
	t.Run("OwnerCommentsOutsideReview", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.CommentItem(fake.Owner, p.item, "note", nil))
		must(t, p.f.CommentChange(fake.Owner, p.ch, "note"))
		if c, ok := only(t, p.itemEvents(t)).Detail.(ports.Commented); !ok || c.Actor.ID != fake.Owner.ID {
			t.Error("no Commented event")
		}
		if len(p.manual(t)) != 0 {
			t.Error("a conversation comment is a manual comment")
		}
		if recs, _ := p.f.Tracker().FindRecords(ctx, p.item, ports.RecordStatus); len(recs) != 0 {
			t.Error("a plain comment is a record")
		}
	})
	t.Run("OwnerOpensChangeForUnclaimedItem", func(t *testing.T) {
		f := fake.New(fake.DefaultConfig())
		it, _ := f.NewItem(fake.Owner, "t", "b", ports.MarkerReady)
		_, cur, _ := f.Host().Changes(ctx, "")
		_, err := f.Push(fake.Owner, "owner-fix")
		must(t, err)
		ch, err := f.OpenChange(fake.Owner, it, "owner-fix")
		must(t, err)
		evs, _, _ := f.Host().Changes(ctx, cur)
		if o, ok := only(t, evs).Detail.(ports.Opened); !ok || o.Item != it || o.Actor.ID != fake.Owner.ID {
			t.Fatalf("event %+v", evs[0].Detail)
		}
		if c, _ := f.Host().ChangeForItem(ctx, it); c == nil || c.Ref != ch || c.Author.ID != fake.Owner.ID {
			t.Errorf("ChangeForItem = %+v", c)
		}
	})
}

// TestNFR2AC1_KillPointsThenRetry kills every write method before and after
// its write, retries with the same key, and checks one result.
func TestNFR2AC1_KillPointsThenRetry(t *testing.T) {
	type write struct {
		name string
		call func(p *project) error
		// check runs after the retry.
		check func(t *testing.T, p *project)
	}
	writes := []write{
		{"Claim", func(p *project) error {
			_, err := p.f.Tracker().Claim(ctx, p.item, "v1", "k")
			return err
		}, func(t *testing.T, p *project) {
			// in_progress, ready and the claim record (#14 D1).
			if n := len(p.itemEvents(t)); n != 3 {
				t.Errorf("%d events after a retried claim, want 3", n)
			}
			recs, err := p.f.Tracker().FindRecords(ctx, p.item, ports.RecordStatus)
			must(t, err)
			if len(recs) != 1 || recs[0].Key != "k" {
				t.Errorf("claim records after a retried claim: %+v", recs)
			}
		}},
		{"SetMarker", func(p *project) error {
			return p.f.Tracker().SetMarker(ctx, p.item, ports.MarkerBlocked, true, "k")
		}, func(t *testing.T, p *project) { only(t, p.itemEvents(t)) }},
		{"Comment", func(p *project) error {
			_, err := p.f.Tracker().Comment(ctx, p.item, rec(ports.RecordStatus, "k"), untrusted.New("x"), "k")
			return err
		}, func(t *testing.T, p *project) {
			if r, _ := p.f.Tracker().FindRecords(ctx, p.item, ports.RecordStatus); len(r) != 1 {
				t.Errorf("%d records", len(r))
			}
		}},
		{"CreateItem", func(p *project) error {
			_, err := p.f.Tracker().CreateItem(ctx, ports.NewItem{Record: rec(ports.RecordFollowUp, "k")}, "k")
			return err
		}, func(t *testing.T, p *project) {
			r1, _ := p.f.Tracker().CreateItem(ctx, ports.NewItem{Record: rec(ports.RecordFollowUp, "k")}, "k")
			r2, _ := p.f.Tracker().CreateItem(ctx, ports.NewItem{Record: rec(ports.RecordFollowUp, "k")}, "k")
			if r1 != r2 {
				t.Errorf("created %s and %s", r1, r2)
			}
		}},
		{"AddDependency", func(p *project) error {
			return p.f.Tracker().AddDependency(ctx, p.item, depItem(p), "k")
		}, func(t *testing.T, p *project) {
			if d, _ := p.f.Tracker().Dependencies(ctx, p.item); len(d) != 1 {
				t.Errorf("%d dependencies", len(d))
			}
		}},
		{"Close", func(p *project) error {
			return p.f.Tracker().Close(ctx, p.item, ports.CloseCompleted, "k")
		}, func(t *testing.T, p *project) { only(t, p.itemEvents(t)) }},
		{"SetLabel", func(p *project) error {
			return p.f.Host().SetLabel(ctx, p.ch, ports.MarkerAborted, true, "k")
		}, func(t *testing.T, p *project) {
			if c, _ := p.f.Host().Change(ctx, p.ch); len(c.Markers) != 1 {
				t.Errorf("markers %v", c.Markers)
			}
		}},
		{"PostReview", func(p *project) error {
			_, err := p.f.Host().PostReview(ctx, p.ch, ports.ReviewPost{Record: rec(ports.RecordReview, "k")}, "k")
			return err
		}, func(t *testing.T, p *project) {
			if r, _ := p.f.Host().ReviewRecords(ctx, p.ch); len(r) != 1 {
				t.Errorf("%d reviews", len(r))
			}
		}},
		{"PostReply", func(p *project) error {
			return p.f.Host().PostReply(ctx, p.ch, ports.ThreadRef(p.ch), rec(ports.RecordReply, "k"), untrusted.New("x"), "k")
		}, func(t *testing.T, p *project) {
			recs, err := p.f.Replies(p.ch, ports.ThreadRef(p.ch))
			must(t, err)
			if len(recs) != 1 || recs[0].Key != "k" || recs[0].Author.ID != fake.Coder.ID {
				t.Errorf("replies after a retry: %+v, want one coder reply", recs)
			}
		}},
		{"Merge", func(p *project) error {
			_ = p.f.SetGate(p.head, "ci", ports.GatePass)
			out, err := p.f.Host().Merge(ctx, p.ch, p.head, ports.MergeMethodSquash, "k")
			if err == nil && out != ports.MergeMerged {
				return errors.New(string(out))
			}
			return err
		}, func(t *testing.T, p *project) {
			n := 0
			for _, ev := range p.changeEvents(t) {
				if _, ok := ev.Detail.(ports.Merged); ok {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%d merges", n)
			}
		}},
	}
	for _, w := range writes {
		for _, ph := range []fake.Phase{fake.BeforeWrite, fake.AfterWrite} {
			t.Run(w.name+map[fake.Phase]string{fake.BeforeWrite: "_Before", fake.AfterWrite: "_After"}[ph], func(t *testing.T) {
				p := newProject(t)
				p.f.KillAt(w.name, ph)
				if err := w.call(p); !errors.Is(err, fake.ErrKilled) || !errors.Is(err, ports.ErrTransient) {
					t.Fatalf("killed call: %v, want ErrKilled", err)
				}
				must(t, w.call(p))
				w.check(t, p)
			})
		}
	}
	t.Run("OpenChange", func(t *testing.T) {
		for _, ph := range []fake.Phase{fake.BeforeWrite, fake.AfterWrite} {
			f := fake.New(fake.DefaultConfig())
			it, _ := f.NewItem(fake.Owner, "t", "b")
			end, _ := f.StartCoderRun(it)
			_, err := f.Push(fake.Coder, "agent/"+string(it))
			must(t, err)
			end()
			n := ports.NewChange{Item: it, Branch: "agent/" + string(it), Record: rec(ports.RecordChange, "k")}
			f.KillAt("OpenChange", ph)
			if _, err := f.Host().OpenChange(ctx, n, ports.IdentityCoder, "k"); !errors.Is(err, fake.ErrKilled) {
				t.Fatalf("killed: %v", err)
			}
			ref, err := f.Host().OpenChange(ctx, n, ports.IdentityCoder, "k")
			must(t, err)
			if c, _ := f.Host().ChangeForItem(ctx, it); c == nil || c.Ref != ref {
				t.Errorf("phase %d: ChangeForItem = %+v", ph, c)
			}
		}
	})
	t.Run("ResolveFinding", func(t *testing.T) {
		p := newProject(t)
		_, err := p.f.Host().PostReview(ctx, p.ch, ports.ReviewPost{Record: rec(ports.RecordReview, "r"),
			Findings: []ports.FindingPost{{ID: "F1", Path: "a.go", Line: 1, Record: rec(ports.RecordFinding, "f")}}}, "r")
		must(t, err)
		p.changeEvents(t)
		p.f.KillAt("ResolveFinding", fake.AfterWrite)
		if err := p.f.Host().ResolveFinding(ctx, p.ch, "F1", "k"); !errors.Is(err, fake.ErrKilled) {
			t.Fatalf("killed: %v", err)
		}
		must(t, p.f.Host().ResolveFinding(ctx, p.ch, "F1", "k"))
		only(t, p.changeEvents(t))
	})
}

// depItem returns one other item of the project, created on first use.
func depItem(p *project) ports.ItemRef {
	if p.dep == "" {
		p.dep, _ = p.f.NewItem(fake.Owner, "dep", "dep")
	}
	return p.dep
}

// TestPostReplyToThreadAfterKill: the same for a reply in a thread.
func TestPostReplyToThreadAfterKill(t *testing.T) {
	p := newProject(t)
	th, err := p.f.ReviewComment(fake.Owner, p.ch, "a.go", 1, "why?")
	must(t, err)
	p.f.KillAt("PostReply", fake.AfterWrite)
	if err := p.f.Host().PostReply(ctx, p.ch, th, rec(ports.RecordReply, "k"), untrusted.New("x"), "k"); !errors.Is(err, fake.ErrKilled) {
		t.Fatalf("killed: %v", err)
	}
	must(t, p.f.Host().PostReply(ctx, p.ch, th, rec(ports.RecordReply, "k"), untrusted.New("x"), "k"))
	recs, err := p.f.Replies(p.ch, th)
	must(t, err)
	if len(recs) != 1 {
		t.Errorf("%d replies in the thread, want 1", len(recs))
	}
	if _, err := p.f.Replies(p.ch, "T-none"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("unknown thread: %v", err)
	}
	if err := p.f.Host().PostReply(ctx, p.ch, "T-none", rec(ports.RecordReply, "x"), untrusted.New("x"), "x"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("reply to an unknown thread: %v", err)
	}
}

func TestKillAtRejectsReads(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("KillAt on a read method didn't panic")
		}
	}()
	fake.New(fake.DefaultConfig()).KillAt("Item", fake.AfterWrite)
}

func TestErrorClasses(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	if _, err := f.Tracker().Item(ctx, "404"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
	if _, err := f.Host().Change(ctx, "404"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing change: %v", err)
	}
	if _, _, err := f.Tracker().Changes(ctx, "not-a-cursor"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("bad cursor: %v", err)
	}
	if _, err := f.Host().OpenChange(ctx, ports.NewChange{Branch: "agent/none"}, ports.IdentityCoder, "k"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("open change on a missing branch: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Tracker().Candidates(cancelled); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("cancelled context: %v", err)
	}
	it, _ := f.NewItem(fake.Owner, "t", "b")
	if err := f.Tracker().Close(ctx, it, ports.CloseOpen, "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("close with reason open: %v", err)
	}
	if err := f.Tracker().AddDependency(ctx, it, it, "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("self dependency: %v", err)
	}
	if _, err := f.Host().OpenChange(ctx, ports.NewChange{Branch: "main"}, ports.Identity("nobody"), "k"); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("unknown identity: %v", err)
	}
}

// TestErrorClassesD8 covers the D8 outcomes the other tests don't.
func TestErrorClassesD8(t *testing.T) {
	p := newProject(t)
	if _, _, err := p.f.Host().Changes(ctx, "999999"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("cursor past the end: %v", err)
	}
	if _, err := p.f.Host().OpenChange(ctx, ports.NewChange{Branch: "main", Record: rec(ports.RecordChange, "b")}, ports.IdentityCoder, "b"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("change from the base branch: %v", err)
	}
	must(t, p.f.SetGate(p.head, "ci", ports.GatePass))
	if out, err := p.f.Host().Merge(ctx, p.ch, p.head, ports.MergeMethodSquash, "m"); err != nil || out != ports.MergeMerged {
		t.Fatalf("merge: %v %v", out, err)
	}
	if out, _ := p.f.Host().Merge(ctx, p.ch, "0000", ports.MergeMethodSquash, "m2"); out != ports.MergeNotMergeable {
		t.Errorf("merge of a change merged at another commit: %s", out)
	}
	v := func() ports.Version { it, _ := p.f.Tracker().Item(ctx, p.item); return it.Version }
	must(t, p.f.CloseItem(fake.Owner, p.item, ports.CloseCompleted))
	if _, err := p.f.Tracker().Claim(ctx, p.item, v(), "c"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("claim of a closed item: %v", err)
	}
}

func TestOpenChangeConflictsWithOpenChangeOnBranch(t *testing.T) {
	p := newProject(t)
	_, err := p.f.Host().OpenChange(ctx, ports.NewChange{Item: p.item, Branch: "agent/" + string(p.item), Record: rec(ports.RecordChange, "other")}, ports.IdentityCoder, "other")
	if !errors.Is(err, ports.ErrConflict) {
		t.Errorf("second change on the branch: %v, want ErrConflict", err)
	}
}

// TestChangeLinkBranchNames: a coder change record links only on the item's
// branch names (INTEGRATION-SPEC §3.3, §6.2, D44).
func TestChangeLinkBranchNames(t *testing.T) {
	for branch, linked := range map[string]bool{"agent/1": true, "agent/1-2": true, "agent/1-10": true, "agent/1-1": false, "agent/12": false, "other/1": false} {
		f := fake.New(fake.DefaultConfig())
		it, _ := f.NewItem(fake.Owner, "t", "b") // ref "2": the base commit took 1
		end, _ := f.StartCoderRun(it)
		b := strings.Replace(branch, "/1", "/"+string(it), 1)
		_, err := f.Push(fake.Coder, b)
		end()
		must(t, err)
		_, err = f.Host().OpenChange(ctx, ports.NewChange{Item: it, Branch: b, Record: rec(ports.RecordChange, "k")}, ports.IdentityCoder, "k")
		must(t, err)
		c, _ := f.Host().ChangeForItem(ctx, it)
		if (c != nil) != linked {
			t.Errorf("branch %s: linked=%v, want %v", b, c != nil, linked)
		}
	}
}

func TestUnlistedBotIsNotManualCommenter(t *testing.T) {
	p := newProject(t)
	_, _ = p.f.ReviewComment(fake.OtherBot, p.ch, "a.go", 1, "bump")
	_, _ = p.f.SubmitReview(fake.OtherBot, p.ch, fake.ReviewChangesRequested, "", nil)
	if mcs := p.manual(t); len(mcs) != 0 {
		t.Errorf("unlisted bot comments are manual comments: %+v", mcs)
	}
}

func TestMergeRefusals(t *testing.T) {
	p := newProject(t)
	merge := func() ports.MergeOutcome {
		out, err := p.f.Host().Merge(ctx, p.ch, p.head, ports.MergeMethodMerge, "")
		must(t, err)
		return out
	}
	if out := merge(); out != ports.MergeNotMergeable {
		t.Errorf("missing gate: %s", out)
	}
	must(t, p.f.SetGate(p.head, "ci", ports.GateFail))
	if out := merge(); out != ports.MergeNotMergeable {
		t.Errorf("failed gate: %s", out)
	}
	must(t, p.f.SetGate(p.head, "ci", ports.GatePass))
	_, err := p.f.Push(fake.Owner, "main")
	must(t, err)
	if out := merge(); out != ports.MergeNotMergeable {
		t.Errorf("head lacks the base tip: %s", out)
	}
	must(t, p.f.CloseChange(fake.Owner, p.ch))
	if out := merge(); out != ports.MergeNotMergeable {
		t.Errorf("closed change: %s", out)
	}
}

func TestTrustHistory(t *testing.T) {
	t.Run("PushedWhileLoopRulesetDisabled", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.SetRuleset(fake.RulesetLoopBranches, fake.RulesetDisable))
		h, err := p.f.Push(fake.Human, "agent/"+string(p.item))
		must(t, err)
		must(t, p.f.SetRuleset(fake.RulesetLoopBranches, fake.RulesetEnable))
		r, _ := p.f.Host().Trust(ctx, p.ch)
		want := []ports.TrustCause{ports.TrustRulesetChanged, ports.TrustPushedBeforeRuleset}
		if len(r.Causes) != 2 || r.Causes[0] != want[0] || r.Causes[1] != want[1] || r.Commits[len(r.Commits)-1] != h {
			t.Errorf("trust %+v", r)
		}
	})
	t.Run("ChangeOpenedAfterRulesetChangeIsTrusted", func(t *testing.T) {
		f := fake.New(fake.DefaultConfig())
		must(t, f.SetRuleset(fake.RulesetTags, fake.RulesetChange))
		it, _ := f.NewItem(fake.Owner, "t", "b")
		end, _ := f.StartCoderRun(it)
		_, err := f.Push(fake.Coder, "agent/"+string(it))
		end()
		must(t, err)
		ch, _ := f.Host().OpenChange(ctx, ports.NewChange{Item: it, Branch: "agent/" + string(it), Record: rec(ports.RecordChange, "k")}, ports.IdentityCoder, "k")
		if r, _ := f.Host().Trust(ctx, ch); len(r.Causes) != 0 {
			t.Errorf("trust %+v", r)
		}
	})
	t.Run("AdminRemovedStillTaintsItsCommits", func(t *testing.T) {
		p := newProject(t)
		must(t, p.f.AddAdmin(fake.Human))
		h, err := p.f.Push(fake.Human, "agent/"+string(p.item))
		must(t, err)
		must(t, p.f.RemoveAdmin(fake.Human))
		r, _ := p.f.Host().Trust(ctx, p.ch)
		if len(r.Causes) != 1 || r.Causes[0] != ports.TrustAdminNotOwner || len(r.Commits) != 1 || r.Commits[0] != h {
			t.Errorf("trust %+v", r)
		}
	})
	t.Run("CoderPushWithoutRunRefused", func(t *testing.T) {
		p := newProject(t)
		if _, err := p.f.Push(fake.Coder, "agent/"+string(p.item)); !errors.Is(err, ports.ErrPermission) {
			t.Errorf("coder push with a revoked token: %v", err)
		}
	})
}

func TestValidate(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	if p := f.Tracker().Validate(ctx, ports.ProfileTracker{Adapter: "fake"}); len(p) != 0 {
		t.Errorf("problems %+v", p)
	}
	if p := f.Host().Validate(ctx, ports.ProfileCodeHost{Adapter: "github"}); len(p) != 1 || p[0].Severity != ports.SeverityError || p[0].Hint == "" {
		t.Errorf("problems %+v", p)
	}
}

// TestC15_PusherIndependentOfPaging: a push during another item's coder run
// is Unknown whatever page of the poll its HeadMoved is on (review R1).
func TestC15_PusherIndependentOfPaging(t *testing.T) {
	for _, size := range []int{1, 2, 100} {
		cfg := fake.DefaultConfig()
		cfg.PageSize = size
		f := fake.New(cfg)
		a, _ := f.NewItem(fake.Owner, "a", "a")
		b, _ := f.NewItem(fake.Owner, "b", "b")
		end, _ := f.StartCoderRun(a)
		head, err := f.Push(fake.Coder, "agent/"+string(a))
		must(t, err)
		end()
		ch, err := f.Host().OpenChange(ctx, ports.NewChange{Item: a, Branch: "agent/" + string(a), Record: rec(ports.RecordChange, "k")}, ports.IdentityCoder, "k")
		must(t, err)
		var cur ports.Cursor
		drain := func() []ports.ChangeEvent {
			var all []ports.ChangeEvent
			for {
				evs, next, err := f.Host().Changes(ctx, cur)
				must(t, err)
				cur = next
				if len(evs) == 0 {
					return all
				}
				all = append(all, evs...)
			}
		}
		drain()
		must(t, f.SetGate(head, "ci", ports.GatePending)) // one event before the push
		endB, _ := f.StartCoderRun(b)
		pushed, err := f.Push(fake.Owner, "agent/"+string(a))
		must(t, err)
		endB()
		var got ports.Pusher
		for _, ev := range drain() {
			if hm, ok := ev.Detail.(ports.HeadMoved); ok && hm.Commit == pushed {
				got = hm.Pusher
			}
		}
		if got != ports.PusherUnknown {
			t.Errorf("page size %d: pusher %q, want unknown", size, got)
		}
		r, _ := f.Host().Trust(ctx, ch)
		if len(r.Causes) == 0 || r.Causes[len(r.Causes)-1] != ports.TrustPusherUnknown {
			t.Errorf("page size %d: trust %+v, want pusher_unknown", size, r)
		}
	}
}

// TestC15_PusherUsesOpenChangeOnSharedBranch: when two changes used the
// same branch, the open one decides the item (review R5).
func TestC15_PusherUsesOpenChangeOnSharedBranch(t *testing.T) {
	for i := 0; i < 20; i++ {
		f := fake.New(fake.DefaultConfig())
		one, _ := f.NewItem(fake.Owner, "1", "1")
		two, _ := f.NewItem(fake.Owner, "2", "2")
		_, err := f.Push(fake.Owner, "feature-x")
		must(t, err)
		old, err := f.OpenChange(fake.Owner, one, "feature-x")
		must(t, err)
		must(t, f.CloseChange(fake.Owner, old))
		ch, err := f.OpenChange(fake.Owner, two, "feature-x")
		must(t, err)
		_, cur, _ := f.Host().Changes(ctx, "")
		end, _ := f.StartCoderRun(two)
		pushed, err := f.Push(fake.Coder, "feature-x")
		must(t, err)
		end()
		evs, _, _ := f.Host().Changes(ctx, cur)
		for _, ev := range evs {
			if hm, ok := ev.Detail.(ports.HeadMoved); ok && ev.Change == ch && hm.Commit == pushed && hm.Pusher != ports.PusherCoder {
				t.Fatalf("run %d: pusher %s, want coder", i, hm.Pusher)
			}
		}
	}
}

// TestCoverageGaps covers the small paths the suite and other tests miss.
func TestCoverageGaps(t *testing.T) {
	f := fake.New(fake.Config{}) // zero fields take the defaults
	if c := f.Config(); c.BaseBranch != "main" || c.BranchPrefix != "agent/" || c.Coder != fake.Coder {
		t.Errorf("defaults %+v", c)
	}
	start := f.Now()
	f.Advance(time.Hour)
	if f.Now().Sub(start) != time.Hour {
		t.Error("Advance didn't move the clock")
	}
	if c := f.Tracker().Capabilities(); c.Assign || !c.NativeDependencies || c.MaxBody == 0 {
		t.Errorf("tracker caps %+v", c)
	}
	if c := f.Host().Capabilities(); !c.PushRule || !c.FileLevelComments {
		t.Errorf("host caps %+v", c)
	}

	f = fake.New(fake.DefaultConfig()) // DefaultConfig sets Owners to fake.Owner
	// Tags: the owner may create one, once; the human may not.
	must(t, f.CreateTag(fake.Owner, "v1"))
	if err := f.CreateTag(fake.Owner, "v1"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("duplicate tag: %v", err)
	}
	if err := f.CreateTag(fake.Human, "v2"); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("human tag: %v", err)
	}
	if err := f.CreateTag(fake.Reviewer, "v3"); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("reviewer tag: %v", err)
	}

	// A follow-up item's body record is found, by the loop only.
	since := f.Now()
	ref, err := f.Tracker().CreateItem(ctx, ports.NewItem{Record: rec(ports.RecordFollowUp, "fu"), Since: since}, "fu")
	must(t, err)
	recs, err := f.Tracker().FindRecords(ctx, ref, ports.RecordFollowUp)
	must(t, err)
	if len(recs) != 1 || recs[0].Author.ID != fake.Loop.ID {
		t.Errorf("follow-up body record: %+v", recs)
	}

	// A change opened as the reviewer or loop identity has that author.
	it, _ := f.NewItem(fake.Owner, "t", "b")
	_, err = f.Push(fake.Owner, "agent/plan/x")
	must(t, err)
	ch, err := f.Host().OpenChange(ctx, ports.NewChange{Item: it, Branch: "agent/plan/x", Record: rec(ports.RecordProposal, "p")}, ports.IdentityLoop, "p")
	must(t, err)
	if c, _ := f.Host().Change(ctx, ch); c.Author.ID != fake.Loop.ID {
		t.Errorf("author %+v", c.Author)
	}

	// Pusher on a branch whose only change is closed: its item still counts.
	must(t, f.CloseChange(fake.Owner, ch))
	end, _ := f.StartCoderRun(it)
	pushed, err := f.Push(fake.Owner, "agent/plan/x")
	must(t, err)
	end()
	if got := f.PusherOf(pushed); got != ports.PusherCoder {
		t.Errorf("push on a branch whose only change is closed: pusher %q, want coder", got)
	}

	// The bot and reviewer can't push; a review in an unknown state fails.
	if _, err := f.Push(fake.Bot, "feature"); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("bot push: %v", err)
	}
	if _, err := f.SubmitReview(fake.Owner, ch, "LGTM", "", nil); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("bad review state: %v", err)
	}
	if err := f.CloseItem(fake.Owner, it, ports.CloseOpen); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("close with reason open: %v", err)
	}
}
