package workflows

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
	"github.com/mayusuk/agent-loop/internal/projection"
)

// PRODUCT-SPEC §11.1, the v0.1 acceptance scenario, against the fake
// adapter, scripted handlers and Temporal's test environment. Each step is
// its own project run (WIP 2) seeded with the items the step names (work
// plan #25 D1). Every run ends with the credential check of step 8.

// Step 1: A's clean round-1 review, then three owner comments hold the
// merge. An address run fixes the first (moving the head), explains the
// second and calls the third not valid, and the coder's resolution of the
// third thread is ignored. The manual-reply review, which is no round,
// rules the third open; the owner resolves it before the next merge
// check, and A merges with no follow-up.
func TestScenario11_1_Step1_ManualCommentsHoldTheMerge(t *testing.T) {
	s := newScenario(t)
	a := s.newItem("A")
	s.hold(a)
	var threads []ports.ThreadRef
	s.on(a, contract.StageImplement, s.impl())
	s.on(a, contract.StageReview, s.clean())
	var ref ports.ChangeRef
	s.at(10*time.Minute, func() {
		ch := s.change(a)
		ref = ch.Ref
		for _, body := range []string{"rename this", "why this way?", "split this file"} {
			tr, err := s.f.ReviewComment(fake.Owner, ch.Ref, "a.go", 1, body)
			s.must(err)
			threads = append(threads, tr)
		}
		s.release(a)
	})
	var manualIDs []string
	s.on(a, contract.StageAddress, s.addr(address{
		push: true,
		manual: func(i int, id string) contract.ReplyKind {
			manualIDs = append(manualIDs, id)
			return []contract.ReplyKind{contract.ReplyFixed, contract.ReplyExplained, contract.ReplyNotValid}[i]
		},
		during: func(in contract.Input) {
			// The handler tries to clear the third comment itself.
			s.must(s.f.ResolveThread(fake.Coder, ports.ChangeRef(in.Change.Ref), threads[2]))
		},
	}))
	var ruled []string
	s.on(a, contract.StageReview, s.rev(review{
		manual: map[int]contract.ManualRulingValue{2: contract.RulingOpen},
		before: func(in contract.Input) {
			ruled = in.Review.RuleManual
			if in.Review.Kind != contract.KindManualReply {
				t.Errorf("second review kind %s, want manual_reply", in.Review.Kind)
			}
			// The owner clears the third comment by resolving its thread
			// (unresolving the coder's resolution first), before the next
			// merge check.
			ref := ports.ChangeRef(in.Change.Ref)
			s.must(s.f.UnresolveThread(fake.Owner, ref, threads[2]))
			s.must(s.f.ResolveThread(fake.Owner, ref, threads[2]))
		},
	}))
	s.runProject()
	s.noCredentials()

	if got := s.itemStates(a); !slices.Equal(got, []core.State{core.StateImplementing, core.StateInReview, core.StateMerging, core.StateAddressing, core.StateInReview, core.StateMerging, core.StateMerged}) {
		t.Fatalf("A states %v", got)
	}
	if len(manualIDs) != 3 || !slices.Equal(ruled, manualIDs) {
		t.Fatalf("answered %v, manual-reply review ruled %v: the coder's resolution must not clear the third", manualIDs, ruled)
	}
	if got := s.reviewKinds(ref); !slices.Equal(got, []string{"round/1", "manual_reply"}) {
		t.Fatalf("A's reviews %v: the manual-reply review must not count as a round", got)
	}
	if got := s.stagesOf(a); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress, contract.StageReview}) {
		t.Fatalf("A's stages %v", got)
	}
	if len(s.posted.items) != 0 {
		t.Fatalf("follow-ups filed: %v", s.posted.refs)
	}
	if it := s.itemState(a); it.CloseReason != ports.CloseCompleted {
		t.Fatalf("A after merge: %+v", it)
	}
}

// Step 2: B's round-1 score-4 finding is addressed and round 2 leaves a
// score-2 finding. A merged first, so B's head lacks the base tip: the
// worker merges the base with no agent run (D41), a base-update review
// (no round) follows, and B merges. A not-ready follow-up lists the
// score-2 finding by ID, score, status and link only.
func TestScenario11_1_Step2_BaseUpdateAndFollowUp(t *testing.T) {
	s := newScenario(t)
	a, b := s.newItem("A"), s.newItem("B")
	s.hold(a)
	s.hold(b)
	s.on(a, contract.StageImplement, s.impl())
	s.on(a, contract.StageReview, s.clean())
	s.on(b, contract.StageImplement, s.impl())
	s.on(b, contract.StageReview, s.rev(review{add: []newF{{score: 4}}, before: func(contract.Input) { s.release(a) }}))
	s.on(b, contract.StageAddress, s.addr(address{push: true, finding: func(string) contract.ReplyKind { return contract.ReplyFixed }}))
	s.on(b, contract.StageReview,
		s.rev(review{findings: map[string]contract.FindingStatus{"F1": contract.StatusAddressed}, add: []newF{{score: 2}}}),
		s.rev(review{before: func(in contract.Input) {
			if in.Review.Kind != contract.KindBaseUpdate {
				t.Errorf("B's third review is %s, want base_update", in.Review.Kind)
			}
		}}))
	var ref ports.ChangeRef
	s.at(30*time.Minute, func() {
		if !slices.Contains(s.itemStates(a), core.StateMerged) {
			t.Errorf("A hasn't merged: %v", s.itemStates(a))
		}
		ref = s.change(b).Ref
		s.release(b)
	})
	s.runProject()
	s.noCredentials()

	if !slices.Contains(s.itemStates(b), core.StateMerged) {
		t.Fatalf("B states %v", s.itemStates(b))
	}
	if got := s.reviewKinds(ref); !slices.Equal(got, []string{"round/1", "round/2", "base_update"}) {
		t.Fatalf("B's reviews %v", got)
	}
	// The base update is one merge commit by the worker, with no handler.
	if got := s.stagesOf(b); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress, contract.StageReview, contract.StageReview}) {
		t.Fatalf("B's stages %v: the base update must run no handler", got)
	}
	rec := s.reviews(ref)
	merge, beforeBase := ports.Commit(rec[2].Commit), ports.Commit(rec[1].Commit)
	if ps, err := s.f.Parents(merge); err != nil || len(ps) != 2 || ps[0] != beforeBase {
		t.Fatalf("base-update head %s has parents %v (%v), want %s and the base tip", merge, ps, err, beforeBase)
	}
	if len(s.posted.items) != 1 {
		t.Fatalf("follow-ups %d, want 1", len(s.posted.items))
	}
	fu := s.itemState(s.posted.refs[0])
	if hasMarker(fu, ports.MarkerReady) || !hasMarker(fu, ports.MarkerFollowUp) {
		t.Fatalf("follow-up markers %v: want follow-up and not ready", fu.Markers)
	}
	body := untrusted.Read(s.posted.items[0].Body)
	if !strings.Contains(body, "| F2 | 2 | open |") || strings.Contains(body, "F1") || strings.Contains(body, "finding body") {
		t.Fatalf("follow-up body must list F2 by ID, score, status and link only:\n%s", body)
	}
}

// Step 3: a manual comment during C's round 3, which still has a score-5
// finding, escalates C (needs-owner, a notification, the comment listed)
// and C keeps its WIP slot. The owner pushes a fix and resumes; an address
// run answers both and a clean fourth review uses the one extra round. A
// later manual comment doesn't escalate: it is answered, ruled open,
// answered again and ruled addressed, and C merges. Neither manual-reply
// review counts as a round.
func TestScenario11_1_Step3_RoundLimitEscalationAndResume(t *testing.T) {
	s := newScenario(t)
	c := s.newItem("C")
	branch := branchOf(c)
	s.hold(c)
	var first, second ports.ThreadRef
	s.on(c, contract.StageImplement, s.impl())
	s.on(c, contract.StageReview,
		s.rev(review{add: []newF{{score: 5}}}),
		s.rev(review{}),
		s.rev(review{before: func(in contract.Input) {
			var err error
			first, err = s.f.ReviewComment(fake.Owner, ports.ChangeRef(in.Change.Ref), "a.go", 1, "this needs a test")
			s.must(err)
		}}),
		s.rev(review{findings: map[string]contract.FindingStatus{"F1": contract.StatusAddressed}}),
		s.rev(review{manual: map[int]contract.ManualRulingValue{0: contract.RulingOpen}}),
		s.rev(review{}),
	)
	var answered [][]string
	record := func(in contract.Input) {
		answered = append(answered, append(slices.Clone(in.Address.AnswerFindings), in.Address.AnswerManual...))
	}
	s.on(c, contract.StageAddress,
		s.addr(address{}), s.addr(address{}),
		s.addr(address{during: record}),
		s.addr(address{during: record}),
		s.addr(address{during: record}),
	)
	s.at(30*time.Minute, func() {
		if got := s.itemState(c); !hasMarker(got, ports.MarkerNeedsOwner) {
			t.Errorf("C not escalated after round 3: %v", got.Markers)
		}
		if !s.holdsSlot(c) {
			t.Error("escalated C doesn't hold its WIP slot")
		}
		_, err := s.f.Push(fake.Owner, branch)
		s.must(err)
		s.must(s.f.SetMarker(fake.Owner, c, ports.MarkerNeedsOwner, false))
	})
	s.at(2*time.Hour, func() {
		var err error
		second, err = s.f.ReviewComment(fake.Owner, s.change(c).Ref, "a.go", 2, "one more thing")
		s.must(err)
		s.release(c)
	})
	var ref ports.ChangeRef
	s.at(time.Hour, func() { ref = s.change(c).Ref })
	s.runProject()
	s.noCredentials()

	states := s.itemStates(c)
	if !slices.Contains(states, core.StateMerged) {
		t.Fatalf("C states %v", states)
	}
	escalations := 0
	for _, st := range states {
		if st == core.StateEscalated {
			escalations++
		}
	}
	if escalations != 1 {
		t.Fatalf("C escalated %d times, want once: %v", escalations, states)
	}
	if got := s.reviewKinds(ref); !slices.Equal(got, []string{"round/1", "round/2", "round/3", "round/4", "manual_reply", "manual_reply"}) {
		t.Fatalf("C's reviews %v", got)
	}
	if row := s.lastRow(c); row.Round != 4 || row.RoundsAllowed != 4 {
		t.Fatalf("C used %d of %d rounds, want 4 of 4", row.Round, row.RoundsAllowed)
	}
	mcs, err := s.f.Host().ManualComments(context.Background(), ref)
	s.must(err)
	ids := map[ports.ThreadRef]string{}
	for _, m := range mcs {
		ids[m.Thread] = m.ID
	}
	if !strings.Contains(s.posted.all(), ids[first]) {
		t.Errorf("the escalation comment doesn't list %s", ids[first])
	}
	want := [][]string{{"F1", ids[first]}, {ids[second]}, {ids[second]}}
	if fmt.Sprint(answered) != fmt.Sprint(want) {
		t.Fatalf("address runs answered %v, want %v", answered, want)
	}
	notified := false
	s.notes.mu.Lock()
	for _, n := range s.notes.got {
		notified = notified || (n.Item == c && n.Reason == core.ReasonRoundLimit)
	}
	s.notes.mu.Unlock()
	if !notified {
		t.Fatalf("no round-limit notification: %v", s.notes.reasons())
	}
}

// Step 4: the worker host is killed while B is in review, after the
// review was posted but before its result reached the workflow, and
// restarted. The rerun finds the posted review: no second review and no
// second handler run (NFR-1 AC1).
func TestScenario11_1_Step4_WorkerKilledDuringReview(t *testing.T) {
	s := newScenario(t)
	b := s.newItem("B")
	s.on(b, contract.StageImplement, s.impl())
	s.on(b, contract.StageReview, s.clean())
	killed := false
	var ref ports.ChangeRef
	s.runHook = func(ctx context.Context, req StageRequest) (StageReport, error) {
		rep, err := s.a.RunStage(ctx, req)
		if err == nil && req.Run.Stage == contract.StageReview && !killed {
			killed, ref = true, ports.ChangeRef(req.Run.Change)
			return StageReport{}, errKilled
		}
		return rep, err
	}
	s.runProject()
	s.noCredentials()

	if !killed {
		t.Fatal("the worker was never killed during the review")
	}
	if !slices.Contains(s.itemStates(b), core.StateMerged) {
		t.Fatalf("B states %v", s.itemStates(b))
	}
	if got := s.reviewKinds(ref); !slices.Equal(got, []string{"round/1"}) {
		t.Fatalf("B's reviews %v, want one", got)
	}
	if got := s.stagesOf(b); !slices.Equal(got, []contract.Stage{contract.StageImplement, contract.StageReview}) {
		t.Fatalf("B's handler runs %v: the review must run once", got)
	}
	if _, results := s.rec.stages(); !slices.Contains(results, contract.ReasonWorkerLost) {
		t.Fatalf("run results %v: the killed run isn't recorded as worker_lost", results)
	}
}

// Step 5: the owner pushes to B's branch between review and merge. The
// merge stops and an owner-push review follows, which doesn't use the
// round allowance: with one round allowed, B still merges.
func TestScenario11_1_Step5_OwnerPushBeforeMerge(t *testing.T) {
	s := newScenario(t)
	s.cfg.Item.Policy.MaxRounds = 1
	b := s.newItem("B")
	s.hold(b)
	s.on(b, contract.StageImplement, s.impl())
	s.on(b, contract.StageReview, s.clean(), s.clean())
	var ref ports.ChangeRef
	var pushed ports.Commit
	s.at(10*time.Minute, func() {
		ref = s.change(b).Ref
		var err error
		pushed, err = s.f.Push(fake.Owner, branchOf(b))
		s.must(err)
		s.release(b)
	})
	s.runProject()
	s.noCredentials()

	if !slices.Contains(s.itemStates(b), core.StateMerged) {
		t.Fatalf("B states %v", s.itemStates(b))
	}
	if got := s.reviewKinds(ref); !slices.Equal(got, []string{"round/1", "owner_push"}) {
		t.Fatalf("B's reviews %v", got)
	}
	if recs := s.reviews(ref); recs[1].Commit != string(pushed) {
		t.Fatalf("owner-push review of %s, want the owner's head %s", recs[1].Commit, pushed)
	}
	if row := s.lastRow(b); row.Round != 1 {
		t.Fatalf("B used %d rounds, want 1", row.Round)
	}
}

// outsidePush pushes a commit to the branch as an account that is neither
// the coder identity nor an owner account: on the fake, a human made a
// (non-owner) admin for the push, so the loop-branch ruleset lets it by
// (§11.1 step 6 on fake).
func (s *scen) outsidePush(branch string) ports.Commit {
	s.t.Helper()
	s.must(s.f.AddAdmin(fake.Human))
	c, err := s.f.Push(fake.Human, branch)
	s.must(err)
	s.must(s.f.RemoveAdmin(fake.Human))
	return c
}

// Step 6: an outside push makes D's commits untrusted. A label change by a
// human who isn't an owner does nothing. D is escalated, and not addressed
// or merged. The owner's resume approves those commits and D is reviewed
// again; a second outside push escalates D again.
func TestScenario11_1_Step6_UntrustedCommitEscalates(t *testing.T) {
	s := newScenario(t)
	d := s.newItem("D")
	s.hold(d)
	s.on(d, contract.StageImplement, s.impl())
	s.on(d, contract.StageReview, s.clean(), s.clean(), s.clean())
	var first ports.Commit
	var ref ports.ChangeRef
	escalated := func(when string) {
		if row := s.lastRow(d); row.State != core.StateEscalated {
			t.Errorf("%s: D is %s, want escalated (states %v)", when, row.State, s.itemStates(d))
		}
	}
	var stagesAtResume int
	s.at(10*time.Minute, func() {
		ref = s.change(d).Ref
		first = s.outsidePush(branchOf(d))
	})
	s.at(30*time.Minute, func() {
		escalated("after the outside push")
		if !hasMarker(s.itemState(d), ports.MarkerNeedsOwner) {
			t.Error("the escalation set no needs-owner label")
		}
		// A human who isn't an owner removes needs-owner: no effect.
		s.must(s.f.SetMarker(fake.Human, d, ports.MarkerNeedsOwner, false))
		stagesAtResume = len(s.stagesOf(d))
	})
	s.at(40*time.Minute, func() {
		escalated("after a non-owner removed needs-owner")
		if n := len(s.stagesOf(d)); n != stagesAtResume {
			t.Errorf("a stage ran after a non-owner's label change: %v", s.stagesOf(d))
		}
		// The owner resumes: the label is gone, so they set it and
		// remove it (work plan D6).
		s.must(s.f.SetMarker(fake.Owner, d, ports.MarkerNeedsOwner, true))
		s.must(s.f.SetMarker(fake.Owner, d, ports.MarkerNeedsOwner, false))
	})
	s.at(70*time.Minute, func() {
		if row := s.lastRow(d); row.State != core.StateMerging {
			t.Errorf("resumed D is %s, want merging (its gate is held)", row.State)
		}
		s.outsidePush(branchOf(d))
	})
	var states []core.State
	var stages []contract.Stage
	s.at(2*time.Hour, func() {
		escalated("after the second outside push")
		if !hasMarker(s.itemState(d), ports.MarkerNeedsOwner) {
			t.Error("the second escalation set no needs-owner label")
		}
		states, stages = s.itemStates(d), s.stagesOf(d)
		s.must(s.f.CloseItem(fake.Owner, d, ports.CloseNotDone)) // ends the run
	})
	s.runProject()
	s.noCredentials()

	if slices.Contains(states, core.StateMerged) || slices.Contains(stages, contract.StageAddress) {
		t.Fatalf("an untrusted D was addressed or merged: states %v, stages %v", states, stages)
	}
	n := 0
	for _, st := range states {
		if st == core.StateEscalated {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("D escalated %d times, want 2: %v", n, states)
	}
	// The resume approved the first push's commits, and D was reviewed
	// on that head.
	reviewed := false
	for _, r := range s.reviews(ref) {
		reviewed = reviewed || r.Commit == string(first)
	}
	if !reviewed {
		t.Fatalf("D wasn't reviewed again on the approved head %s: %v", first, s.reviewKinds(ref))
	}
	approved := false
	for _, r := range statusRecords(t, s.harness, d) {
		approved = approved || slices.Contains(r.Commits, string(first))
	}
	if !approved {
		t.Fatalf("no record approves %s", first)
	}
}

func (s *scen) changeMarked(ref ports.ChangeRef, m ports.Marker) bool {
	s.t.Helper()
	ch, err := s.f.Host().Change(context.Background(), ref)
	s.must(err)
	return slices.Contains(ch.Markers, m)
}

func (s *scen) claimed(item ports.ItemRef) bool {
	return hasMarker(s.itemState(item), ports.MarkerInProgress)
}

// Step 7: the owner aborts D. D and its change get aborted, the change
// stays open, and D keeps its WIP slot (so a third ready item waits) until
// the owner closes D, which removes aborted from the change.
func TestScenario11_1_Step7_AbortKeepsTheSlotUntilClosed(t *testing.T) {
	s := newScenario(t)
	d, e := s.newItem("D"), s.newItem("E")
	s.hold(d)
	s.hold(e)
	for _, it := range []ports.ItemRef{d, e} {
		s.on(it, contract.StageImplement, s.impl())
		s.on(it, contract.StageReview, s.clean())
	}
	var f ports.ItemRef
	var ref ports.ChangeRef
	s.at(10*time.Minute, func() {
		ref = s.change(d).Ref
		s.must(s.f.SetMarker(fake.Owner, d, ports.MarkerAborted, true))
		f = s.newItem("F")
		s.on(f, contract.StageImplement, s.impl())
		s.on(f, contract.StageReview, s.clean())
	})
	s.at(40*time.Minute, func() {
		if row := s.lastRow(d); row.State != core.StateAborted {
			t.Errorf("D is %s, want aborted", row.State)
		}
		if !s.changeMarked(ref, ports.MarkerAborted) {
			t.Error("D's change isn't marked aborted")
		}
		if ch, _ := s.f.Host().Change(context.Background(), ref); ch.State != ports.ChangeStateOpen {
			t.Errorf("D's change is %s, want open", ch.State)
		}
		if !s.holdsSlot(d) {
			t.Error("aborted D doesn't hold its WIP slot")
		}
		if s.claimed(f) {
			t.Error("F was claimed while aborted D held a slot")
		}
		s.must(s.f.CloseItem(fake.Owner, d, ports.CloseNotDone))
	})
	// F merges first, so E's head lacks the base tip: a base-update
	// review follows.
	s.on(e, contract.StageReview, s.clean())
	s.at(50*time.Minute, func() { s.release(e) })
	s.runProject()
	s.noCredentials()

	if s.changeMarked(ref, ports.MarkerAborted) {
		t.Fatal("closing D left aborted on its change")
	}
	if ch, _ := s.f.Host().Change(context.Background(), ref); ch.State != ports.ChangeStateOpen {
		t.Fatalf("D's change is %s; closing the item must not close it", ch.State)
	}
	for _, it := range []ports.ItemRef{e, f} {
		if !slices.Contains(s.itemStates(it), core.StateMerged) {
			t.Fatalf("item %s states %v", it, s.itemStates(it))
		}
	}
}

// boardShows checks that every item the project runs is on the board with
// its state, as its last decision left it, and a next action or a wait
// reason (FR-11 AC1).
func (s *scen) boardShows(when string, want map[ports.ItemRef]core.State) {
	s.t.Helper()
	rows := map[string]projection.BoardRow{}
	for _, r := range s.board() {
		rows[r.Item] = r
	}
	running := s.project().State.Items
	if len(rows) != len(want) || len(running) != len(want) {
		s.t.Errorf("%s: board %v, running %v, want %v", when, rows, running, want)
	}
	for item, st := range want {
		r, ok := rows[string(item)]
		switch {
		case !ok:
			s.t.Errorf("%s: %s isn't on the board", when, item)
		case r.State != st:
			s.t.Errorf("%s: the board shows %s %s, want %s", when, item, r.State, st)
		case r.NextAction == "" && r.WaitReason == "":
			s.t.Errorf("%s: the board shows no next action or wait for %s (%s)", when, item, r.State)
		}
	}
}

// Step 8: throughout, the board shows each item's state and next action,
// and no credential appears in any log. Every step's run ends with the
// credential check (scen.noCredentials); here the board is read at each
// turn of a run with an item that merges and one that escalates.
func TestScenario11_1_Step8_BoardAndNoCredentials(t *testing.T) {
	s := newScenario(t)
	a, b := s.newItem("A"), s.newItem("B")
	s.hold(a)
	s.on(a, contract.StageImplement, s.impl())
	s.on(a, contract.StageReview, s.clean())
	s.on(b, contract.StageImplement, func(h *harness, in contract.Input, out string) string {
		write(h.t, out, "q.md", "Which API? "+s.leak(h))
		return resultJSON(in, "needs_owner", map[string]any{"question_file": "q.md"})
	})
	s.at(10*time.Minute, func() {
		s.boardShows("A waiting on its gate, B escalated", map[ports.ItemRef]core.State{a: core.StateMerging, b: core.StateEscalated})
		s.release(a)
	})
	s.at(30*time.Minute, func() {
		s.boardShows("after A merged", map[ports.ItemRef]core.State{b: core.StateEscalated})
		s.must(s.f.CloseItem(fake.Owner, b, ports.CloseNotDone))
	})
	s.at(40*time.Minute, func() { s.boardShows("after B closed", map[ports.ItemRef]core.State{}) })
	s.runProject()
	s.noCredentials()
	if !strings.Contains(s.posted.all(), "Which API?") {
		t.Fatal("the escalation didn't post the handler's question, so the check saw no handler text")
	}
}

// Step 9: the project reaches its daily limit (4 agent runs here: both
// implements and both round reviews, so Y's base-update review waits). No new
// agent stage starts and nothing new is picked; the stage already running
// finishes; after 00:00 the waiting item and the unpicked one continue on
// their own (FR-16 AC3, AC4).
func TestScenario11_1_Step9_DailyLimitHoldsUntilTheDayBoundary(t *testing.T) {
	s := newScenario(t)
	s.cfg.DailyRuns = 4
	s.cfg.SupervisorInterval = 20 * time.Hour // past 00:00 from 09:00
	s.cfg.PollInterval = 30 * time.Minute
	start := s.env.Now()
	midnight := core.NextDayBoundary(start, time.UTC)
	x, y := s.newItem("X"), s.newItem("Y")
	for _, it := range []ports.ItemRef{x, y} {
		s.on(it, contract.StageImplement, s.impl())
		s.on(it, contract.StageReview, s.clean(), s.clean())
	}
	var z ports.ItemRef
	s.at(5*time.Minute, func() {
		z = s.newItem("Z")
		s.on(z, contract.StageImplement, s.impl())
		s.on(z, contract.StageReview, s.clean())
	})
	s.at(midnight.Sub(start)-time.Minute, func() {
		if !s.project().DailyLimitReached {
			t.Error("the daily limit isn't reached before 00:00")
		}
		if s.claimed(z) {
			t.Error("Z was picked at the daily limit")
		}
		waiting := 0
		for _, it := range []ports.ItemRef{x, y} {
			if s.lastRow(it).State == core.StateWaitingLimit {
				waiting++
			}
		}
		if waiting != 1 {
			t.Errorf("%d items wait for the limit, want 1", waiting)
		}
	})
	s.runProject()
	s.noCredentials()

	var before, after []string
	s.rec.mu.Lock()
	for _, r := range s.rec.starts {
		run := r.Item + ":" + string(r.Stage)
		if r.Started.Before(midnight) {
			before = append(before, run)
		} else {
			after = append(after, run)
		}
	}
	s.rec.mu.Unlock()
	if len(before) != 4 {
		t.Fatalf("agent runs before 00:00: %v, want 4", before)
	}
	for _, it := range []ports.ItemRef{x, y, z} {
		if !slices.Contains(s.itemStates(it), core.StateMerged) {
			t.Fatalf("item %s states %v (runs before 00:00 %v, after %v)", it, s.itemStates(it), before, after)
		}
	}
	if !slices.Contains(after, string(z)+":implement") {
		t.Fatalf("Z wasn't implemented after 00:00: %v", after)
	}
	rs := s.notes.reasons()
	if !slices.Contains(rs, "daily_limit") || !slices.Contains(rs, "daily_reset") {
		t.Fatalf("notifications %v", rs)
	}
}
