package core

import (
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// harness drives Decide one event at a time, keeping the clock moving.
type harness struct {
	t    *testing.T
	p    Policy
	s    ItemState
	acts []Action
	n    int
}

func newHarness(t *testing.T, p Policy) *harness {
	t.Helper()
	return &harness{t: t, p: p}
}

func (h *harness) at() At { h.n++; return At{t0.Add(time.Duration(h.n) * time.Minute)} }

func (h *harness) do(e Event) *harness {
	h.t.Helper()
	before := h.s.Clone()
	h.s, h.acts = Decide(h.s, e, h.p)
	if len(h.acts) == 0 {
		h.t.Fatalf("%s: no actions; the decision record is missing", e.Name())
	}
	if _, ok := h.acts[len(h.acts)-1].(RecordDecision); !ok {
		h.t.Fatalf("%s: last action is %T, want RecordDecision", e.Name(), h.acts[len(h.acts)-1])
	}
	if rec := h.record(); rec.From != before.State || rec.To != h.s.State || rec.Event != e.Name() {
		h.t.Fatalf("%s: record %+v doesn't match %s -> %s", e.Name(), rec, before.State, h.s.State)
	}
	return h
}

func (h *harness) record() DecisionRecord { return h.acts[len(h.acts)-1].(RecordDecision).Record }

// start claims the item, with an optional existing change.
func (h *harness) start(ch *Change, reviews ...ReviewRecord) *harness {
	h.t.Helper()
	return h.do(Started{At: h.at(), WorkflowID: "item/p/42", Item: "42", Claim: 1, Change: ch, Reviews: reviews})
}

func (h *harness) runID() string {
	h.t.Helper()
	if h.s.Run == nil {
		h.t.Fatalf("no run in state %s", h.s.State)
	}
	return h.s.Run.RunID
}

func (h *harness) result(o contract.Outcome) *harness {
	h.t.Helper()
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: h.s.Run.Stage, Outcome: o, Reason: "x"})
}

func (h *harness) implemented(head string) *harness {
	h.t.Helper()
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageImplement, Outcome: contract.OutcomeDone,
		Implement: &ImplementOutcome{Change: "57", Head: head}})
}

// reviewed completes the running review on its commit with these findings
// (every finding on the change).
func (h *harness) reviewed(fs []Finding, rulings ...ManualRuling) *harness {
	h.t.Helper()
	rv := h.s.Run.Review
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageReview, Outcome: contract.OutcomeDone,
		Review: &ReviewOutcome{Commit: rv.Commit, Kind: rv.Kind, Findings: fs, ManualRulings: rulings}})
}

// addressed completes the running address run, replying to every answer
// ID with a counting reply.
func (h *harness) addressed(head string) *harness {
	h.t.Helper()
	a := h.s.Run.Address
	o := &AddressOutcome{Head: head}
	for _, id := range a.AnswerFindings {
		o.Replies = append(o.Replies, ReplyOutcome{To: id, Kind: contract.ReplyFixed, Counts: true})
	}
	for _, id := range a.AnswerManual {
		o.ManualReplies = append(o.ManualReplies, ReplyOutcome{To: id, Kind: contract.ReplyExplained, Counts: true})
	}
	return h.addressedWith(o)
}

func (h *harness) addressedWith(o *AddressOutcome) *harness {
	h.t.Helper()
	return h.do(StageResult{At: h.at(), RunID: h.runID(), Stage: contract.StageAddress, Outcome: contract.OutcomeDone, Address: o})
}

func (h *harness) state(want State) *harness {
	h.t.Helper()
	if h.s.State != want {
		h.t.Fatalf("state = %s, want %s (rule %q)", h.s.State, want, h.record().Rule)
	}
	return h
}

// runStage returns the decision's RunStage action, or fails.
func (h *harness) runStage() RunStage {
	h.t.Helper()
	for _, a := range h.acts {
		if r, ok := a.(RunStage); ok {
			return r
		}
	}
	h.t.Fatalf("no RunStage in %v", h.record().Actions)
	return RunStage{}
}

func (h *harness) noRunStage() {
	h.t.Helper()
	for _, a := range h.acts {
		if _, ok := a.(RunStage); ok {
			h.t.Fatalf("unexpected RunStage %+v", a)
		}
	}
}

func (h *harness) escalation(reason string) PostStatus {
	h.t.Helper()
	h.state(StateEscalated)
	if h.s.Escalation == nil || h.s.Escalation.Reason != reason {
		h.t.Fatalf("escalation = %+v, want reason %s", h.s.Escalation, reason)
	}
	var ps *PostStatus
	var marker, notify bool
	for _, a := range h.acts {
		switch a := a.(type) {
		case PostStatus:
			ps = &a
		case SetMarker:
			marker = marker || (a.Marker == MarkerNeedsOwner && a.On)
		case Notify:
			notify = true
		}
	}
	if ps == nil || ps.Event != contract.EventEscalate || ps.Reason != reason || !marker || !notify {
		h.t.Fatalf("escalation actions incomplete: %v", h.record().Actions)
	}
	return *ps
}

func (h *harness) reviewKind(want contract.ReviewKind) *harness {
	h.t.Helper()
	h.state(StateInReview)
	if got := h.runStage().Review.Kind; got != want {
		h.t.Fatalf("review kind = %s, want %s", got, want)
	}
	return h
}

func (h *harness) addressMode(want contract.AddressMode) *harness {
	h.t.Helper()
	h.state(StateAddressing)
	if got := h.runStage().Address.Mode; got != want {
		h.t.Fatalf("address mode = %s, want %s", got, want)
	}
	return h
}

func f(id string, score int, st contract.FindingStatus) Finding {
	return Finding{ID: id, Score: score, Status: st}
}

var (
	open4  = f("F1", 4, contract.StatusOpen)
	fixed4 = f("F1", 4, contract.StatusAddressed)
	open2  = f("F2", 2, contract.StatusOpen)
	open5  = f("F3", 5, contract.StatusOpen)
)

// inReview returns a harness with an implemented item whose round-1 review
// is running.
func inReview(t *testing.T, p Policy) *harness {
	t.Helper()
	return newHarness(t, p).start(nil).implemented("h1").reviewKind(contract.KindRound)
}
