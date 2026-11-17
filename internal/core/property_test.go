package core

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

func equalStates(a, b ItemState) bool { return reflect.DeepEqual(a, b) }

// randomEvent picks an event that is plausible for s, mixing in noise
// (unknown run IDs, events for other states).
func randomEvent(r *rand.Rand, s ItemState, i int) Event {
	at := At{t0.Add(time.Duration(i) * time.Minute)}
	id := "noise"
	if s.Run != nil && r.Intn(10) > 0 {
		id = s.Run.RunID
	}
	stage := contract.StageImplement
	if s.Run != nil {
		stage = s.Run.Stage
	}
	head := []string{"h1", "h2", "h3", "h4"}[r.Intn(4)]
	findings := func() []Finding {
		var fs []Finding
		for j := range r.Intn(3) {
			st := []contract.FindingStatus{contract.StatusOpen, contract.StatusAddressed, contract.StatusPartiallyAddressed, contract.StatusWithdrawn}[r.Intn(4)]
			fs = append(fs, Finding{ID: []string{"F1", "F2", "F3"}[j], Score: 1 + r.Intn(5), Status: st})
		}
		return fs
	}
	trustReport := func() TrustReport {
		if r.Intn(4) > 0 {
			return TrustReport{}
		}
		return TrustReport{Causes: []string{[]string{CauseFork, CausePusherUnknown, CauseRulesetChanged}[r.Intn(3)]}, Commits: []string{head}}
	}
	if r.Intn(4) == 0 {
		return ownerEvent(r, at, head)
	}
	switch r.Intn(17) {
	case 0:
		return Started{At: at, WorkflowID: "item/p/1", Item: "1", Claim: 1}
	case 1, 2, 3:
		res := StageResult{At: at, RunID: id, Stage: stage, Outcome: contract.OutcomeDone}
		switch stage {
		case contract.StageImplement:
			res.Implement = &ImplementOutcome{Change: "57", Head: head}
		case contract.StageReview:
			commit := head
			if s.Change != nil && r.Intn(3) > 0 {
				commit = s.Change.Head
			}
			kind := contract.KindRound
			if s.Run != nil && s.Run.Review != nil {
				kind = s.Run.Review.Kind
			}
			res.Review = &ReviewOutcome{Commit: commit, Kind: kind, Findings: findings(),
				ManualRulings: []ManualRuling{{Comment: "MC-1", Ruling: contract.RulingAddressed}}}
		case contract.StageAddress:
			res.Address = &AddressOutcome{Head: head, Replies: []ReplyOutcome{{To: "F1", Counts: r.Intn(2) == 0}},
				ManualReplies: []ReplyOutcome{{To: "MC-1", Counts: true}}}
		}
		return res
	case 4:
		o := []contract.Outcome{contract.OutcomeFailed, contract.OutcomeNeedsOwner, contract.OutcomeBlocked, contract.OutcomeComplete}[r.Intn(4)]
		return StageResult{At: at, RunID: id, Stage: stage, Outcome: o,
			Blocked: &BlockedOutcome{Dependency: "61", State: []DependencyState{DependencyOpen, DependencyDone, DependencyNotDone}[r.Intn(3)]}}
	case 5:
		return StageCrashed{At: at, RunID: id}
	case 6:
		return StageTimedOut{At: at, RunID: id}
	case 7:
		return DailyLimitReached{At: at, RunID: id}
	case 8:
		return HeadMoved{At: at, Commit: head, Pusher: []Pusher{PusherCoder, PusherOwner, PusherUnknown}[r.Intn(3)]}
	case 9:
		return ManualCommentChanged{At: at, Comment: "MC-1", HumanCleared: r.Intn(2) == 0, Reopened: r.Intn(2) == 0}
	case 10:
		return Resumed{At: at}
	case 11:
		return DependencyClosed{At: at, Dependency: "61", Done: r.Intn(2) == 0}
	case 12:
		return BlockRemoved{At: at}
	case 13:
		return TrustRefused{At: at, RunID: id, Report: trustReport()}
	case 14:
		checkHead := head
		if s.Change != nil && r.Intn(3) > 0 {
			checkHead = s.Change.Head
		}
		gates := []Gate{{Name: "test", State: []GateState{GatePass, GatePass, GateFail, GatePending, GateMissing}[r.Intn(5)]}}
		return MergeChecked{At: at, Head: checkHead, Pusher: PusherCoder, Trust: trustReport(), ContainsBase: r.Intn(4) > 0, Gates: gates}
	case 15:
		return MergeResult{At: at, Outcome: []MergeOutcome{MergeMerged, MergeHeadMoved, MergeNotMergeable, MergeFailed}[r.Intn(4)]}
	}
	return DayBoundary{At: at}
}

// ownerEvent picks an owner or manual action (work plan #12).
func ownerEvent(r *rand.Rand, at At, head string) Event {
	by := []Role{RoleOwner, RoleOwner, RoleOther, RoleLoop}[r.Intn(4)]
	switch r.Intn(9) {
	case 0, 1:
		m := []string{MarkerPaused, MarkerAborted, MarkerInProgress, MarkerNeedsOwner, MarkerBlocked, MarkerReady}[r.Intn(6)]
		return MarkerChanged{At: at, By: by, Marker: m, Added: r.Intn(2) == 0, OnChange: r.Intn(5) == 0}
	case 2, 3:
		return OwnerCommand{At: at, Command: []Command{CommandPause, CommandResume, CommandAbort}[r.Intn(3)]}
	case 4:
		return Assigned{At: at, By: by}
	case 5:
		return ItemClosed{At: at, By: by}
	case 6:
		return ChangeClosed{At: at, By: by, Change: "57"}
	case 7:
		return ChangeMerged{At: at, By: by, Change: "57"}
	}
	return ChangeReopened{At: at, Change: Change{Ref: "57", Head: head}}
}

func TestTECH4_DecideIsDeterministic(t *testing.T) {
	for seed := int64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		p := Policy{MaxRounds: 1 + r.Intn(3)}
		var events []Event
		s := ItemState{}
		for i := range 60 {
			e := randomEvent(r, s, i)
			events = append(events, e)
			s, _ = Decide(s, e, p)
		}
		replay := func() (ItemState, [][]Action) {
			var st ItemState
			var all [][]Action
			for _, e := range events {
				var acts []Action
				st, acts = Decide(st, e, p)
				all = append(all, acts)
			}
			return st, all
		}
		s1, a1 := replay()
		s2, a2 := replay()
		if !equalStates(s1, s2) || !reflect.DeepEqual(a1, a2) || !equalStates(s1, s) {
			t.Fatalf("seed %d: replays differ", seed)
		}
		for i, acts := range a1 {
			if _, ok := acts[len(acts)-1].(RecordDecision); !ok {
				t.Fatalf("seed %d event %d: no decision record", seed, i)
			}
		}
	}
}
