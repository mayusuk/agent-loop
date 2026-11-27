package github_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// mergeFix pushes files on branch as the owner, opens a pull request and
// merges it as the loop with method m. It returns the change and the head.
func (p *stubProject) mergeFix(t *testing.T, branch string, files map[string]string, m ports.MergeMethod) (ports.ChangeRef, ports.Commit) {
	t.Helper()
	head, err := p.driver.PushFiles(actorOf(p.owner), branch, files)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.driver.OpenChange(actorOf(p.owner), "", branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.driver.SetGate(head, "ci", ports.GatePass); err != nil {
		t.Fatal(err)
	}
	out, err := p.host.Merge(ctx, ch, head, m, ports.IdemKey("merge:"+branch))
	if err != nil || out != ports.MergeMerged {
		t.Fatalf("merge %s: %s %v", branch, out, err)
	}
	return ch, head
}

// TestFR17AC8_PlanAtAfterMergeCommitIsTheBranchCommit: after a merge-commit
// merge the commits API names the branch's own commit, and its pull request
// is found through the commit's pull requests (#156 D2, D3).
func TestFR17AC8_PlanAtAfterMergeCommitIsTheBranchCommit(t *testing.T) {
	p := newStub(t, 5)
	ch, head := p.mergeFix(t, "fix", map[string]string{"plans/p/plan.md": "v1"}, ports.MergeMethodMerge)
	v, err := p.host.PlanAt(ctx, "plans/p")
	if err != nil {
		t.Fatal(err)
	}
	if v.Commit != head || v.Change != ch || v.MergedBy == nil || v.MergedBy.ID != itoa(p.loop.ID) {
		t.Fatalf("PlanAt = %+v, want commit %s, change %s merged by the loop", v, head, ch)
	}
}

// TestFR17AC8_PlanAtDirectPushHasNoChange: a commit pushed straight to the
// base has no change and no merger (#156 D3).
func TestFR17AC8_PlanAtDirectPushHasNoChange(t *testing.T) {
	p := newStub(t, 5)
	head, err := p.driver.PushFiles(actorOf(p.owner), "main", map[string]string{"plans/p/plan.md": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.host.PlanAt(ctx, "/plans/p/")
	if err != nil {
		t.Fatal(err)
	}
	if v.Commit != head || v.Change != "" || v.MergedBy != nil {
		t.Fatalf("PlanAt = %+v, want commit %s with no change", v, head)
	}
}

// TestPlanAtFilesAndLimits: nested files by relative path, sorted; a file
// over MaxPlanFile with its size and no content; a sibling directory with
// the same prefix left out; bad directories refused (#156 D1, D4).
func TestPlanAtFilesAndLimits(t *testing.T) {
	p := newStub(t, 5)
	big := strings.Repeat("x", ports.MaxPlanFile+1)
	p.mergeFix(t, "fix", map[string]string{
		"plans/p/tasks/b.md": "b", "plans/p/plan.md": "plan", "plans/p/big.md": big, "plans/p2/plan.md": "other",
	}, ports.MergeMethodSquash)
	v, err := p.host.PlanAt(ctx, "plans/p")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range v.Files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "big.md,plan.md,tasks/b.md" {
		t.Fatalf("files = %s", got)
	}
	if v.Files[0].Size != int64(len(big)) || untrusted.Read(v.Files[0].Content) != "" {
		t.Errorf("big file: size %d, %d bytes of content", v.Files[0].Size, len(untrusted.Read(v.Files[0].Content)))
	}
	if untrusted.Read(v.Files[2].Content) != "b" {
		t.Errorf("tasks/b.md = %q", untrusted.Read(v.Files[2].Content))
	}
	for _, dir := range []string{"", "plans/../x"} {
		if _, err := p.host.PlanAt(ctx, dir); !errors.Is(err, ports.ErrConflict) {
			t.Errorf("PlanAt(%q): %v, want ErrConflict", dir, err)
		}
	}
	if _, err := p.host.PlanAt(ctx, "plans/p/plan.md"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("PlanAt on a file: %v, want ErrConflict", err)
	}
}

// TestPlanAtLeavesOutSubmodulesAndSymlinks: GitHub lists a submodule in a
// directory as a file, and fetching a symlink returns its target, which
// may lie outside the plan. Neither is a regular file at its path, so
// neither is in Files (#156 D4).
func TestPlanAtLeavesOutSubmodulesAndSymlinks(t *testing.T) {
	p := newStub(t, 5)
	p.srv.SetSubmodule("plans/p/vendor")
	p.srv.SetSymlink("plans/p/link", "README.md")
	p.mergeFix(t, "fix", map[string]string{
		"plans/p/plan.md": "plan", "plans/p/vendor": "", "plans/p/link": "../../README.md", "README.md": "outside",
	}, ports.MergeMethodSquash)
	v, err := p.host.PlanAt(ctx, "plans/p")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range v.Files {
		got = append(got, f.Path+"="+untrusted.Read(f.Content))
	}
	if strings.Join(got, ",") != "plan.md=plan" {
		t.Fatalf("files = %s, want plan.md=plan alone", strings.Join(got, ","))
	}
}

// TestPlanAtDirectoryDeleted: a merged pull request that deletes the plan
// directory is its latest version, with no files and no error (#156 D4).
func TestPlanAtDirectoryDeleted(t *testing.T) {
	p := newStub(t, 5)
	p.mergeFix(t, "add", map[string]string{"plans/p/plan.md": "v1", "README.md": "r"}, ports.MergeMethodSquash)
	owner := actorOf(p.owner)
	if _, err := p.driver.PushFiles(owner, "del", map[string]string{"README.md": "r2"}); err != nil {
		t.Fatal(err)
	}
	var cur struct {
		SHA string `json:"sha"`
	}
	if err := p.driver.Call(owner, "GET", "/contents/plans/p/plan.md?ref=del", nil, &cur); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := p.driver.Call(owner, "DELETE", "/contents/plans/p/plan.md", map[string]any{"branch": "del", "sha": cur.SHA, "message": "drop the plan"}, &out); err != nil {
		t.Fatal(err)
	}
	head := ports.Commit(out.Commit.SHA)
	del, err := p.driver.OpenChange(owner, "", "del")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.driver.SetGate(head, "ci", ports.GatePass); err != nil {
		t.Fatal(err)
	}
	if res, err := p.host.Merge(ctx, del, head, ports.MergeMethodSquash, "merge:del"); err != nil || res != ports.MergeMerged {
		t.Fatalf("merge del: %s %v", res, err)
	}
	v, err := p.host.PlanAt(ctx, "plans/p")
	if err != nil {
		t.Fatal(err)
	}
	if v.Change != del || v.Commit == "" || len(v.Files) != 0 {
		t.Fatalf("PlanAt = %+v, want change %s with no files", v, del)
	}
}

// TestFR17AC8_PlanAtSeveralChangesContainTheCommit: two merged pull
// requests contain the commit that changed the plan, and neither merge
// commit is it; the earlier merged one is reported, though the other has
// the lower number and is listed first (#156 D3).
func TestFR17AC8_PlanAtSeveralChangesContainTheCommit(t *testing.T) {
	p := newStub(t, 5)
	owner := actorOf(p.owner)
	x, err := p.driver.PushFiles(owner, "a", map[string]string{"plans/p/plan.md": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.driver.Call(owner, "POST", "/git/refs", map[string]any{"ref": "refs/heads/b", "sha": string(x)}, nil); err != nil {
		t.Fatal(err)
	}
	y, err := p.driver.PushFiles(owner, "b", map[string]string{"other.md": "o"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.driver.OpenChange(owner, "", "b")
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.driver.OpenChange(owner, "", "a")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		ch   ports.ChangeRef
		head ports.Commit
	}{{a, x}, {b, y}} {
		if err := p.driver.SetGate(m.head, "ci", ports.GatePass); err != nil {
			t.Fatal(err)
		}
		if out, err := p.host.Merge(ctx, m.ch, m.head, ports.MergeMethodMerge, ports.IdemKey("merge:"+string(m.ch))); err != nil || out != ports.MergeMerged {
			t.Fatalf("merge %s: %s %v", m.ch, out, err)
		}
	}
	v, err := p.host.PlanAt(ctx, "plans/p")
	if err != nil {
		t.Fatal(err)
	}
	if v.Commit != x || v.Change != a {
		t.Fatalf("PlanAt = commit %s change %s, want %s from %s (merged first), not %s", v.Commit, v.Change, x, a, b)
	}
}

// TestProposalRules: proposals need a plan branch, no item, a plan key,
// and the coder (#156 D5).
func TestProposalRules(t *testing.T) {
	p := newStub(t, 5)
	rec := func(k ports.IdemKey, plan string) ports.Record {
		return ports.Record{Type: ports.RecordProposal, Key: k, Data: []byte(`{"plan":"` + plan + `"}`)}
	}
	cases := []struct {
		name string
		n    ports.NewChange
		as   ports.Identity
		want error
	}{
		{"loop", ports.NewChange{Branch: "agent/plan/m", Record: rec("k1", "p")}, ports.IdentityLoop, ports.ErrPermission},
		{"item", ports.NewChange{Item: "7", Branch: "agent/plan/m", Record: rec("k2", "p")}, ports.IdentityCoder, ports.ErrConflict},
		{"branch", ports.NewChange{Branch: "agent/7", Record: rec("k3", "p")}, ports.IdentityCoder, ports.ErrConflict},
		{"bare prefix", ports.NewChange{Branch: "agent/plan/", Record: rec("k4", "p")}, ports.IdentityCoder, ports.ErrConflict},
		{"no plan", ports.NewChange{Branch: "agent/plan/m", Record: rec("k5", "")}, ports.IdentityCoder, ports.ErrConflict},
	}
	for _, c := range cases {
		if _, err := p.host.OpenChange(ctx, c.n, c.as, c.n.Record.Key); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}
