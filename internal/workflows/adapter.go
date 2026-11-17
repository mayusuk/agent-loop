package workflows

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// The adapter activities: the writes and reads the core's actions need,
// made as the loop identity by the adapters. Each write passes on the
// action's idempotency key, which names the workflow ID and the claim
// number (TECHNICAL-SPEC §9), so these activities may be retried by
// Temporal (§6.5). They run no handler and issue no handler token.

// MarkerRequest is a SetMarker action on an item.
type MarkerRequest struct {
	Item   string
	Action core.SetMarker
	// Change is the item's change, for a marker that goes on the change
	// (Action.OnChange: aborted, FR-10 AC7).
	Change string
}

// SetMarker sets or clears a loop marker on the item, or on its change
// when the action says so.
func (a *Activities) SetMarker(ctx context.Context, req MarkerRequest) error {
	if req.Action.OnChange {
		if req.Change == "" {
			return nil // no change to mark
		}
		return a.Host.SetLabel(ctx, ports.ChangeRef(req.Change), ports.Marker(req.Action.Marker), req.Action.On, ports.IdemKey(req.Action.Key))
	}
	return a.Tracker.SetMarker(ctx, ports.ItemRef(req.Item), ports.Marker(req.Action.Marker), req.Action.On, ports.IdemKey(req.Action.Key))
}

// StatusRequest is a PostStatus action on an item.
type StatusRequest struct {
	Item   string
	Claim  int
	Action core.PostStatus
	// DetailFile is the StageReport's DetailFile when the status is for a
	// needs_owner or blocked result: the handler's question or reason,
	// quoted in the comment.
	DetailFile string
}

// PostStatus posts a loop status comment: plain language for the owner,
// then a status record (INTEGRATION-SPEC §6.5). An escalation mentions the
// owner accounts and says what they need to do (FR-10 AC1, FR-11 AC3).
func (a *Activities) PostStatus(ctx context.Context, req StatusRequest) (string, error) {
	act := req.Action
	rec, err := record(contract.Record{
		Type: contract.RecordStatus, Key: act.Key, Item: req.Item, Event: act.Event, Reason: act.Reason,
		Claim: req.Claim, FindingIDs: act.FindingIDs, CommentIDs: act.CommentIDs, Commits: act.Commits,
		RoundsAdded: act.RoundsAdded,
	})
	if err != nil {
		return "", err
	}
	body := a.statusText(act)
	if req.DetailFile != "" {
		b, err := os.ReadFile(req.DetailFile)
		if err != nil {
			return "", fmt.Errorf("workflows: status detail: %w", err)
		}
		if q := strings.TrimSpace(string(b)); q != "" {
			body += "\n\nThe handler wrote:\n\n" + quote(q)
		}
	}
	if act.Event == contract.EventEscalate {
		if m := a.mention(); m != "" {
			body = m + " " + body
		}
	}
	ref, err := a.Tracker.Comment(ctx, ports.ItemRef(req.Item), rec, a.text(body), ports.IdemKey(act.Key))
	return string(ref), err
}

// mention is "@login" for each owner account with a login.
func (a *Activities) mention() string {
	var m []string
	for _, o := range a.Owners {
		if o.Login != "" {
			m = append(m, "@"+o.Login)
		}
	}
	return strings.Join(m, " ")
}

func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// reasonText is the plain-language cause of each reason code.
var reasonText = map[string]string{
	core.ReasonRoundLimit:                "the review round allowance is used up and a blocking finding is still open",
	core.ReasonWorkerLost:                "the worker was lost during the stage three times",
	core.ReasonRetriesExhausted:          "the stage failed on every attempt",
	core.ReasonTimeLimit:                 "the stage reached its time limit",
	core.ReasonNeedsOwner:                "the handler has a question for the owner",
	core.ReasonBlockedNoDependency:       "the handler is blocked and named no dependency",
	core.ReasonDependencyNotDone:         "the dependency was closed without being done",
	core.ReasonBaseUpdateLimit:           "the base-update limit is used up",
	core.ReasonUntrustedCommit:           "the change has commits the loop can't trust",
	core.ReasonBranchOutsidePrefix:       "the change's branch isn't under the loop's branch prefix; push it to the item's loop branch instead, or approve its commits by resuming",
	core.ReasonPusherUnknown:             "the head moved while another item's coder run held a push token, so the pusher is unknown",
	core.ReasonForkChange:                "the change is from a fork, which the loop never trusts",
	core.ReasonEmptyReviewNeedsDismissal: "an empty \"changes requested\" review blocks the merge; only dismissing it clears it",
	core.ReasonGateFailed:                "a required gate failed",
	core.ReasonMergeRefused:              "the code host refused the merge",
	core.ReasonMergeFailed:               "the merge call failed",
}

// statusText is the comment's plain-language text. It names references
// only: reason codes, stages and IDs.
func (a *Activities) statusText(act core.PostStatus) string {
	var b strings.Builder
	why := reasonText[act.Reason]
	if why == "" && act.Reason != "" {
		why = "reason `" + act.Reason + "`"
	}
	switch act.Event {
	case contract.EventEscalate:
		b.WriteString("agent-loop needs you: " + why + ".")
		if act.Stage != "" {
			b.WriteString(" Stage: " + string(act.Stage) + ".")
		}
	case contract.EventBlocked:
		b.WriteString("agent-loop is waiting: the item is blocked")
		if act.Dependency != "" {
			b.WriteString(" by " + act.Dependency + ". It continues when that item is done")
		}
		b.WriteString(".")
	case contract.EventResume:
		b.WriteString("agent-loop resumed this item.")
		if act.RoundsAdded != nil && *act.RoundsAdded > 0 {
			fmt.Fprintf(&b, " Review rounds added: %d.", *act.RoundsAdded)
		}
	default:
		fmt.Fprintf(&b, "agent-loop: %s", act.Event)
		if why != "" {
			b.WriteString(": " + why)
		}
		b.WriteString(".")
	}
	list := func(name string, ids []string) {
		if len(ids) > 0 {
			b.WriteString("\n\n" + name + ": " + strings.Join(ids, ", "))
		}
	}
	list("Findings", act.FindingIDs)
	list("Manual comments", act.CommentIDs)
	list("Commits", act.Commits)
	list("Gates", act.Gates)
	if act.Event == contract.EventEscalate {
		b.WriteString("\n\nWhen it is settled, remove `needs-owner` to resume the item.")
	}
	return b.String()
}

// NotifyRequest is a Notify action on an item.
type NotifyRequest struct {
	Item   string
	URL    string
	Action core.Notify
}

// Notify notifies the owner on every channel (FR-11 AC3, TECHNICAL-SPEC
// §12).
func (a *Activities) Notify(ctx context.Context, req NotifyRequest) error {
	if a.Notifier == nil {
		return nil
	}
	act := req.Action
	msg := fmt.Sprintf("Item %s: %s", req.Item, act.Event)
	if why := reasonText[act.Reason]; why != "" {
		msg = fmt.Sprintf("Item %s needs you: %s", req.Item, why)
	}
	return a.Notifier.Notify(ctx, ports.Notification{
		Project: a.Project, Item: ports.ItemRef(req.Item), Reason: act.Reason, Message: msg, URL: req.URL, Key: ports.IdemKey(act.Key),
	})
}

// TrustCheck runs the pre-stage trust check of a change (TECHNICAL-SPEC
// §8.3). The workflow applies the owner's approvals with Check.
func (a *Activities) TrustCheck(ctx context.Context, change string) (core.TrustReport, error) {
	tr, err := a.Host.Trust(ctx, ports.ChangeRef(change))
	if err != nil {
		return core.TrustReport{}, err
	}
	return trustReport(tr), nil
}

func trustReport(tr ports.TrustReport) core.TrustReport {
	var r core.TrustReport
	for _, c := range tr.Causes {
		r.Causes = append(r.Causes, string(c))
	}
	for _, c := range tr.Commits {
		r.Commits = append(r.Commits, string(c))
	}
	return r
}

// MergeCheckRequest is a CheckMerge action. Pusher is who pushed the head,
// as the workflow last saw it.
type MergeCheckRequest struct {
	Action core.CheckMerge
	Pusher core.Pusher
}

// CheckMerge reads the merge facts right before merging (FR-7): the head,
// trust, whether the head contains the base tip, and the required gates on
// the head.
func (a *Activities) CheckMerge(ctx context.Context, req MergeCheckRequest) (core.MergeChecked, error) {
	ref := ports.ChangeRef(req.Action.Change)
	ch, err := a.Host.Change(ctx, ref)
	if err != nil {
		return core.MergeChecked{}, err
	}
	tr, err := a.Host.Trust(ctx, ref)
	if err != nil {
		return core.MergeChecked{}, err
	}
	hasBase, err := a.Host.ContainsBase(ctx, ref)
	if err != nil {
		return core.MergeChecked{}, err
	}
	gs, err := a.Host.Gates(ctx, ref, ch.Head)
	if err != nil {
		return core.MergeChecked{}, err
	}
	out := core.MergeChecked{At: core.At{Time: a.now()}, Head: string(ch.Head), Pusher: req.Pusher, Trust: trustReport(tr), ContainsBase: hasBase}
	for _, g := range gs {
		out.Gates = append(out.Gates, core.Gate{Name: g.Name, State: core.GateState(g.State)})
	}
	return out, nil
}

// Merge merges the change as the loop identity, pinned to the reviewed
// commit, with the profile's merge method (FR-7 AC5, AC7). A retry finds it
// merged at that commit (TECHNICAL-SPEC §9).
func (a *Activities) Merge(ctx context.Context, act core.Merge) (core.MergeResult, error) {
	m := a.MergeMethod
	if m == "" {
		m = ports.MergeMethodSquash
	}
	o, err := a.Host.Merge(ctx, ports.ChangeRef(act.Change), ports.Commit(act.Pinned), m, ports.IdemKey(act.Key))
	if err != nil {
		return core.MergeResult{}, err
	}
	return core.MergeResult{At: core.At{Time: a.now()}, Outcome: core.MergeOutcome(o)}, nil
}

// FollowUpRequest is a FileFollowUp action.
type FollowUpRequest struct {
	Action core.FileFollowUp
}

// FileFollowUp files the follow-up item for the findings left open (FR-8):
// one row per finding with its ID, score, status and link, and no text from
// the review (AC1); the profile's type, follow-up and ready markers (AC2,
// AC5); links to the original item and change; the key followup:<change>
// (AC3). It then comments on the original item with a link to it (AC5),
// and returns the follow-up's ref.
func (a *Activities) FileFollowUp(ctx context.Context, req FollowUpRequest) (string, error) {
	act := req.Action
	ch, err := a.Host.Change(ctx, ports.ChangeRef(act.Change))
	if err != nil {
		return "", err
	}
	orig, err := a.Tracker.Item(ctx, ports.ItemRef(act.Item))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Findings left open when change %s (%s) for item %s (%s) merged.\n\n", ch.Ref, ch.URL, orig.Ref, orig.URL)
	b.WriteString("| Finding | Score | Status | Link |\n|---|---|---|---|\n")
	for _, r := range act.Rows {
		link := ch.URL
		if r.Comment != "" {
			link = fmt.Sprintf("%s (thread %s)", ch.URL, r.Comment)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s |\n", r.ID, r.Score, r.Status, link)
	}
	rec, err := record(contract.Record{Type: contract.RecordFollowup, Key: act.Key, Item: act.Item, Change: act.Change})
	if err != nil {
		return "", err
	}
	markers := []ports.Marker{ports.MarkerFollowUp}
	if t := a.FollowUpType; t != "" {
		markers = append([]ports.Marker{t}, markers...)
	} else {
		markers = append([]ports.Marker{ports.MarkerTypeFeature}, markers...)
	}
	if a.FollowUpReady {
		markers = append(markers, ports.MarkerReady)
	}
	n := ports.NewItem{
		Markers: markers,
		Title:   a.text(fmt.Sprintf("Follow-up: findings left open on change %s", ch.Ref)),
		Body:    a.text(b.String()),
		Record:  rec,
		Since:   ch.MergedAt,
	}
	ref, err := a.Tracker.CreateItem(ctx, n, ports.IdemKey(act.Key))
	if err != nil {
		return "", err
	}
	fu, err := a.Tracker.Item(ctx, ref)
	if err != nil {
		return "", err
	}
	ckey := act.Key + ":comment"
	crec, err := record(contract.Record{Type: contract.RecordFollowup, Key: ckey, Item: string(ref), Change: act.Change})
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("agent-loop filed follow-up %s (%s) for the findings left open on change %s.", ref, fu.URL, ch.Ref)
	if _, err := a.Tracker.Comment(ctx, ports.ItemRef(act.Item), crec, a.text(text), ports.IdemKey(ckey)); err != nil {
		return "", err
	}
	return string(ref), nil
}

// ApproveRequest is an ApproveCommits action on an item.
type ApproveRequest struct {
	Item   string
	Claim  int
	Action core.ApproveCommits
}

// ApproveCommits writes the approved_commits record of the owner's resume
// (FR-13 AC6, INTEGRATION-SPEC §9.8 step 4).
func (a *Activities) ApproveCommits(ctx context.Context, req ApproveRequest) error {
	act := req.Action
	rec, err := record(contract.Record{Type: contract.RecordApprovedCommits, Key: act.Key, Item: req.Item, Claim: req.Claim, Commits: act.Commits})
	if err != nil {
		return err
	}
	text := "The owner's resume approved these commits: " + strings.Join(act.Commits, ", ") + "."
	_, err = a.Tracker.Comment(ctx, ports.ItemRef(req.Item), rec, a.text(text), ports.IdemKey(act.Key))
	return err
}
