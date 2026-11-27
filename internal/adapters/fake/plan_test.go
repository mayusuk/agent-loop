package fake_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

func proposalRec(k ports.IdemKey, plan string) ports.Record {
	return ports.Record{Type: ports.RecordProposal, Key: k, Data: []byte(`{"type":"proposal","v":1,"key":"` + string(k) + `","plan":"` + plan + `"}`)}
}

// mergedFix pushes files on branch as the owner, opens a change and merges
// it as the loop with method m. It returns the change and the pushed commit.
func mergedFix(t *testing.T, f *fake.Fake, branch string, files map[string]string, m ports.MergeMethod) (ports.ChangeRef, ports.Commit) {
	t.Helper()
	head, err := f.PushFiles(fake.Owner, branch, files)
	must(t, err)
	ch, err := f.OpenChange(fake.Owner, "", branch)
	must(t, err)
	must(t, f.SetGate(head, "ci", ports.GatePass))
	out, err := f.Host().Merge(ctx, ch, head, m, ports.IdemKey("merge:"+branch))
	must(t, err)
	if out != ports.MergeMerged {
		t.Fatalf("merge %s: %s", branch, out)
	}
	return ch, head
}

// TestFR17AC8_PlanAtAfterMergeCommitIsTheChangesCommit: after a merge-commit
// merge the latest commit that changed the plan is the change's own commit,
// and the change is found from it (#156 D2, D3).
func TestFR17AC8_PlanAtAfterMergeCommitIsTheChangesCommit(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	ch, head := mergedFix(t, f, "fix", map[string]string{"plans/p/plan.md": "v1", "plans/p/tasks.yaml": "t"}, ports.MergeMethodMerge)
	v, err := f.Host().PlanAt(ctx, "plans/p")
	must(t, err)
	if v.Commit != head || v.Change != ch || v.MergedBy == nil || v.MergedBy.ID != fake.Loop.ID {
		t.Fatalf("PlanAt = %+v, want commit %s, change %s merged by the loop", v, head, ch)
	}
}

// TestFR17AC8_PlanAtDirectPushHasNoChange: a commit no merged change
// contains has no change and no merger, so the core can't count it as
// owner-merged (#156 D3).
func TestFR17AC8_PlanAtDirectPushHasNoChange(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	head, err := f.PushFiles(fake.Owner, "main", map[string]string{"plans/p/plan.md": "v1"})
	must(t, err)
	v, err := f.Host().PlanAt(ctx, "plans/p/")
	must(t, err)
	if v.Commit != head || v.Change != "" || v.MergedBy != nil {
		t.Fatalf("PlanAt = %+v, want commit %s with no change", v, head)
	}
}

// TestPlanAtFilesAndLimits: nested files are returned by relative path,
// sorted; a file over MaxPlanFile has its size and no content; a file
// outside the directory or in a sibling with the same prefix is left out
// (#156 D4).
func TestPlanAtFilesAndLimits(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	big := strings.Repeat("x", ports.MaxPlanFile+1)
	mergedFix(t, f, "fix", map[string]string{
		"plans/p/tasks/b.md": "b", "plans/p/plan.md": "plan", "plans/p/big.md": big,
		"plans/p2/plan.md": "other", "README.md": "r",
	}, ports.MergeMethodSquash)
	v, err := f.Host().PlanAt(ctx, "plans/p")
	must(t, err)
	var paths []string
	for _, pf := range v.Files {
		paths = append(paths, pf.Path)
	}
	if got := strings.Join(paths, ","); got != "big.md,plan.md,tasks/b.md" {
		t.Fatalf("files = %s", got)
	}
	if v.Files[0].Size != int64(len(big)) || untrusted.Read(v.Files[0].Content) != "" {
		t.Errorf("big file: size %d, content %d bytes; want its size and no content", v.Files[0].Size, len(untrusted.Read(v.Files[0].Content)))
	}
	if untrusted.Read(v.Files[2].Content) != "b" || v.Files[2].Size != 1 {
		t.Errorf("tasks/b.md = %q (%d)", untrusted.Read(v.Files[2].Content), v.Files[2].Size)
	}
	if _, err := f.Host().PlanAt(ctx, ""); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("PlanAt(\"\"): %v, want ErrConflict", err)
	}
}

// TestPlanAtDirectoryDeleted: a merged change that deletes the plan
// directory is its latest version, with no files and no error (#156 D4).
func TestPlanAtDirectoryDeleted(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	mergedFix(t, f, "add", map[string]string{"plans/p/plan.md": "v1", "README.md": "r"}, ports.MergeMethodSquash)
	head, err := f.DeleteFiles(fake.Owner, "del", "plans/p/plan.md")
	must(t, err)
	del, err := f.OpenChange(fake.Owner, "", "del")
	must(t, err)
	must(t, f.SetGate(head, "ci", ports.GatePass))
	if out, err := f.Host().Merge(ctx, del, head, ports.MergeMethodSquash, "merge:del"); err != nil || out != ports.MergeMerged {
		t.Fatalf("merge del: %s %v", out, err)
	}
	v, err := f.Host().PlanAt(ctx, "plans/p")
	must(t, err)
	if v.Change != del || v.Commit == "" || len(v.Files) != 0 {
		t.Fatalf("PlanAt = %+v, want change %s with no files", v, del)
	}
}

// TestFR17AC8_PlanAtSeveralChangesContainTheCommit: two changes contain
// the commit that changed the plan; the one merged first brought it in and
// is reported, even though the other was opened first (#156 D3).
func TestFR17AC8_PlanAtSeveralChangesContainTheCommit(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	x, err := f.PushFiles(fake.Owner, "a", map[string]string{"plans/p/plan.md": "v1"})
	must(t, err)
	f.BranchFrom("b", "a")
	y, err := f.PushFiles(fake.Owner, "b", map[string]string{"other.md": "o"})
	must(t, err)
	b, err := f.OpenChange(fake.Owner, "", "b")
	must(t, err)
	a, err := f.OpenChange(fake.Owner, "", "a")
	must(t, err)
	merge := func(ch ports.ChangeRef, head ports.Commit) {
		t.Helper()
		must(t, f.SetGate(head, "ci", ports.GatePass))
		if out, err := f.Host().Merge(ctx, ch, head, ports.MergeMethodMerge, ports.IdemKey("merge:"+string(ch))); err != nil || out != ports.MergeMerged {
			t.Fatalf("merge %s: %s %v", ch, out, err)
		}
	}
	merge(a, x)
	// The fake wants b up to date with the base before it merges.
	y, err = f.MergeBaseInto(fake.Owner, "b")
	must(t, err)
	merge(b, y)
	v, err := f.Host().PlanAt(ctx, "plans/p")
	must(t, err)
	if v.Commit != x || v.Change != a {
		t.Fatalf("PlanAt = commit %s change %s, want %s from %s (merged first), not %s", v.Commit, v.Change, x, a, b)
	}
}

// TestProposalRules: proposals are the coder's, off an item, on a plan
// branch, with a plan key (#156 D5).
func TestProposalRules(t *testing.T) {
	f := fake.New(fake.DefaultConfig())
	end, err := f.StartCoderRun("7")
	must(t, err)
	_, err = f.Push(fake.Coder, "agent/plan/mint-1")
	must(t, err)
	_, err = f.Push(fake.Coder, "agent/7")
	must(t, err)
	end()
	cases := []struct {
		name string
		n    ports.NewChange
		as   ports.Identity
		want error
	}{
		{"loop", ports.NewChange{Branch: "agent/plan/mint-1", Record: proposalRec("k1", "p")}, ports.IdentityLoop, ports.ErrPermission},
		{"item", ports.NewChange{Item: "7", Branch: "agent/plan/mint-1", Record: proposalRec("k2", "p")}, ports.IdentityCoder, ports.ErrConflict},
		{"branch", ports.NewChange{Branch: "agent/7", Record: proposalRec("k3", "p")}, ports.IdentityCoder, ports.ErrConflict},
		{"bare prefix", ports.NewChange{Branch: "agent/plan/", Record: proposalRec("k4", "p")}, ports.IdentityCoder, ports.ErrConflict},
		{"no plan", ports.NewChange{Branch: "agent/plan/mint-1", Record: proposalRec("k5", "")}, ports.IdentityCoder, ports.ErrConflict},
	}
	for _, c := range cases {
		if _, err := f.Host().OpenChange(ctx, c.n, c.as, c.n.Record.Key); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	ref, err := f.Host().OpenChange(ctx, ports.NewChange{Branch: "agent/plan/mint-1", Record: proposalRec("ok", "p")}, ports.IdentityCoder, "ok")
	must(t, err)
	ch, err := f.Host().Change(ctx, ref)
	must(t, err)
	if ch.Plan != "p" || ch.Item != "" {
		t.Errorf("proposal change: plan %q item %q", ch.Plan, ch.Item)
	}
}
