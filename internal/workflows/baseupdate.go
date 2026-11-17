package workflows

import (
	"context"
	"errors"
	"fmt"

	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// BaseUpdateRequest is one UpdateBase call: the core's action and the
// item's branch.
type BaseUpdateRequest struct {
	Item   string
	Branch string // the change's branch; empty means <branch_prefix><item>
	Action core.UpdateBase
}

// BaseUpdateReport is the base update's outcome, as the event for
// core.Decide: exactly one field is set.
type BaseUpdateReport struct {
	Updated   *core.BaseUpdated       `json:",omitempty"`
	Conflicts *core.BaseConflicts     `json:",omitempty"`
	Refused   *core.BaseUpdateRefused `json:",omitempty"`
}

// Event returns the report's event.
func (r BaseUpdateReport) Event() core.Event {
	switch {
	case r.Updated != nil:
		return *r.Updated
	case r.Conflicts != nil:
		return *r.Conflicts
	case r.Refused != nil:
		return *r.Refused
	}
	return nil
}

// UpdateBase merges the base tip into the item's branch with no handler and
// no agent run (D41, INTEGRATION-SPEC §5.3): a merge without a working tree
// (git merge-tree); without conflicts one merge commit, first parent the
// branch head and second the base tip, authored and committed as the coder
// identity, pushed as a plain fast-forward with the coder worker token. With
// conflicts it pushes nothing: the core starts an address run in
// base_update mode. If the branch head moved since the core read it, or
// moves before the push, nothing is pushed or retried. The run is recorded
// as a coder run (I11) and its token is revoked when it ends. The loop
// identity is never used (PRODUCT-SPEC §8 item 8).
func (a *Activities) UpdateBase(ctx context.Context, req BaseUpdateRequest) (BaseUpdateReport, error) {
	at := func() core.At { return core.At{Time: a.now()} }
	branch := req.Branch
	if branch == "" {
		branch = a.BranchPrefix + req.Item
	}
	expected := ports.Commit(req.Action.Head)
	runID := req.Action.Key
	item := ports.ItemRef(req.Item)

	tokens := secrets.NewRun(a.Secrets)
	endHead := expected
	started := false
	// Revoke first, then end the coder run, so no coder token is live
	// outside a recorded run (I11).
	defer func() {
		tokens.Close(context.WithoutCancel(ctx))
		if started {
			a.CoderRuns.Ended(context.WithoutCancel(ctx), item, runID, a.now(), endHead)
		}
	}()
	if a.CoderRuns != nil {
		if err := a.CoderRuns.Started(ctx, item, runID, a.now()); err != nil {
			return BaseUpdateReport{}, fmt.Errorf("workflows: coder run: %w", err)
		}
		started = true
	}

	lease, err := tokens.Issue(ctx, ports.IdentityCoder, a.coderWorkerScope())
	if err != nil {
		return BaseUpdateReport{}, fmt.Errorf("workflows: coder worker token: %w", err)
	}
	tok, err := lease.Token(ctx)
	if err != nil {
		return BaseUpdateReport{}, err
	}
	a.redactor().Add(tok.Secret(), tok.ExpiresAt)

	d, err := a.Workspace.Prepare(ctx, a.Repo, ports.Branch{Name: branch})
	if err != nil {
		return BaseUpdateReport{}, fmt.Errorf("workflows: workspace: %w", err)
	}
	defer a.Workspace.Release(context.WithoutCancel(ctx), d)
	head := d.Head
	endHead = head
	base, err := a.Git.FetchedHead(ctx, d, a.Repo.BaseBranch)
	if err != nil {
		return BaseUpdateReport{}, err
	}
	hasBase, err := a.Git.IsAncestor(ctx, d, base, head)
	if err != nil {
		return BaseUpdateReport{}, err
	}
	if head != expected {
		// A retry after this activity's own push finds its merge commit:
		// first parent the expected head, containing the base.
		if ps, err := a.Git.Parents(ctx, d, head); err == nil && len(ps) == 2 && ps[0] == expected && hasBase {
			return BaseUpdateReport{Updated: &core.BaseUpdated{At: at(), Head: string(head)}}, nil
		}
		return BaseUpdateReport{Refused: &core.BaseUpdateRefused{At: at()}}, nil
	}
	if hasBase {
		return BaseUpdateReport{Updated: &core.BaseUpdated{At: at(), Head: string(head)}}, nil
	}
	tree, conflicts, err := a.Git.MergeTree(ctx, d, head, base)
	if err != nil {
		return BaseUpdateReport{}, err
	}
	if len(conflicts) > 0 {
		return BaseUpdateReport{Conflicts: &core.BaseConflicts{At: at()}}, nil
	}
	msg := fmt.Sprintf("Merge %s into %s", a.Repo.BaseBranch, branch)
	mc, err := a.Git.CommitMerge(ctx, d, tree, []ports.Commit{head, base}, msg, a.CoderSignature)
	if err != nil {
		return BaseUpdateReport{}, err
	}
	err = a.Git.Push(ctx, d, a.Repo, mc, branch, tok)
	if errors.Is(err, ports.ErrConflict) {
		return BaseUpdateReport{Refused: &core.BaseUpdateRefused{At: at()}}, nil
	}
	if err != nil {
		return BaseUpdateReport{}, err
	}
	endHead = mc
	return BaseUpdateReport{Updated: &core.BaseUpdated{At: at(), Head: string(mc)}}, nil
}
