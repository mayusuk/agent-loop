package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// planKey returns a plan key no earlier run used, so a live sandbox
// repository can run the case again.
func planKey(c string) string {
	return fmt.Sprintf("%s-%d", c, time.Now().UnixNano())
}

// planRec returns a record of type rt with key and a plan key.
func planRec(rt ports.RecordType, key ports.IdemKey, plan string) ports.Record {
	data, _ := json.Marshal(map[string]any{"type": rt, "v": 1, "key": key, "plan": plan})
	return ports.Record{Type: rt, Key: key, Data: data}
}

// proposal pushes files on a plan branch as the coder and opens the
// proposal as the worker does. It returns the proposal and its head.
func (s *suite) proposal(plan string, files map[string]string) (ports.ChangeRef, ports.Commit) {
	s.t.Helper()
	e := s.env()
	branch := e.BranchPrefix + "plan/" + plan
	var head ports.Commit
	s.coderRun(s.item(), func() {
		var err error
		head, err = e.Driver.PushFiles(e.Coder, branch, files)
		s.must(err)
	})
	k := s.key("proposal")
	ref, err := e.Host.OpenChange(s.ctx(), ports.NewChange{Branch: branch, Base: e.BaseBranch, Record: planRec(ports.RecordProposal, k, plan)}, ports.IdentityCoder, k)
	s.must(err)
	return ref, head
}

// c19: PlanAt returns the later of two merged changes to a plan, its merger
// by ID, and the plan's files at its commit. Unrelated merges and open
// changes don't move it (FR-17 AC8, FR-13 AC7).
func c19(s *suite) {
	e := s.env()
	ctx := s.ctx()
	plan := planKey("c19")
	dir := "plans/" + plan
	if _, err := e.Host.PlanAt(ctx, dir); !errors.Is(err, ports.ErrNotFound) {
		s.t.Errorf("PlanAt before any commit: %v, want ErrNotFound", err)
	}

	prop, head := s.proposal(plan, map[string]string{dir + "/plan.md": "v1\n", dir + "/tasks.yaml": "t1\n"})
	s.passGates(head)
	s.must(e.Driver.MergeChange(e.Owner, prop))
	v1, err := e.Host.PlanAt(ctx, dir)
	s.must(err)
	if v1.Change != prop || v1.MergedBy == nil || v1.MergedBy.ID != e.Owner.ID {
		s.t.Errorf("after the proposal: change %s merged by %+v, want %s merged by the owner %s", v1.Change, v1.MergedBy, prop, e.Owner.ID)
	}

	// The fix: an owner's change, merged by the loop, so its merger differs.
	fixBranch := "c19-fix-" + plan
	fixHead, err := e.Driver.PushFiles(e.Owner, fixBranch, map[string]string{dir + "/plan.md": "v2\n", dir + "/tasks/a.md": "a\n"})
	s.must(err)
	fix, err := e.Driver.OpenChange(e.Owner, "", fixBranch)
	s.must(err)
	s.passGates(fixHead)
	out, err := e.Host.Merge(ctx, fix, fixHead, ports.MergeMethodSquash, s.key("merge-fix"))
	s.must(err)
	if out != ports.MergeMerged {
		s.t.Fatalf("merge the fix: %s", out)
	}

	// A later merged change outside the plan, and an open change to it.
	it := s.item(ports.MarkerReady, ports.MarkerTypeFeature)
	other, otherHead := s.loopChange(it)
	s.passGates(otherHead)
	if out, err := e.Host.Merge(ctx, other, otherHead, ports.MergeMethodSquash, s.key("merge-other")); err != nil || out != ports.MergeMerged {
		s.t.Fatalf("merge the other change: %s %v", out, err)
	}
	openBranch := "c19-open-" + plan
	_, err = e.Driver.PushFiles(e.Owner, openBranch, map[string]string{dir + "/plan.md": "v3\n"})
	s.must(err)
	_, err = e.Driver.OpenChange(e.Owner, "", openBranch)
	s.must(err)

	v2, err := e.Host.PlanAt(ctx, dir)
	s.must(err)
	if v2.Change != fix || v2.Commit == v1.Commit || v2.Commit == "" {
		s.t.Errorf("after the fix: commit %s change %s, want a new commit from %s", v2.Commit, v2.Change, fix)
	}
	if v2.MergedBy == nil || v2.MergedBy.ID != e.Loop.ID || v2.MergedAt.IsZero() {
		s.t.Errorf("after the fix: merged by %+v at %v, want the loop %s by ID", v2.MergedBy, v2.MergedAt, e.Loop.ID)
	}
	var got []string
	for _, f := range v2.Files {
		got = append(got, fmt.Sprintf("%s=%q/%d", f.Path, untrusted.Read(f.Content), f.Size))
	}
	if want := `plan.md="v2\n"/3 tasks.yaml="t1\n"/3 tasks/a.md="a\n"/2`; strings.Join(got, " ") != want {
		s.t.Errorf("files = %s, want %s", strings.Join(got, " "), want)
	}

	// A dir that names a file, is empty or climbs out is refused (#156 D9).
	for _, bad := range []string{dir + "/plan.md", "", "/", dir + "/../x"} {
		if _, err := e.Host.PlanAt(ctx, bad); !errors.Is(err, ports.ErrConflict) {
			s.t.Errorf("PlanAt(%q): %v, want ErrConflict", bad, err)
		}
	}
}

// c20: proposals are opened once per key on a plan branch, report their
// plan key, and are refused off a plan branch or with an item.
func c20(s *suite) {
	e := s.env()
	ctx := s.ctx()
	_, cur := s.hostFeed("")
	plan := planKey("c20")
	branch := e.BranchPrefix + "plan/" + plan
	s.coderRun(s.item(), func() {
		_, err := e.Driver.PushFiles(e.Coder, branch, map[string]string{"plans/" + plan + "/plan.md": "p\n"})
		s.must(err)
	})
	k := s.key("proposal")
	n := ports.NewChange{Branch: branch, Base: e.BaseBranch, Record: planRec(ports.RecordProposal, k, plan)}
	p1, err := e.Host.OpenChange(ctx, n, ports.IdentityCoder, k)
	s.must(err)
	p2, err := e.Host.OpenChange(ctx, n, ports.IdentityCoder, k)
	s.must(err)
	if p1 != p2 {
		s.t.Errorf("OpenChange of a proposal repeated: %s and %s", p1, p2)
	}
	evs, _ := s.hostFeed(cur)
	opened := 0
	for _, ev := range evs {
		if _, ok := ev.Detail.(ports.Opened); ok {
			opened++
		}
	}
	if opened != 1 {
		s.t.Errorf("%d Opened events, want 1", opened)
	}
	if c := s.getChange(p1); c.Plan != plan || c.Item != "" {
		s.t.Errorf("proposal: plan %q item %q, want plan %q and no item", c.Plan, c.Item, plan)
	}

	it := s.item(ports.MarkerReady, ports.MarkerTypeFeature)
	ch, _ := s.loopChange(it)
	if c := s.getChange(ch); c.Plan != "" || c.Item != it {
		s.t.Errorf("item change: plan %q item %q, want no plan and item %s", c.Plan, c.Item, it)
	}
	k = s.key("off")
	off := ports.NewChange{Branch: s.getChange(ch).Branch, Base: e.BaseBranch, Record: planRec(ports.RecordProposal, k, plan)}
	if _, err := e.Host.OpenChange(ctx, off, ports.IdentityCoder, k); !errors.Is(err, ports.ErrConflict) {
		s.t.Errorf("proposal off a plan branch: %v, want ErrConflict", err)
	}
	k = s.key("item")
	withItem := ports.NewChange{Item: it, Branch: branch, Base: e.BaseBranch, Record: planRec(ports.RecordProposal, k, plan)}
	if _, err := e.Host.OpenChange(ctx, withItem, ports.IdentityCoder, k); !errors.Is(err, ports.ErrConflict) {
		s.t.Errorf("proposal with an item: %v, want ErrConflict", err)
	}
}

// c21: CreateItem finds task and plan records from Since right after the
// write (FR-17 AC3, INTEGRATION-SPEC §6.1).
func c21(s *suite) {
	e := s.env()
	ctx := s.ctx()
	since := s.getItem(s.item()).CreatedAt // stands in for the plan's merge time
	plan := planKey("c21")
	for _, rt := range []ports.RecordType{ports.RecordTask, ports.RecordPlan} {
		k := s.key(string(rt))
		n := ports.NewItem{Record: planRec(rt, k, plan), Since: since}
		if rt == ports.RecordTask {
			n.Markers = []ports.Marker{ports.MarkerReady, ports.MarkerTypeFeature}
		} else {
			n.Markers = []ports.Marker{ports.MarkerPlan}
		}
		i1, err := e.Tracker.CreateItem(ctx, n, k)
		s.must(err)
		i2, err := e.Tracker.CreateItem(ctx, n, k)
		s.must(err)
		if i1 != i2 {
			s.t.Errorf("CreateItem with a %s record repeated: %s and %s", rt, i1, i2)
		}
		k2 := s.key(string(rt) + "-other")
		n.Record = planRec(rt, k2, plan)
		i3, err := e.Tracker.CreateItem(ctx, n, k2)
		s.must(err)
		if i3 == i1 {
			s.t.Errorf("CreateItem with another %s key returned the first item %s", rt, i1)
		}
	}
}
