package core

import (
	"fmt"
	"slices"

	"github.com/mayusuk/agent-loop/contract"
)

// Role is who took an action, as the workflow finds it by matching the
// actor's account ID with the profile (FR-13 AC7, INTEGRATION-SPEC §8.1).
type Role string

// The roles. RoleOther is any account that is neither an owner account nor
// one of the three identities: another human, or a bot.
const (
	RoleOwner    Role = "owner"
	RoleCoder    Role = "coder"
	RoleReviewer Role = "reviewer"
	RoleLoop     Role = "loop"
	RoleOther    Role = "other"
)

// Identities are the profile's account IDs (FR-13 AC1, INTEGRATION-SPEC
// §13: owners are matched by ID).
type Identities struct {
	Owners                []string
	Coder, Reviewer, Loop string
}

// Role returns the role of the account with this stable ID. An empty ID is
// never an owner.
func (ids Identities) Role(id string) Role {
	switch {
	case id == "":
		return RoleOther
	case id == ids.Loop:
		return RoleLoop
	case id == ids.Coder:
		return RoleCoder
	case id == ids.Reviewer:
		return RoleReviewer
	case slices.Contains(ids.Owners, id):
		return RoleOwner
	}
	return RoleOther
}

// MarkerChanged: a marker was added to or removed from the item, or from
// its change when OnChange is set (TECHNICAL-SPEC §4). Only a change By an
// owner account has an effect (FR-13 AC7).
type MarkerChanged struct {
	At
	By       Role
	Marker   string
	Added    bool
	OnChange bool
}

// Command is an owner command from the API (TECHNICAL-SPEC §11). API
// commands act as the owner.
type Command string

// The item commands.
const (
	CommandPause  Command = "pause"
	CommandResume Command = "resume"
	CommandAbort  Command = "abort"
)

// OwnerCommand: `agent-loop item pause|resume|abort` (FR-10 AC3, AC7).
type OwnerCommand struct {
	At
	Command Command
}

// Assigned: a human was assigned to the claimed item (FR-12 AC4).
type Assigned struct {
	At
	By Role
}

// ItemClosed: the item was closed while its change wasn't merged
// (FR-12 AC4). Any account's close counts (FR-13 AC7).
type ItemClosed struct {
	At
	By Role
}

// ChangeClosed: the change was closed without merging (FR-12 AC4).
type ChangeClosed struct {
	At
	By     Role
	Change string
}

// ChangeMerged: the change was merged. A merge By anyone but the loop is a
// merge by hand (FR-12 AC4, FR-8 AC4).
type ChangeMerged struct {
	At
	By     Role
	Change string
}

// ChangeReopened: a closed change of the claimed item is open again, with
// its review records (FR-12 AC4, FR-6 AC5).
type ChangeReopened struct {
	At
	Change  Change
	Reviews []ReviewRecord
}

// Commented: someone commented on the item or change outside a review. It
// is recorded as data only (FR-12 AC4, §8.5).
type Commented struct {
	At
	By Role
}

// Name implementations.
func (MarkerChanged) Name() string  { return "marker_changed" }
func (OwnerCommand) Name() string   { return "owner_command" }
func (Assigned) Name() string       { return "assigned" }
func (ItemClosed) Name() string     { return "item_closed" }
func (ChangeClosed) Name() string   { return "change_closed" }
func (ChangeMerged) Name() string   { return "change_merged" }
func (ChangeReopened) Name() string { return "change_reopened" }
func (Commented) Name() string      { return "commented" }

// Claimed reports whether the item holds a claim in one of the §6 claimed
// states (FR-2 AC3).
func (s ItemState) Claimed() bool {
	switch s.State {
	case StateNone, StateMerged, StateClosed, StateReleased:
		return false
	}
	return true
}

// live is a claimed item, or one whose release waits for its running work.
func (d *decision) live() bool {
	return d.s.Claimed() || (d.s.State == StateReleased && d.s.Running != "")
}

// owner handles the owner's actions and the manual actions of FR-12 AC4.
// It returns false for any other event.
func (d *decision) owner(e Event) bool {
	switch e := e.(type) {
	case MarkerChanged:
		d.markerChanged(e)
	case OwnerCommand:
		d.command(e)
	case Resumed:
		d.resumed()
	case BlockRemoved:
		d.blockRemoved()
	case Assigned:
		d.release("FR-12 AC4 item assigned to a human: release the claim")
	case ItemClosed:
		d.itemClosed()
	case ChangeClosed:
		d.changeClosed(e)
	case ChangeMerged:
		d.changeMerged(e)
	case ChangeReopened:
		d.changeReopened(e)
	case Commented:
		d.rule = "FR-12 AC4 comment recorded as data only (§8.5)"
	default:
		return false
	}
	return true
}

func (d *decision) markerChanged(e MarkerChanged) {
	if e.By != RoleOwner {
		d.rule = "FR-13 AC7 label change by an account that isn't an owner: no effect"
		return
	}
	if !d.s.Claimed() {
		return
	}
	if e.OnChange {
		d.rule = "FR-12 AC4 marker changed only on the change: no effect"
		return
	}
	switch e.Marker {
	case MarkerPaused:
		if e.Added {
			d.pause()
		} else {
			d.unpause()
		}
	case MarkerAborted:
		if e.Added {
			d.abort()
		} else if d.s.State == StateAborted {
			d.release("FR-10 AC7 owner removed aborted from the item: release the claim")
		}
	case MarkerInProgress:
		if !e.Added {
			d.release("FR-12 AC4 owner removed in-progress: release the claim")
		}
	case MarkerNeedsOwner:
		if e.Added {
			d.ownerEscalate()
		} else {
			d.resumed()
		}
	case MarkerBlocked:
		if !e.Added {
			d.blockRemoved()
		}
	}
	if d.rule == "ignored" {
		d.rule = "FR-12 AC4 label change has no effect on a claimed item"
	}
}

func (d *decision) command(e OwnerCommand) {
	switch e.Command {
	case CommandPause:
		d.pause()
	case CommandResume:
		if d.s.State == StatePaused {
			d.unpause()
		} else {
			d.resumed()
		}
	case CommandAbort:
		d.abort()
	}
}

// inFlight reports whether work the core started is still running: a
// handler run, the agent-free base merge or a merge call.
func (d *decision) inFlight() bool {
	switch d.s.State {
	case StateImplementing, StateInReview, StateAddressing:
		return d.s.Run != nil || d.s.UpdatingBase
	case StateMerging:
		return d.s.Merge != nil && d.s.Merge.Calling
	}
	return false
}

// hold moves the item to an owner state and keeps any in-flight work in
// Running, so its result is still recorded (work plan #12 D1). Anything
// that isn't running (a pending run, a merge check) is dropped.
func (d *decision) hold(target State) {
	if d.s.Running == "" && d.inFlight() {
		d.s.Running = d.s.State
	}
	if d.s.Running == "" {
		d.s.Run, d.s.Merge, d.s.UpdatingBase = nil, nil, false
	}
	d.s.State = target
}

func isHold(s State) bool {
	switch s {
	case StatePaused, StateAborted, StateEscalated, StateReleased:
		return true
	}
	return false
}

// under is the state whose events a held item still takes: its in-flight
// stage, or blocked while it waits for a dependency (work plan #12 D2).
func (d *decision) under() State {
	if d.s.Running != "" {
		return d.s.Running
	}
	if d.s.BlockedOn != "" && d.s.State != StateReleased {
		return StateBlocked
	}
	return ""
}

func (d *decision) flight() string {
	run, calling := "", false
	if d.s.Run != nil {
		run = d.s.Run.RunID
	}
	if d.s.Merge != nil {
		calling = d.s.Merge.Calling
	}
	return fmt.Sprintf("%s|%t|%t|%s", run, d.s.UpdatingBase, calling, d.s.BlockedOn)
}

// held decides an event for an item an owner action holds, as if it were
// in its underlying state, then keeps it held: nothing new starts, and the
// hold says what else is dropped (work plan #12 D1, D3, D6).
func (d *decision) held(e Event) {
	hold, under := d.s.State, d.under()
	before := d.flight()
	d.s.State, d.s.Running = under, ""
	mark := len(d.actions)
	d.plain(e)
	started := slices.ContainsFunc(d.actions[mark:], isStart)
	if !started && d.s.State == under && d.flight() == before {
		// Nothing ended (for example a head move was recorded).
		d.s.State = hold
		if under != StateBlocked {
			d.s.Running = under
		}
		return
	}
	if d.s.State == StateMerged {
		// The merge call finished: the merge stands (work plan #12 D1).
		d.filter(mark, isStart)
		return
	}
	escalated := d.s.State == StateEscalated
	switch hold {
	case StatePaused:
		d.filter(mark, isStart, isEscalationAct)
		if escalated {
			d.s.PendingEscalation = true
		}
	case StateAborted, StateReleased:
		d.filter(mark, isStart, isEscalationAct, isBlockAct)
		d.s.Escalation, d.s.PendingEscalation, d.s.BlockedOn = nil, false, ""
	default: // escalated by the owner
		d.filter(mark, isStart)
	}
	d.s.State = hold
	d.s.Run, d.s.Merge, d.s.UpdatingBase, d.s.Running = nil, nil, false, ""
	d.rule += fmt.Sprintf(" (held %s: nothing new starts)", hold)
	switch {
	case hold == StateEscalated && !escalated && d.s.PendingEscalation:
		d.s.PendingEscalation = false
		d.announce(*d.s.Escalation)
	case hold == StateReleased:
		d.finishRelease(d.rule)
	}
}

// filter drops the actions emitted since mark that any of drop matches.
func (d *decision) filter(mark int, drop ...func(Action) bool) {
	out := slices.Clone(d.actions[:mark])
	for _, a := range d.actions[mark:] {
		if !slices.ContainsFunc(drop, func(f func(Action) bool) bool { return f(a) }) {
			out = append(out, a)
		}
	}
	d.actions = out
}

func isStart(a Action) bool {
	switch a.(type) {
	case RunStage, CheckMerge, UpdateBase, Merge, Wait:
		return true
	}
	return false
}

func isEscalationAct(a Action) bool {
	switch a := a.(type) {
	case SetMarker:
		return a.Marker == MarkerNeedsOwner && a.On
	case PostStatus:
		return a.Event == contract.EventEscalate
	case Notify:
		return a.Event == contract.EventEscalate
	}
	return false
}

func isBlockAct(a Action) bool {
	switch a := a.(type) {
	case SetMarker:
		return a.Marker == MarkerBlocked && a.On
	case PostStatus:
		return a.Event == contract.EventBlocked
	}
	return false
}

// pause holds the item (FR-10 AC3). The running handler finishes and its
// result is recorded.
func (d *decision) pause() {
	if !d.s.Claimed() || d.s.State == StatePaused || d.s.State == StateAborted {
		return
	}
	d.hold(StatePaused)
	d.emit(
		d.marker(MarkerPaused, true),
		PostStatus{Key: d.key("pause"), Event: contract.EventPause},
		Wait{Reason: "paused"},
	)
	d.rule = "FR-10 AC3 owner paused the item"
}

// unpause resumes a paused item: escalated if it has an escalation,
// otherwise the next stage (§6 owner-actions table, FR-10 AC3, AC8).
func (d *decision) unpause() {
	if d.s.State != StatePaused {
		return
	}
	zero := 0
	d.emit(
		d.marker(MarkerPaused, false),
		PostStatus{Key: d.key("resume"), Event: contract.EventResume, Reason: ReasonPaused, RoundsAdded: &zero},
	)
	switch {
	case d.s.Escalation != nil:
		d.s.State = StateEscalated
		if d.s.PendingEscalation {
			d.s.PendingEscalation = false
			d.announce(*d.s.Escalation)
		} else {
			d.emit(Wait{Reason: "escalated: " + d.s.Escalation.Reason})
		}
		d.rule = "§6 resume of a paused item with needs-owner: escalated"
	case d.s.Running != "":
		d.s.State, d.s.Running = d.s.Running, ""
		d.rule = "FR-10 AC3 resume: the running work continues"
	case d.s.BlockedOn != "":
		d.s.State = StateBlocked
		d.emit(Wait{Reason: "blocked on " + d.s.BlockedOn})
		d.rule = "FR-10 AC3 resume: still blocked on an open dependency (work plan #12 D2)"
	default:
		ra := d.s.ResumeAddress
		d.s.ResumeAddress = false
		d.s.Run = nil
		d.nextStage(ra)
		d.rule = "FR-10 AC3 resume: " + d.rule
	}
}

// abort stops the item after the running work (FR-10 AC7). It keeps
// in-progress and leaves the change open.
func (d *decision) abort() {
	if !d.s.Claimed() || d.s.State == StateAborted {
		return
	}
	d.hold(StateAborted)
	d.s.Escalation, d.s.PendingEscalation, d.s.ResumeAddress = nil, false, false
	d.emit(d.marker(MarkerAborted, true))
	if d.s.Change != nil {
		d.emit(d.changeMarker(MarkerAborted, true))
	}
	d.emit(PostStatus{Key: d.key("abort"), Event: contract.EventAbort}, Wait{Reason: "aborted"})
	d.rule = "FR-10 AC7 owner aborted the item"
}

// changeMarker sets or clears a marker on the item's change.
func (d *decision) changeMarker(name string, on bool) SetMarker {
	return SetMarker{Key: d.key(fmt.Sprintf("change_marker:%s:%t", name, on)), Marker: name, On: on, OnChange: true}
}

// ownerEscalate is the owner adding needs-owner (FR-12 AC4): the item is
// escalated after the running work ends; a paused item when it resumes;
// an aborted item stays aborted.
func (d *decision) ownerEscalate() {
	switch d.s.State {
	case StateEscalated, StateAborted:
		return
	case StatePaused:
		if d.s.Escalation == nil {
			d.s.Escalation = &Escalation{Reason: ReasonOwnerEscalated}
			d.s.PendingEscalation = true
		}
		d.rule = "FR-12 AC4 needs-owner on a paused item: escalated when it resumes"
		return
	}
	inflight := d.inFlight()
	d.hold(StateEscalated)
	esc := Escalation{Reason: ReasonOwnerEscalated}
	d.s.Escalation = &esc
	if inflight {
		d.s.PendingEscalation = true
		d.emit(Wait{Reason: "escalated after the running work"})
		d.rule = "FR-12 AC4 owner added needs-owner: escalate after the running work"
		return
	}
	d.announce(esc)
	d.rule = "FR-12 AC4 owner added needs-owner: escalate"
}

// release ends the claim once nothing is in flight (FR-12 AC4).
func (d *decision) release(rule string) {
	if !d.s.Claimed() {
		return
	}
	if d.s.Running != "" || d.inFlight() {
		d.hold(StateReleased)
		d.s.Escalation, d.s.PendingEscalation, d.s.BlockedOn = nil, false, ""
		d.emit(Wait{Reason: "release after the running work"})
		d.rule = rule + " after the running work"
		return
	}
	d.finishRelease(rule)
}

// clearLabels removes the loop's state labels from the item, and aborted
// from the change (§6 owner-actions table, FR-12 AC4).
func (d *decision) clearLabels() {
	for _, m := range []string{MarkerInProgress, MarkerBlocked, MarkerNeedsOwner, MarkerPaused, MarkerAborted} {
		d.emit(d.marker(m, false))
	}
	if d.s.Change != nil {
		d.emit(d.changeMarker(MarkerAborted, false))
	}
}

// leave clears everything the item held in the loop.
func (d *decision) leave(st State) {
	d.s.State = st
	d.s.Run, d.s.Merge, d.s.UpdatingBase, d.s.Running = nil, nil, false, ""
	d.s.Escalation, d.s.PendingEscalation, d.s.BlockedOn, d.s.ResumeAddress = nil, false, "", false
}

func (d *decision) finishRelease(rule string) {
	d.clearLabels()
	d.leave(StateReleased)
	d.emit(PostStatus{Key: d.key("release"), Event: contract.EventRelease, Reason: ReasonReleased}, EndItem{Reason: ReasonReleased})
	d.rule = rule
}

// stop cancels the running handler run, if one runs (§6, TECHNICAL-SPEC
// §6.5). The base merge and a merge call are short adapter calls that are
// left to end; their outcomes are then ignored (work plan #12 D8).
func (d *decision) stop() {
	agent := func(s State) bool {
		return s == StateImplementing || s == StateInReview || s == StateAddressing
	}
	if d.s.Run != nil && !d.s.UpdatingBase && (agent(d.s.State) || agent(d.s.Running)) {
		d.emit(StopRun{RunID: d.s.Run.RunID})
	}
	d.s.Run, d.s.Merge, d.s.UpdatingBase, d.s.Running = nil, nil, false, ""
}

// itemClosed: the claim ends and the loop's labels go (FR-12 AC4).
func (d *decision) itemClosed() {
	if !d.live() {
		return
	}
	d.stop()
	d.clearLabels()
	d.leave(StateClosed)
	d.emit(PostStatus{Key: d.key("manual_action"), Event: contract.EventManualAction, Reason: ReasonItemClosed},
		EndItem{Reason: ReasonItemClosed})
	d.rule = "FR-12 AC4 item closed by hand: stop, release the claim"
}

// changeMerged: a merge by hand ends the item with no follow-up
// (FR-12 AC4, FR-8 AC4, work plan #11 D12).
func (d *decision) changeMerged(e ChangeMerged) {
	if e.By == RoleLoop || !d.live() || (d.s.Change != nil && e.Change != d.s.Change.Ref) {
		return
	}
	d.stop()
	d.leave(StateMerged)
	d.emit(
		d.marker(MarkerInProgress, false),
		CloseItem{Key: d.key("close_item")},
		PostStatus{Key: d.key("manual_action"), Event: contract.EventManualAction, Reason: ReasonMergedByHand},
		EndItem{Reason: ReasonMergedByHand},
	)
	d.rule = "FR-12 AC4 change merged by hand: no follow-up (FR-8 AC4)"
}

// dropChange forgets the closed change and its review state; the next
// change starts its own rounds (FR-6 AC5, work plan #12 D10).
func (d *decision) dropChange() {
	d.s.Change = nil
	d.s.RoundsUsed, d.s.BaseUpdates = 0, 0
	d.s.Findings, d.s.LatestReview = nil, nil
	d.s.ManualComments, d.s.PendingRulings = nil, nil
	d.s.NextKind, d.s.OwnerPushed, d.s.UnknownPushes = "", false, nil
}

// changeClosed escalates "change closed by hand"; on resume, implement
// runs on a new branch (FR-12 AC4, D44).
func (d *decision) changeClosed(e ChangeClosed) {
	if d.s.State == StateReleased && d.s.Running != "" {
		d.stop()
		d.finishRelease("FR-12 AC4 change closed during a release: release now")
		return
	}
	if !d.s.Claimed() || d.s.Change == nil || e.Change != d.s.Change.Ref {
		return
	}
	d.stop()
	d.s.ClosedChanges++
	d.dropChange()
	d.s.ResumeAddress = false
	switch d.s.State {
	case StateAborted:
		d.rule = "FR-12 AC4 change closed on an aborted item: stays aborted"
		return
	case StatePaused:
		d.s.Escalation = &Escalation{Reason: ReasonChangeClosed}
		d.s.PendingEscalation = true
		d.unblock()
		d.rule = "FR-12 AC4 change closed on a paused item: escalated when it resumes"
		return
	}
	d.unblock()
	d.escalate(Escalation{Reason: ReasonChangeClosed}, "FR-12 AC4 change closed by hand: escalate (D44)")
}

func (d *decision) unblock() {
	if d.s.BlockedOn != "" {
		d.s.BlockedOn = ""
		d.emit(d.marker(MarkerBlocked, false))
	}
}

// adopt takes an open change and rebuilds its review state from its
// records (FR-3 AC3, FR-6 AC5).
func (d *decision) adopt(ch Change, reviews []ReviewRecord) {
	d.s.Change = &ch
	d.s.RoundsUsed, d.s.BaseUpdates = CountReviews(reviews)
	for _, r := range reviews {
		if !r.Stale {
			d.s.Findings = slices.Clone(r.Findings)
			d.s.LatestReview = &Review{Commit: r.Commit, Kind: r.Kind}
		}
	}
}

// changeReopened adopts a reopened change when the item has none and no
// work is in flight (FR-12 AC4, work plan #12 D11).
func (d *decision) changeReopened(e ChangeReopened) {
	if !d.s.Claimed() || d.s.Change != nil || d.s.Running != "" || d.inFlight() || e.Change.Ref == "" {
		return
	}
	d.adopt(e.Change, e.Reviews)
	if d.s.ClosedChanges > 0 {
		d.s.ClosedChanges--
	}
	closedEsc := d.s.Escalation != nil && d.s.Escalation.Reason == ReasonChangeClosed
	switch {
	case d.s.State == StateEscalated && closedEsc:
		d.s.Escalation = nil
		d.emit(d.marker(MarkerNeedsOwner, false),
			PostStatus{Key: d.key("manual_action"), Event: contract.EventManualAction, Reason: ReasonChangeReopened})
		d.s.Run = nil
		d.nextStage(false)
		d.rule = "FR-12 AC4 change reopened: adopted, " + d.rule
	case d.s.State == StatePaused && closedEsc:
		d.s.Escalation, d.s.PendingEscalation = nil, false
		d.rule = "FR-12 AC4 change reopened on a paused item: adopted, no escalation"
	default:
		d.rule = "FR-12 AC4 change reopened: adopted"
	}
}
