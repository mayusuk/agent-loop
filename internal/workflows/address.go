package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// postAddress applies the address checks that need the repository and the
// tracker, then posts each reply as the coder identity in its finding's or
// comment's thread (INTEGRATION-SPEC §5.2 address, §6.4):
//
//   - done.head is the branch's remote head;
//   - FR-5 AC2 again, where a deferred reply counts only when deferred_to is
//     an open item (FR-5 AC5);
//   - in base_update mode, done.head is one merge commit whose first parent
//     is the input head and whose second parent contains the base tip
//     (FR-5 AC6).
func (r *run) postAddress(ctx context.Context, d *contract.AddressDone) (StageReport, error) {
	a := r.a
	change := ports.ChangeRef(r.req.Run.Change)
	head, err := a.Git.RemoteHead(ctx, a.Repo, r.branch)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return StageReport{}, err
	}
	if string(head) != d.Head {
		return r.failed(fmt.Sprintf("done.head %s is not the remote head %s of %s", d.Head, head, r.branch)), nil
	}

	open := map[string]bool{}
	deferredOK := func(item string) bool {
		item = strings.TrimSpace(item)
		if v, ok := open[item]; ok {
			return v
		}
		it, err := a.Tracker.Item(ctx, ports.ItemRef(item))
		open[item] = err == nil && it.CloseReason == ports.CloseOpen
		return open[item]
	}
	mf, mm := contract.MissingReplies(r.in, d, deferredOK)
	moved := d.Head != r.in.Workspace.Head
	if len(mf) > 0 && !moved {
		return r.failed(fmt.Sprintf("the head did not move and %s have no reply that counts (FR-5 AC2, AC5)", strings.Join(mf, ", "))), nil
	}
	if len(mm) > 0 {
		return r.failed(fmt.Sprintf("manual comments %s have no reply that counts (FR-5 AC2, AC5)", strings.Join(mm, ", "))), nil
	}

	if r.in.Address.Mode == contract.ModeBaseUpdate {
		if reason, err := r.checkBaseMerge(ctx, ports.Commit(d.Head)); err != nil || reason != "" {
			if err != nil {
				return StageReport{}, err
			}
			return r.failed(reason), nil
		}
	}

	st, err := a.readReviews(ctx, change)
	if err != nil {
		return StageReport{}, err
	}
	mcs, err := a.Host.ManualComments(ctx, change)
	if err != nil {
		return StageReport{}, err
	}
	mthread := map[string]ports.ThreadRef{}
	for _, m := range mcs {
		if m.Kind == ports.ManualCommentThread && m.Thread != "" {
			mthread[m.ID] = m.Thread
		}
	}

	out := &core.AddressOutcome{Head: d.Head}
	for _, rp := range d.Replies {
		body, err := readOut(r.outDir, rp.BodyFile)
		if err != nil {
			return StageReport{}, err
		}
		to, ok := st.threads[rp.Finding]
		if !ok {
			to = ports.ThreadRef(change) // a finding with no thread (I8)
		}
		key := r.key("reply:" + rp.Finding)
		rec, err := record(contract.Record{Type: contract.RecordReply, Key: string(key), Change: string(change),
			Finding: rp.Finding, Kind: string(rp.Kind), DeferredTo: rp.DeferredTo, Head: d.Head})
		if err != nil {
			return StageReport{}, err
		}
		if to == ports.ThreadRef(change) {
			body = fmt.Sprintf("**Reply to %s** (%s)\n\n%s", rp.Finding, rp.Kind, strings.TrimSpace(body))
		}
		text := a.text(body)
		if err := a.retry(ctx, func() error { return a.Host.PostReply(ctx, change, to, rec, text, key) }); err != nil {
			return StageReport{}, err
		}
		out.Replies = append(out.Replies, core.ReplyOutcome{To: rp.Finding, Kind: rp.Kind, Counts: counts(rp.Kind, rp.DeferredTo, deferredOK)})
	}
	for _, rp := range d.ManualReplies {
		body, err := readOut(r.outDir, rp.BodyFile)
		if err != nil {
			return StageReport{}, err
		}
		to, ok := mthread[rp.Comment]
		if !ok {
			// A review body or a comment with no thread: a change comment
			// that names the comment (§6.1 manual_reply).
			to = ports.ThreadRef(change)
			body = fmt.Sprintf("**Reply to %s** (%s)\n\n%s", rp.Comment, rp.Kind, strings.TrimSpace(body))
		}
		key := r.key("manual_reply:" + rp.Comment)
		rec, err := record(contract.Record{Type: contract.RecordManualReply, Key: string(key), Change: string(change),
			Comment: rp.Comment, Kind: string(rp.Kind), DeferredTo: rp.DeferredTo})
		if err != nil {
			return StageReport{}, err
		}
		text := a.text(body)
		if err := a.retry(ctx, func() error { return a.Host.PostReply(ctx, change, to, rec, text, key) }); err != nil {
			return StageReport{}, err
		}
		out.ManualReplies = append(out.ManualReplies, core.ReplyOutcome{To: rp.Comment, Kind: rp.Kind, Counts: counts(rp.Kind, rp.DeferredTo, deferredOK)})
	}
	res := r.result(contract.OutcomeDone)
	res.Address = out
	return StageReport{Result: res}, nil
}

func counts(k contract.ReplyKind, to *string, ok func(string) bool) bool {
	return k != contract.ReplyDeferred || (to != nil && strings.TrimSpace(*to) != "" && ok(*to))
}

// checkBaseMerge checks a base_update run's head (FR-5 AC6): one new merge
// commit, first parent the input head, second parent a base commit that
// contains the input base tip. It returns the reason the result fails, or
// "".
func (r *run) checkBaseMerge(ctx context.Context, head ports.Commit) (string, error) {
	ps, err := r.a.Git.Parents(ctx, r.dir, head)
	if errors.Is(err, ports.ErrNotFound) {
		return fmt.Sprintf("done.head %s is not in the run's tree", head), nil
	}
	if err != nil {
		return "", err
	}
	if len(ps) != 2 {
		return fmt.Sprintf("done.head %s has %d parents, not one merge of the base (FR-5 AC6)", head, len(ps)), nil
	}
	if string(ps[0]) != r.in.Workspace.Head {
		return fmt.Sprintf("done.head %s's first parent %s is not the input head %s (FR-5 AC6)", head, ps[0], r.in.Workspace.Head), nil
	}
	tip := ""
	if r.in.Change != nil {
		tip = r.in.Change.BaseTip
	}
	ok, err := r.a.Git.IsAncestor(ctx, r.dir, ports.Commit(tip), ps[1])
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("done.head %s's second parent %s doesn't contain the base tip %s (FR-5 AC6)", head, ps[1], tip), nil
	}
	return "", nil
}
