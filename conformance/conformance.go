package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// ChangedFile is the file every Driver.Push changes, so a finding can point
// at a path and line that are in a change's diff (INTEGRATION-SPEC §5.2).
const ChangedFile = "conformance.txt"

// Review states for Driver.SubmitReview, as the code host names them.
type ReviewState = string

const (
	ReviewApprove          ReviewState = "APPROVE"
	ReviewComment          ReviewState = "COMMENT"
	ReviewChangesRequested ReviewState = "CHANGES_REQUESTED"
)

// The rulesets of INTEGRATION-SPEC §9.8 step 1, and what a test can do to
// them.
type (
	Ruleset       = string
	RulesetAction = string
)

const (
	RulesetLoopBranches Ruleset = "loop_branches"
	RulesetBaseBranch   Ruleset = "base_branch"
	RulesetTags         Ruleset = "tags"

	RulesetChange  RulesetAction = "change"  // edit it; it stays active
	RulesetDisable RulesetAction = "disable" // keep it, not enforced
	RulesetEnable  RulesetAction = "enable"
	RulesetRemove  RulesetAction = "remove"
	RulesetRestore RulesetAction = "restore" // create it again
)

// Driver acts in the sandbox project as a given account, the way a person or
// app would on the real tracker and code host. An action the account isn't
// allowed to take returns an error that is ports.ErrPermission.
type Driver interface {
	// Tracker.
	NewItem(as ports.Actor, title, body string, markers ...ports.Marker) (ports.ItemRef, error)
	SetMarker(as ports.Actor, item ports.ItemRef, m ports.Marker, on bool) error
	Assign(as ports.Actor, item ports.ItemRef, assignee *ports.Actor) error // nil clears
	// CommentItem comments on the item. rec, if set, is a record marker in
	// the comment (to forge records, C3).
	CommentItem(as ports.Actor, item ports.ItemRef, body string, rec *ports.Record) error
	CloseItem(as ports.Actor, item ports.ItemRef, reason ports.CloseReason) error
	ReopenItem(as ports.Actor, item ports.ItemRef) error

	// Code host. A branch named "<login>:<name>" is in that account's fork.
	// Push pushes one new commit that changes ChangedFile, creating the
	// branch from the base tip if it doesn't exist.
	Push(as ports.Actor, branch string) (ports.Commit, error)
	MergeBaseInto(as ports.Actor, branch string) (ports.Commit, error) // pushes a merge of the base tip
	// OpenChange opens a change from branch. A non-empty item is the code
	// host's closing reference (INTEGRATION-SPEC §6.2).
	OpenChange(as ports.Actor, item ports.ItemRef, branch string) (ports.ChangeRef, error)
	CommentChange(as ports.Actor, change ports.ChangeRef, body string) error // a conversation comment
	// ReviewComment starts a review thread on path and line (0: file level).
	ReviewComment(as ports.Actor, change ports.ChangeRef, path string, line int, body string) (ports.ThreadRef, error)
	ReplyInThread(as ports.Actor, change ports.ChangeRef, thread ports.ThreadRef, body string) error
	ResolveThread(as ports.Actor, change ports.ChangeRef, thread ports.ThreadRef) error
	UnresolveThread(as ports.Actor, change ports.ChangeRef, thread ports.ThreadRef) error
	SubmitReview(as ports.Actor, change ports.ChangeRef, state ReviewState, body string, rec *ports.Record) (ports.ReviewRef, error)
	DismissReview(as ports.Actor, change ports.ChangeRef, review ports.ReviewRef) error
	MergeChange(as ports.Actor, change ports.ChangeRef) error
	CloseChange(as ports.Actor, change ports.ChangeRef) error
	ReopenChange(as ports.Actor, change ports.ChangeRef) error
	CreateTag(as ports.Actor, name string) error
	// Replies returns the records in a thread's comments, or in the change's
	// conversation comments when thread is ThreadRef(change) (D3), each with
	// its Author. It only reads.
	Replies(change ports.ChangeRef, thread ports.ThreadRef) ([]ports.Record, error)

	// Repository settings, as the owner.
	SetGate(c ports.Commit, name string, s ports.GateState) error
	RequireApprovals(n int) error
	SetRuleset(r Ruleset, a RulesetAction) error
	AddAdmin(a ports.Actor) error
	RemoveAdmin(a ports.Actor) error

	// StartCoderRun records a coder run for the item in the worker's run
	// records, holding a live coder push token until end is called
	// (INTEGRATION-SPEC §9.8 step 6). The worker's base merge is one too.
	StartCoderRun(item ports.ItemRef) (end func(), err error)
}

// Env is one fresh sandbox project.
type Env struct {
	Tracker ports.WorkTracker
	Host    ports.CodeHost
	Driver  Driver

	Owner    ports.Actor // an owner account, admin of the repository
	Human    ports.Actor // another human with write access
	Bot      ports.Actor // a bot listed in manual_comment_bots
	Coder    ports.Actor
	Reviewer ports.Actor
	Loop     ports.Actor

	BaseBranch   string
	BranchPrefix string
	Gates        []string // the required gates, at least one
	FeedPage     int      // the most events one Changes call returns
}

// Factory returns a fresh Env for each case.
type Factory func(t *testing.T) Env

// Run runs every conformance case, C1 to C16 of INTEGRATION-SPEC §11, each
// as a subtest on a fresh Env. The Env needs both ports.
func Run(t *testing.T, f Factory) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &suite{t: t, f: f}
			if s.env().Host == nil {
				t.Fatal("conformance: Run needs Env.Host; use RunTracker for a tracker alone")
			}
			c.run(s)
		})
	}
}

// trackerCases are the cases a WorkTracker alone can run (work plan #14
// D13).
var trackerCases = map[string]bool{
	"TestC1_RepeatedWritesLeaveOneResult":      true,
	"TestC2_ClaimConflictsAfterHumanChange":    true,
	"TestC3_RecordsCountOnlyFromAllowedAuthor": true,
	"TestC4_ChangesReportLabelActors":          true,
	"TestC5_ClosedDoneAndNotDoneAreDistinct":   true,
	"TestC11_HumanTextOnlyAsUntrustedText":     true,
	"TestC12_ChangesNeverSkipAcrossPages":      true,
}

// RunTracker runs the tracker cases for a WorkTracker with no CodeHost yet
// (Env.Host nil): C1 to C5, C11 and C12, without their code-host parts.
func RunTracker(t *testing.T, f Factory) {
	for _, c := range cases {
		if !trackerCases[c.name] {
			continue
		}
		t.Run(c.name, func(t *testing.T) { c.run(&suite{t: t, f: f}) })
	}
}

// trustCases need Trust and pusher attribution (GitHub work plan #15
// D17).
var trustCases = map[string]bool{
	"TestC10_TrustReportsEachCause":               true,
	"TestC13_CoderTokenRefusedOnBaseMergeAndTags": true,
	"TestC15_PusherAttribution":                   true,
}

// RunWithoutTrust runs every case except C10, C13 and C15, for a code host
// whose trust checks come later. The Env needs both ports.
func RunWithoutTrust(t *testing.T, f Factory) {
	for _, c := range cases {
		if trustCases[c.name] {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			s := &suite{t: t, f: f}
			if s.env().Host == nil {
				t.Fatal("conformance: RunWithoutTrust needs Env.Host")
			}
			c.run(s)
		})
	}
}

// hostless reports whether the Env has no CodeHost (RunTracker).
func (s *suite) hostless() bool { return s.env().Host == nil }

var cases = []struct {
	name string
	run  func(*suite)
}{
	{"TestC1_RepeatedWritesLeaveOneResult", c1},
	{"TestC2_ClaimConflictsAfterHumanChange", c2},
	{"TestC3_RecordsCountOnlyFromAllowedAuthor", c3},
	{"TestC4_ChangesReportLabelActors", c4},
	{"TestC5_ClosedDoneAndNotDoneAreDistinct", c5},
	{"TestC6_ManualCommentClearing", c6},
	{"TestC7_MergeWithOldPinIsHeadMoved", c7},
	{"TestC8_ContainsBase", c8},
	{"TestC9_GatesForHeadCommitOnly", c9},
	{"TestC10_TrustReportsEachCause", c10},
	{"TestC11_HumanTextOnlyAsUntrustedText", c11},
	{"TestC12_ChangesNeverSkipAcrossPages", c12},
	{"TestC13_CoderTokenRefusedOnBaseMergeAndTags", c13},
	{"TestC14_ReviewerApprovalSatisfiesRequiredApproval", c14},
	{"TestC15_PusherAttribution", c15},
	{"TestC16_ManualCommentsInFindingThreadsAndEmptyReviews", c16},
}

// suite is one case's state.
type suite struct {
	t   *testing.T
	f   Factory
	e   Env
	set bool
	n   int
}

func (s *suite) env() *Env {
	if !s.set {
		s.e, s.set = s.f(s.t), true
		if len(s.e.Gates) == 0 || s.e.FeedPage <= 0 {
			s.t.Fatal("conformance: Env needs at least one gate and a FeedPage")
		}
	}
	return &s.e
}

// fresh runs fn as a subtest on its own Env.
func (s *suite) fresh(name string, fn func(*suite)) {
	s.t.Run(name, func(t *testing.T) { fn(&suite{t: t, f: s.f}) })
}

func (s *suite) ctx() context.Context { return context.Background() }

func (s *suite) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *suite) refused(err error, what string) {
	s.t.Helper()
	if !errors.Is(err, ports.ErrPermission) {
		s.t.Errorf("%s: got %v, want ErrPermission", what, err)
	}
}

// key returns a fresh idempotency key.
func (s *suite) key(what string) ports.IdemKey {
	s.n++
	return ports.IdemKey("conformance/" + s.t.Name() + ":" + what + ":" + strconv.Itoa(s.n))
}

// rec returns a record of type rt with key.
func rec(rt ports.RecordType, key ports.IdemKey) ports.Record {
	data, _ := json.Marshal(map[string]any{"type": rt, "v": 1, "key": key})
	return ports.Record{Type: rt, Key: key, Data: data}
}

func (s *suite) item(markers ...ports.Marker) ports.ItemRef {
	s.t.Helper()
	ref, err := s.env().Driver.NewItem(s.env().Owner, "conformance item", "conformance body", markers...)
	s.must(err)
	return ref
}

func (s *suite) getItem(ref ports.ItemRef) ports.Item {
	s.t.Helper()
	it, err := s.env().Tracker.Item(s.ctx(), ref)
	s.must(err)
	return it
}

func (s *suite) getChange(ref ports.ChangeRef) ports.Change {
	s.t.Helper()
	c, err := s.env().Host.Change(s.ctx(), ref)
	s.must(err)
	return c
}

// coderRun runs fn during a coder run for item.
func (s *suite) coderRun(item ports.ItemRef, fn func()) {
	s.t.Helper()
	end, err := s.env().Driver.StartCoderRun(item)
	s.must(err)
	defer end()
	fn()
}

// loopChange pushes the item's branch as the coder and opens its change as
// the worker does. It returns the change and its head.
func (s *suite) loopChange(item ports.ItemRef) (ports.ChangeRef, ports.Commit) {
	s.t.Helper()
	e := s.env()
	branch := e.BranchPrefix + string(item)
	var head ports.Commit
	s.coderRun(item, func() {
		var err error
		head, err = e.Driver.Push(e.Coder, branch)
		s.must(err)
	})
	k := s.key("open")
	ref, err := e.Host.OpenChange(s.ctx(), ports.NewChange{
		Item: item, Branch: branch, Base: e.BaseBranch, Record: rec(ports.RecordChange, k),
	}, ports.IdentityCoder, k)
	s.must(err)
	return ref, head
}

// coderPush pushes one commit to the change's branch during a coder run.
func (s *suite) coderPush(item ports.ItemRef, ch ports.ChangeRef) ports.Commit {
	s.t.Helper()
	var head ports.Commit
	s.coderRun(item, func() {
		var err error
		head, err = s.env().Driver.Push(s.env().Coder, s.getChange(ch).Branch)
		s.must(err)
	})
	return head
}

func (s *suite) passGates(c ports.Commit) {
	s.t.Helper()
	for _, g := range s.env().Gates {
		s.must(s.env().Driver.SetGate(c, g, ports.GatePass))
	}
}

// trackerFeed reads the tracker feed from cur until a call returns no
// events (D2).
func (s *suite) trackerFeed(cur ports.Cursor) ([]ports.ItemEvent, ports.Cursor) {
	s.t.Helper()
	var all []ports.ItemEvent
	for {
		evs, next, err := s.env().Tracker.Changes(s.ctx(), cur)
		s.must(err)
		cur = next
		if len(evs) == 0 {
			return all, cur
		}
		all = append(all, evs...)
	}
}

// hostFeed reads the code-host feed from cur until a call returns no events.
func (s *suite) hostFeed(cur ports.Cursor) ([]ports.ChangeEvent, ports.Cursor) {
	s.t.Helper()
	var all []ports.ChangeEvent
	for {
		evs, next, err := s.env().Host.Changes(s.ctx(), cur)
		s.must(err)
		cur = next
		if len(evs) == 0 {
			return all, cur
		}
		all = append(all, evs...)
	}
}

func (s *suite) manual(ch ports.ChangeRef) []ports.ManualComment {
	s.t.Helper()
	mcs, err := s.env().Host.ManualComments(s.ctx(), ch)
	s.must(err)
	return mcs
}

func hasMarker(ms []ports.Marker, m ports.Marker) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}
