package core

import (
	"slices"

	"github.com/mayusuk/agent-loop/contract"
)

// The trust causes a code host reports (INTEGRATION-SPEC §9.8). They mirror
// ports.TrustCause, which core can't import.
const (
	CauseFork                = "fork"
	CauseBranchOutsidePrefix = "branch_outside_prefix"
	CauseRulesetMissing      = "ruleset_missing"
	CauseRulesetChanged      = "ruleset_changed"
	CauseAdminNotOwner       = "admin_not_owner"
	CausePushedBeforeRuleset = "pushed_before_ruleset"
	CausePusherUnknown       = "pusher_unknown"
)

// TrustReport is the code host's pre-stage trust check (CodeHost.Trust): the
// causes, and the commits not reachable from the base tip that they make
// untrusted. A report with no causes is trusted. It applies no approvals.
type TrustReport struct {
	Causes  []string
	Commits []string
}

// TrustVerdict is a report with the owner's approvals applied.
type TrustVerdict struct {
	Untrusted bool
	Fork      bool
	// Commits are the named commits the owner hasn't approved.
	Commits []string
	// Reason is the escalation's reason code (work plan #11 D4).
	Reason string
}

// Check applies the owner's approvals to the report (FR-13 AC5, AC6). A fork
// is always untrusted and never approved. Otherwise the change is untrusted
// when a cause names at least one commit that isn't approved (work plan #11
// D3). The workflow uses it for the pre-stage check; the core uses it again
// on TrustRefused and on every merge check.
func (t TrustReport) Check(approved []string) TrustVerdict {
	if len(t.Causes) == 0 {
		return TrustVerdict{}
	}
	v := TrustVerdict{Fork: slices.Contains(t.Causes, CauseFork)}
	for _, c := range t.Commits {
		if !slices.Contains(approved, c) && !slices.Contains(v.Commits, c) {
			v.Commits = append(v.Commits, c)
		}
	}
	v.Untrusted = v.Fork || len(v.Commits) > 0
	if !v.Untrusted {
		return TrustVerdict{}
	}
	v.Reason = trustReason(t.Causes)
	return v
}

// trustReason picks the reason code by a fixed precedence (work plan #11 D4).
func trustReason(causes []string) string {
	switch {
	case slices.Contains(causes, CauseFork):
		return ReasonForkChange
	case slices.Contains(causes, CauseBranchOutsidePrefix):
		return ReasonBranchOutsidePrefix
	case slices.ContainsFunc(causes, func(c string) bool { return c != CausePusherUnknown }):
		return ReasonUntrustedCommit
	}
	return ReasonPusherUnknown
}

// TrustRefused: the workflow's pre-stage trust check found the change
// untrusted, so it didn't start the run RunID names (work plan #11 D2).
type TrustRefused struct {
	At
	RunID  string
	Report TrustReport
}

// needsTrust reports whether a stage start needs the pre-stage trust check:
// implement on an existing change, and address (FR-13 AC5, TECHNICAL-SPEC
// §8.3).
func (d *decision) needsTrust(st contract.Stage) bool {
	return d.s.Change != nil && (st == contract.StageImplement || st == contract.StageAddress)
}

func (d *decision) trustRefused(e TrustRefused) {
	r := d.current(e.RunID)
	if r == nil || !d.needsTrust(r.Stage) {
		return
	}
	v := e.Report.Check(d.s.ApprovedCommits)
	if !v.Untrusted {
		// Every named commit is approved: start the run after all.
		d.emit(d.runStageAction(r))
		d.rule = "FR-13 AC6 named commits approved: start"
		return
	}
	d.escalateUntrusted(v, r.Stage, "FR-13 AC5 untrusted change before "+string(r.Stage))
}

// escalateUntrusted escalates an untrusted change, naming its commits
// (FR-13 AC5).
func (d *decision) escalateUntrusted(v TrustVerdict, st contract.Stage, rule string) {
	d.s.UnknownPushes = nil
	d.escalate(Escalation{Reason: v.Reason, Stage: st, Trust: true, Fork: v.Fork, Commits: v.Commits}, rule)
}

// unknownPushDuringReview escalates a review that saw an untrusted push,
// once it ends (FR-13 AC5, work plan #11 D6). It returns false when every
// such push has been approved since.
func (d *decision) unknownPushDuringReview() bool {
	var commits []string
	for _, c := range d.s.UnknownPushes {
		if !slices.Contains(d.s.ApprovedCommits, c) {
			commits = append(commits, c)
		}
	}
	d.s.UnknownPushes = nil
	if len(commits) == 0 {
		return false
	}
	d.escalateUntrusted(TrustVerdict{Untrusted: true, Commits: commits, Reason: ReasonPusherUnknown},
		contract.StageReview, "FR-13 AC5 untrusted commit found while the review ran")
	return true
}

// approve records the owner's approval of a trust escalation's commits on
// resume. A fork is never approved (FR-13 AC6, work plan #11 D5).
func (d *decision) approve(esc Escalation) {
	if !esc.Trust || esc.Fork || len(esc.Commits) == 0 {
		return
	}
	for _, c := range esc.Commits {
		if !slices.Contains(d.s.ApprovedCommits, c) {
			d.s.ApprovedCommits = append(d.s.ApprovedCommits, c)
		}
	}
	d.emit(ApproveCommits{Key: d.key("approve"), Commits: slices.Clone(esc.Commits)})
}
