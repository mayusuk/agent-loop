package fake

import (
	"fmt"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// The methods in this file script the sandbox: each acts as the given actor,
// the way a person or app would act on the real tracker or code host. They
// implement conformance.Driver. Refusals are ports.ErrPermission.

func refused(as ports.Actor, what string) error {
	return fmt.Errorf("fake: %s may not %s: %w", as.Login, what, ports.ErrPermission)
}

// NewItem creates an item as the actor.
func (f *Fake) NewItem(as ports.Actor, title, body string, markers ...ports.Marker) (ports.ItemRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newItem(as, untrusted.New(title), untrusted.New(body), markers, nil).ref, nil
}

// SetMarker adds or removes a marker (label) as the actor.
func (f *Fake) SetMarker(as ports.Actor, ref ports.ItemRef, m ports.Marker, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	f.setMarker(it, m, on, as)
	return nil
}

// Assign sets the item's assignee as the actor, or clears it when assignee
// is nil.
func (f *Fake) Assign(as ports.Actor, ref ports.ItemRef, assignee *ports.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	f.tick()
	it.assignees = nil
	human := false
	if assignee != nil {
		it.assignees = []ports.Actor{*assignee}
		human = f.isHuman(*assignee)
	}
	f.itemEvent(it, ports.AssigneeChanged{Actor: as, Human: human})
	return nil
}

// CommentItem comments on the item as the actor. rec, if set, is a record
// marker in the comment, which counts only from its allowed author (C3).
func (f *Fake) CommentItem(as ports.Actor, ref ports.ItemRef, body string, rec *ports.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	f.commentItem(it, as, untrusted.New(body), rec)
	return nil
}

// CloseItem closes the item as the actor.
func (f *Fake) CloseItem(as ports.Actor, ref ports.ItemRef, reason ports.CloseReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	if reason == ports.CloseOpen || it.closeReason != ports.CloseOpen {
		return fmt.Errorf("fake: close item %s: %w", ref, ports.ErrConflict)
	}
	f.closeItem(it, as, reason)
	return nil
}

// ReopenItem reopens a closed item as the actor.
func (f *Fake) ReopenItem(as ports.Actor, ref ports.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	if it.closeReason == ports.CloseOpen {
		return fmt.Errorf("fake: item %s is open: %w", ref, ports.ErrConflict)
	}
	f.tick()
	it.closeReason, it.closedBy = ports.CloseOpen, nil
	f.itemEvent(it, ports.ItemReopened{})
	return nil
}

// Push pushes one new commit to branch as the actor, creating the branch
// from the base tip if it doesn't exist. A branch "<login>:<name>" is in the
// actor's fork. The rulesets decide who may push (§9.8 step 1).
func (f *Fake) Push(as ports.Actor, branch string) (ports.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file := "conformance.txt" // conformance.ChangedFile
	if branch == f.cfg.BaseBranch {
		file = "conformance-base.txt"
	}
	line := fmt.Sprintf("push %d by %s\n", len(f.commits), as.Login)
	return f.push(as, branch, nil, func(files map[string]string) { files[file] += line })
}

// PushFiles pushes one new commit to branch as the actor that sets each of
// files to its content, creating the branch from the base tip if it doesn't
// exist (conformance Driver.PushFiles).
func (f *Fake) PushFiles(as ports.Actor, branch string, files map[string]string) (ports.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.push(as, branch, nil, func(fs map[string]string) {
		for p, c := range files {
			fs[p] = c
		}
	})
}

// MergeBaseInto pushes a merge of the base tip into branch as the actor.
func (f *Fake) MergeBaseInto(as ports.Actor, branch string) (ports.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.branches[branch]; !ok {
		return "", fmt.Errorf("fake: branch %s: %w", branch, ports.ErrNotFound)
	}
	return f.push(as, branch, []ports.Commit{f.branches[f.cfg.BaseBranch]}, nil)
}

func (f *Fake) push(as ports.Actor, branch string, extra []ports.Commit, edit func(map[string]string)) (ports.Commit, error) {
	if !f.canPush(as, branch) {
		return "", refused(as, "push to "+branch)
	}
	f.tick()
	tip, ok := f.branches[branch]
	if !ok {
		tip = f.branches[f.cfg.BaseBranch]
	}
	cm := f.newCommit(append([]ports.Commit{tip}, extra...), as, branch)
	if edit != nil {
		edit(cm.files)
	}
	f.branches[branch] = cm.sha
	for _, ref := range f.changeOrder {
		if c := f.changes[ref]; c.branch == branch && c.state == ports.ChangeStateOpen {
			f.hostLog = append(f.hostLog, hostEvent{ev: ports.ChangeEvent{Change: ref, At: f.now, Detail: ports.HeadMoved{Commit: cm.sha}}, commit: cm.sha})
		}
	}
	return cm.sha, nil
}

// OpenChange opens a change from branch as the actor. A non-empty item is the
// code host's closing reference, which links the change to it (§6.2).
func (f *Fake) OpenChange(as ports.Actor, item ports.ItemRef, branch string) (ports.ChangeRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.canWrite(as) && as.Kind != ports.ActorHuman {
		return "", refused(as, "open a change")
	}
	c, err := f.openChange(as, item, branch, untrusted.New(""), untrusted.New(""), nil)
	if err != nil {
		return "", err
	}
	return c.ref, nil
}

// CommentChange posts a conversation comment (not a review comment).
func (f *Fake) CommentChange(as ports.Actor, ref ports.ChangeRef, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	f.tick()
	c.comments = append(c.comments, &comment{id: f.nextID(), author: as, body: untrusted.New(body), at: f.now})
	return nil
}

// ReviewComment starts a review thread on path and line (0 for file level).
func (f *Fake) ReviewComment(as ports.Actor, ref ports.ChangeRef, path string, line int, body string) (ports.ThreadRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return "", err
	}
	t := f.newThread(c, as, path, line, untrusted.New(body), nil)
	f.hostEvent(ref, ports.ReviewSubmitted{Actor: as})
	return t.ref, nil
}

// ReplyInThread replies in a review thread as the actor.
func (f *Fake) ReplyInThread(as ports.Actor, ref ports.ChangeRef, tref ports.ThreadRef, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.thread(ref, tref)
	if err != nil {
		return err
	}
	f.tick()
	t.comments = append(t.comments, &comment{id: f.nextID(), author: as, body: untrusted.New(body), at: f.now})
	f.hostEvent(ref, ports.ReviewSubmitted{Actor: as})
	return nil
}

func (f *Fake) thread(ref ports.ChangeRef, tref ports.ThreadRef) (*thread, error) {
	t, ok := f.threads[tref]
	if !ok || t.change != ref {
		return nil, fmt.Errorf("fake: thread %s on change %s: %w", tref, ref, ports.ErrNotFound)
	}
	return t, nil
}

// ResolveThread resolves a review thread as the actor.
func (f *Fake) ResolveThread(as ports.Actor, ref ports.ChangeRef, tref ports.ThreadRef) error {
	return f.setResolved(as, ref, tref, true)
}

// UnresolveThread unresolves a review thread as the actor.
func (f *Fake) UnresolveThread(as ports.Actor, ref ports.ChangeRef, tref ports.ThreadRef) error {
	return f.setResolved(as, ref, tref, false)
}

func (f *Fake) setResolved(as ports.Actor, ref ports.ChangeRef, tref ports.ThreadRef, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.thread(ref, tref)
	if err != nil {
		return err
	}
	f.resolve(t, as, on)
	return nil
}

// SubmitReview submits a review as the actor: state is ReviewApprove,
// ReviewComment or ReviewChangesRequested. rec, if set, is a record marker
// in its body, which counts only from the reviewer identity (C3).
func (f *Fake) SubmitReview(as ports.Actor, ref ports.ChangeRef, state, body string, rec *ports.Record) (ports.ReviewRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return "", err
	}
	switch state {
	case ReviewApprove, ReviewComment, ReviewChangesRequested:
	default:
		return "", fmt.Errorf("fake: review state %q: %w", state, ports.ErrConflict)
	}
	return ports.ReviewRef(f.submitReview(c, as, state, untrusted.New(body), rec, "").id), nil
}

// DismissReview dismisses a review as the actor.
func (f *Fake) DismissReview(as ports.Actor, ref ports.ChangeRef, rr ports.ReviewRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	for _, r := range c.reviews {
		if r.id == string(rr) {
			if !r.dismissed {
				f.tick()
				r.dismissed, r.dismissedBy = true, as
				f.hostEvent(ref, ports.ReviewDismissed{Actor: as})
			}
			return nil
		}
	}
	return fmt.Errorf("fake: review %s: %w", rr, ports.ErrNotFound)
}

// MergeChange merges the change as the actor (a merge by hand). Only those
// who may update the base branch can (§9.8 step 1, I10).
func (f *Fake) MergeChange(as ports.Actor, ref ports.ChangeRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	if !f.canPush(as, f.cfg.BaseBranch) {
		return refused(as, "merge into "+f.cfg.BaseBranch)
	}
	if c.state != ports.ChangeStateOpen {
		return fmt.Errorf("fake: change %s is %s: %w", ref, c.state, ports.ErrConflict)
	}
	f.merge(c, as, ports.MergeMethodSquash)
	return nil
}

// CloseChange closes the change without merging, as the actor.
func (f *Fake) CloseChange(as ports.Actor, ref ports.ChangeRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	if c.state != ports.ChangeStateOpen {
		return fmt.Errorf("fake: change %s is %s: %w", ref, c.state, ports.ErrConflict)
	}
	f.tick()
	c.state = ports.ChangeStateClosed
	f.hostEvent(ref, ports.ChangeClosed{Actor: as})
	return nil
}

// ReopenChange reopens a change closed without merging, as the actor.
func (f *Fake) ReopenChange(as ports.Actor, ref ports.ChangeRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	if c.state != ports.ChangeStateClosed {
		return fmt.Errorf("fake: change %s is %s: %w", ref, c.state, ports.ErrConflict)
	}
	f.tick()
	c.state = ports.ChangeStateOpen
	f.hostEvent(ref, ports.ChangeReopened{})
	return nil
}

// Replies returns the records in the thread's comments, or in the change's
// conversation comments when tref is the change's own ref (D3).
func (f *Fake) Replies(ref ports.ChangeRef, tref ports.ThreadRef) ([]ports.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return nil, err
	}
	place := c.comments
	if tref != ports.ThreadRef(ref) {
		t, err := f.thread(ref, tref)
		if err != nil {
			return nil, err
		}
		place = t.comments
	}
	var out []ports.Record
	for _, cm := range place {
		if cm.rec != nil {
			out = append(out, cm.record())
		}
	}
	return out, nil
}

// CreateTag creates a tag at the base tip as the actor (§9.8 step 1, I10).
func (f *Fake) CreateTag(as ports.Actor, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.canWrite(as) || (f.enforced(RulesetTags) && !f.isAdmin(as)) {
		return refused(as, "create tag "+name)
	}
	if _, ok := f.tags[name]; ok {
		return fmt.Errorf("fake: tag %s exists: %w", name, ports.ErrConflict)
	}
	f.tick()
	f.tags[name] = f.branches[f.cfg.BaseBranch]
	return nil
}

// SetGate reports a gate result on a commit, as a check run would.
func (f *Fake) SetGate(cm ports.Commit, name string, s ports.GateState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.commits[cm]; !ok {
		return fmt.Errorf("fake: commit %s: %w", cm, ports.ErrNotFound)
	}
	f.tick()
	if f.gates[cm] == nil {
		f.gates[cm] = map[string]ports.GateState{}
	}
	f.gates[cm][name] = s
	for _, ref := range f.changeOrder {
		if c := f.changes[ref]; c.state == ports.ChangeStateOpen && f.head(c) == cm {
			f.hostEvent(ref, ports.GateChanged{Commit: cm})
		}
	}
	return nil
}

// RequireApprovals sets the approving reviews the base branch requires.
func (f *Fake) RequireApprovals(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.RequiredApprovals = n
	return nil
}

// SetRuleset changes one of the rulesets (RulesetLoopBranches,
// RulesetBaseBranch, RulesetTags) with RulesetChange, RulesetDisable,
// RulesetEnable, RulesetRemove, RulesetRestore or RulesetAddBypass.
func (f *Fake) SetRuleset(name, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.setRuleset(name, action) {
		return fmt.Errorf("fake: ruleset %s: can't %s: %w", name, action, ports.ErrConflict)
	}
	return nil
}

// AddAdmin gives the account admin rights on the repository.
func (f *Fake) AddAdmin(a ports.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isAdmin(a) {
		return nil
	}
	now := f.tick()
	ad := f.admins[a.ID]
	if ad == nil {
		ad = &admin{actor: a}
		f.admins[a.ID] = ad
	}
	ad.iv = append(ad.iv, interval{from: now})
	return nil
}

// RemoveAdmin takes admin rights away from an account that isn't an owner.
func (f *Fake) RemoveAdmin(a ports.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ad := f.admins[a.ID]
	if ad == nil || !ad.iv[len(ad.iv)-1].to.IsZero() {
		return fmt.Errorf("fake: %s isn't an added admin: %w", a.Login, ports.ErrNotFound)
	}
	ad.iv[len(ad.iv)-1].to = f.tick()
	return nil
}

// StartCoderRun records a coder run for the item, which holds a coder push
// token until end is called (I11, D4). The worker's base merge is one too.
func (f *Fake) StartCoderRun(item ports.ItemRef) (end func(), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &coderRun{item: item, iv: interval{from: f.tick()}}
	f.runs = append(f.runs, r)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.iv.to.IsZero() {
			r.iv.to = f.tick()
		}
	}, nil
}

// BranchHead returns the branch's tip, or ErrNotFound. Like the methods
// below, it inspects the sandbox the way `git ls-remote` would.
func (f *Fake) BranchHead(branch string) (ports.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.branches[branch]
	if !ok {
		return "", fmt.Errorf("fake: branch %s: %w", branch, ports.ErrNotFound)
	}
	return c, nil
}

// IsAncestor reports whether a is reachable from b (`git merge-base
// --is-ancestor`).
func (f *Fake) IsAncestor(a, b ports.Commit) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reachable(b)[a]
}

// Parents returns the commit's parents, or ErrNotFound.
func (f *Fake) Parents(c ports.Commit) ([]ports.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cm, ok := f.commits[c]
	if !ok {
		return nil, fmt.Errorf("fake: commit %s: %w", c, ports.ErrNotFound)
	}
	return append([]ports.Commit(nil), cm.parents...), nil
}
