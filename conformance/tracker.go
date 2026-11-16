package conformance

import (
	"errors"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// c1: every write method called twice with the same key leaves one result
// (NFR-2 AC1).
func c1(s *suite) {
	e := s.env()
	ctx := s.ctx()
	_, cur := s.trackerFeed("")
	count := func(pred func(ports.ItemEvent) bool) int {
		evs, next := s.trackerFeed(cur)
		cur = next
		n := 0
		for _, ev := range evs {
			if pred(ev) {
				n++
			}
		}
		return n
	}

	it := s.item(ports.MarkerReady, ports.MarkerTypeFeature)
	v := s.getItem(it).Version
	k := s.key("claim")
	v1, err := e.Tracker.Claim(ctx, it, v, k)
	s.must(err)
	v2, err := e.Tracker.Claim(ctx, it, v, k)
	s.must(err)
	if v1 != v2 || s.getItem(it).Version != v2 {
		s.t.Errorf("Claim repeated: versions %s, %s, item %s", v1, v2, s.getItem(it).Version)
	}
	if n := count(func(ev ports.ItemEvent) bool {
		mc, ok := ev.Detail.(ports.MarkerChanged)
		return ok && mc.Marker == ports.MarkerInProgress && mc.On
	}); n != 1 {
		s.t.Errorf("Claim repeated: %d in_progress events, want 1", n)
	}
	claims, err := e.Tracker.FindRecords(ctx, it, ports.RecordStatus)
	s.must(err)
	if n := countKey(claims, k); n != 1 {
		s.t.Errorf("Claim repeated: %d claim records with the key, want 1 (INTEGRATION-SPEC §9.3 step 4)", n)
	}

	k = s.key("marker")
	s.must(e.Tracker.SetMarker(ctx, it, ports.MarkerBlocked, true, k))
	s.must(e.Tracker.SetMarker(ctx, it, ports.MarkerBlocked, true, k))
	if n := count(func(ev ports.ItemEvent) bool { _, ok := ev.Detail.(ports.MarkerChanged); return ok }); n != 1 {
		s.t.Errorf("SetMarker repeated: %d marker events, want 1", n)
	}

	k = s.key("comment")
	r := rec(ports.RecordStatus, k)
	c1ref, err := e.Tracker.Comment(ctx, it, r, ports.UntrustedText{}, k)
	s.must(err)
	c2ref, err := e.Tracker.Comment(ctx, it, r, ports.UntrustedText{}, k)
	s.must(err)
	recs, err := e.Tracker.FindRecords(ctx, it, ports.RecordStatus)
	s.must(err)
	if c1ref != c2ref || countKey(recs, k) != 1 {
		s.t.Errorf("Comment repeated: refs %s, %s; %d records with the key", c1ref, c2ref, countKey(recs, k))
	}

	k = s.key("create")
	n := ports.NewItem{Markers: []ports.Marker{ports.MarkerFollowUp}, Record: rec(ports.RecordFollowUp, k), Since: s.getItem(it).CreatedAt}
	i1, err := e.Tracker.CreateItem(ctx, n, k)
	s.must(err)
	i2, err := e.Tracker.CreateItem(ctx, n, k)
	s.must(err)
	if i1 != i2 {
		s.t.Errorf("CreateItem repeated: %s and %s", i1, i2)
	}

	k = s.key("dep")
	s.must(e.Tracker.AddDependency(ctx, it, i1, k))
	s.must(e.Tracker.AddDependency(ctx, it, i1, k))
	deps, err := e.Tracker.Dependencies(ctx, it)
	s.must(err)
	if len(deps) != 1 {
		s.t.Errorf("AddDependency repeated: %d dependencies, want 1", len(deps))
	}

	k = s.key("close")
	s.must(e.Tracker.Close(ctx, i1, ports.CloseCompleted, k))
	s.must(e.Tracker.Close(ctx, i1, ports.CloseCompleted, k))
	if n := count(func(ev ports.ItemEvent) bool { _, ok := ev.Detail.(ports.ItemClosed); return ok }); n != 1 {
		s.t.Errorf("Close repeated: %d close events, want 1", n)
	}

	if !s.hostless() {
		c1Host(s, it)
	}
}

func countKey(recs []ports.Record, k ports.IdemKey) int {
	n := 0
	for _, r := range recs {
		if r.Key == k {
			n++
		}
	}
	return n
}

// c2: Claim returns ErrConflict when a human changed the item after it was
// read, and changes nothing.
func c2(s *suite) {
	e := s.env()
	it := s.item(ports.MarkerReady, ports.MarkerTypeFeature)
	read := s.getItem(it)
	s.must(e.Driver.SetMarker(e.Human, it, ports.MarkerTypeBug, true))
	_, err := e.Tracker.Claim(s.ctx(), it, read.Version, s.key("claim"))
	if !errors.Is(err, ports.ErrConflict) {
		s.t.Fatalf("Claim after a human change: got %v, want ErrConflict", err)
	}
	now := s.getItem(it)
	if hasMarker(now.Markers, ports.MarkerInProgress) || !hasMarker(now.Markers, ports.MarkerReady) {
		s.t.Errorf("a refused Claim changed the markers: %v", now.Markers)
	}
	v, err := e.Tracker.Claim(s.ctx(), it, now.Version, s.key("claim"))
	s.must(err)
	after := s.getItem(it)
	if !hasMarker(after.Markers, ports.MarkerInProgress) || hasMarker(after.Markers, ports.MarkerReady) || after.Version != v {
		s.t.Errorf("Claim at the current version: markers %v, version %s (returned %s)", after.Markers, after.Version, v)
	}
}

// c3: FindRecords and ReviewRecords ignore records whose author isn't the
// allowed identity, even with a valid marker.
func c3(s *suite) {
	e := s.env()
	ctx := s.ctx()
	it := s.item(ports.MarkerReady)
	for _, forger := range []ports.Actor{e.Human, e.Owner, e.Coder, e.Reviewer} {
		r := rec(ports.RecordStatus, s.key("forged"))
		s.must(e.Driver.CommentItem(forger, it, "forged", &r))
	}
	k := s.key("real")
	_, err := e.Tracker.Comment(ctx, it, rec(ports.RecordStatus, k), ports.UntrustedText{}, k)
	s.must(err)
	recs, err := e.Tracker.FindRecords(ctx, it, ports.RecordStatus)
	s.must(err)
	if len(recs) != 1 || recs[0].Key != k || recs[0].Author.ID != e.Loop.ID {
		s.t.Errorf("FindRecords: got %d records %+v, want only the loop's", len(recs), recs)
	}
	if s.hostless() {
		return
	}

	ch, head := s.loopChange(it)
	for _, forger := range []ports.Actor{e.Human, e.Owner, e.Coder} {
		r := rec(ports.RecordReview, s.key("forged-review"))
		_, err := e.Driver.SubmitReview(forger, ch, ReviewComment, "forged review", &r)
		s.must(err)
	}
	k = s.key("review")
	_, err = e.Host.PostReview(ctx, ch, ports.ReviewPost{Commit: head, Record: rec(ports.RecordReview, k)}, k)
	s.must(err)
	rrs, err := e.Host.ReviewRecords(ctx, ch)
	s.must(err)
	if len(rrs) != 1 || rrs[0].Record.Key != k || rrs[0].Author.ID != e.Reviewer.ID {
		s.t.Errorf("ReviewRecords: got %d records, want only the reviewer's", len(rrs))
	}
}

// c4: Changes reports each label change with the right actor, including a
// non-owner human's.
func c4(s *suite) {
	e := s.env()
	it := s.item(ports.MarkerTypeFeature)
	_, cur := s.trackerFeed("")
	s.must(e.Driver.SetMarker(e.Owner, it, ports.MarkerReady, true))
	s.must(e.Driver.SetMarker(e.Human, it, ports.MarkerReady, false))
	s.must(e.Tracker.SetMarker(s.ctx(), it, ports.MarkerBlocked, true, s.key("m")))
	evs, _ := s.trackerFeed(cur)
	type want struct {
		actor ports.Actor
		m     ports.Marker
		on    bool
	}
	wants := []want{{e.Owner, ports.MarkerReady, true}, {e.Human, ports.MarkerReady, false}, {e.Loop, ports.MarkerBlocked, true}}
	var got []ports.MarkerChanged
	for _, ev := range evs {
		if mc, ok := ev.Detail.(ports.MarkerChanged); ok && ev.Item == it {
			got = append(got, mc)
		}
	}
	if len(got) != len(wants) {
		s.t.Fatalf("got %d marker events, want %d: %+v", len(got), len(wants), got)
	}
	for i, w := range wants {
		g := got[i]
		if g.Actor.ID != w.actor.ID || g.Actor.Kind != w.actor.Kind || g.Marker != w.m || g.On != w.on {
			s.t.Errorf("event %d = %+v, want %s by %s (%s) on=%v", i, g, w.m, w.actor.Login, w.actor.Kind, w.on)
		}
	}
}

// c5: closed-as-done and closed-not-done are told apart for items and
// dependencies.
func c5(s *suite) {
	e := s.env()
	ctx := s.ctx()
	byOwner, byHuman, notDone, merged, open := s.item(), s.item(), s.item(), s.item(), s.item()
	s.must(e.Driver.CloseItem(e.Owner, byOwner, ports.CloseCompleted))
	s.must(e.Driver.CloseItem(e.Human, byHuman, ports.CloseCompleted))
	s.must(e.Driver.CloseItem(e.Owner, notDone, ports.CloseNotDone))
	if !s.hostless() {
		ch, head := s.loopChange(merged)
		s.passGates(head)
		if out, err := e.Host.Merge(ctx, ch, head, ports.MergeMethodSquash, s.key("merge")); err != nil || out != ports.MergeMerged {
			s.t.Fatalf("Merge: %v, %v", out, err)
		}
	}

	type want struct {
		reason ports.CloseReason
		by     string // closer's ID, "" for none
		merged bool
	}
	wants := map[ports.ItemRef]want{
		byOwner: {ports.CloseCompleted, e.Owner.ID, false},
		byHuman: {ports.CloseCompleted, e.Human.ID, false},
		notDone: {ports.CloseNotDone, e.Owner.ID, false},
		merged:  {ports.CloseOpen, "", true},
		open:    {ports.CloseOpen, "", false},
	}
	if s.hostless() {
		delete(wants, merged) // needs the code host to merge its change
	}
	closer := func(a *ports.Actor) string {
		if a == nil {
			return ""
		}
		return a.ID
	}
	blocked := s.item()
	for ref, w := range wants {
		it := s.getItem(ref)
		if it.CloseReason != w.reason || closer(it.ClosedBy) != w.by {
			s.t.Errorf("item %s: %s by %q, want %s by %q", ref, it.CloseReason, closer(it.ClosedBy), w.reason, w.by)
		}
		s.must(e.Tracker.AddDependency(ctx, blocked, ref, s.key("dep")))
	}
	deps, err := e.Tracker.Dependencies(ctx, blocked)
	s.must(err)
	if len(deps) != len(wants) {
		s.t.Fatalf("got %d dependencies, want %d", len(deps), len(wants))
	}
	for _, d := range deps {
		w := wants[d.Ref]
		if d.CloseReason != w.reason || closer(d.ClosedBy) != w.by || d.ChangeMerged != w.merged {
			s.t.Errorf("dependency %s: %s by %q merged=%v, want %s by %q merged=%v",
				d.Ref, d.CloseReason, closer(d.ClosedBy), d.ChangeMerged, w.reason, w.by, w.merged)
		}
	}
}

// c12: Changes from a cursor never skips an event when the feed has more
// events than one page, also when events arrive between pages.
func c12(s *suite) {
	e := s.env()
	it := s.item()
	_, cur := s.trackerFeed("")
	n := 2*e.FeedPage + 3
	on := false
	toggle := func() {
		on = !on
		s.must(e.Driver.SetMarker(e.Owner, it, ports.MarkerPaused, on))
	}
	for i := 0; i < n; i++ {
		toggle()
	}
	// Read one page, then add more before reading on.
	first, next, err := e.Tracker.Changes(s.ctx(), cur)
	s.must(err)
	for i := 0; i < 2; i++ {
		toggle()
	}
	rest, _ := s.trackerFeed(next)
	all := append(first, rest...)
	if len(all) != n+2 {
		s.t.Fatalf("tracker feed: got %d events, want %d", len(all), n+2)
	}
	for i, ev := range all {
		mc, ok := ev.Detail.(ports.MarkerChanged)
		if !ok || mc.On != (i%2 == 0) {
			s.t.Fatalf("tracker event %d = %+v, want on=%v: an event was skipped or repeated", i, ev.Detail, i%2 == 0)
		}
	}
	if s.hostless() {
		return
	}

	ch, _ := s.loopChange(it)
	_, hcur := s.hostFeed("")
	var heads []ports.Commit
	for i := 0; i < n; i++ {
		h, err := e.Driver.Push(e.Owner, s.getChange(ch).Branch)
		s.must(err)
		heads = append(heads, h)
	}
	evs, _ := s.hostFeed(hcur)
	var got []ports.Commit
	for _, ev := range evs {
		if hm, ok := ev.Detail.(ports.HeadMoved); ok && ev.Change == ch {
			got = append(got, hm.Commit)
		}
	}
	if len(got) != len(heads) {
		s.t.Fatalf("host feed: got %d head moves, want %d", len(got), len(heads))
	}
	for i := range heads {
		if got[i] != heads[i] {
			s.t.Fatalf("host event %d = %s, want %s", i, got[i], heads[i])
		}
	}
}
