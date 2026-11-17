package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

func TestRemoteHead(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	got, err := g.RemoteHead(ctx, repo(r), "agent/42")
	if err != nil || string(got) != r.branch {
		t.Fatalf("RemoteHead = %s, %v; want %s", got, err, r.branch)
	}
	if _, err := g.RemoteHead(ctx, repo(r), "agent/none"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing branch: %v, want ErrNotFound", err)
	}
	if _, err := g.RemoteHead(ctx, repo(r), "-x"); err == nil {
		t.Fatal("a branch starting with - was accepted")
	}
}

// TestFR5AC6_CleanBaseMerge merges a moved base into the branch with no
// working tree, makes one merge commit as the coder and pushes it as a
// fast-forward (INTEGRATION-SPEC §5.3).
func TestFR5AC6_CleanBaseMerge(t *testing.T) {
	r := newRemote(t)
	base := r.commit(t, "c.txt", "base moved")
	run(t, r.work, "push", "--quiet", "origin", "main")
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42"})
	if err != nil {
		t.Fatal(err)
	}
	if fh, _ := g.FetchedHead(ctx, d, "main"); string(fh) != base {
		t.Fatalf("FetchedHead(main) = %s, want %s", fh, base)
	}
	tree, conflicts, err := g.MergeTree(ctx, d, d.Head, ports.Commit(base))
	if err != nil || len(conflicts) != 0 || tree == "" {
		t.Fatalf("MergeTree = %q, %v, %v", tree, conflicts, err)
	}
	sig := Signature{Name: "project-coder[bot]", Email: "9001+project-coder[bot]@users.noreply.github.com"}
	mc, err := g.CommitMerge(ctx, d, tree, []ports.Commit{d.Head, ports.Commit(base)}, "Merge main into agent/42", sig)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, d.Path, "log", "-1", "--format=%an <%ae> %cn", string(mc)); got != sig.Name+" <"+sig.Email+"> "+sig.Name {
		t.Errorf("author and committer = %q", got)
	}
	if err := g.Push(ctx, d, repo(r), mc, "agent/42", ports.Token{}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, r.bare, "rev-parse", "refs/heads/agent/42"); got != string(mc) {
		t.Fatalf("remote head = %s, want %s", got, mc)
	}
	ps, err := g.Parents(ctx, d, mc)
	if err != nil || !reflect.DeepEqual(ps, []ports.Commit{d.Head, ports.Commit(base)}) {
		t.Fatalf("Parents = %v, %v", ps, err)
	}
	// A clean merge leaves nothing for the reviewer to check.
	if rp, err := g.ResolvedPaths(ctx, d, mc); err != nil || len(rp) != 0 {
		t.Fatalf("ResolvedPaths = %v, %v", rp, err)
	}
	if ok, err := g.IsAncestor(ctx, d, ports.Commit(base), mc); err != nil || !ok {
		t.Fatalf("IsAncestor(base, merge) = %v, %v", ok, err)
	}
	if ok, err := g.IsAncestor(ctx, d, mc, ports.Commit(base)); err != nil || ok {
		t.Fatalf("IsAncestor(merge, base) = %v, %v", ok, err)
	}
}

// TestPushNeverForces: a push after the branch moved is refused as
// ErrConflict, and the remote keeps its head (INTEGRATION-SPEC §5.3 step 2).
func TestPushNeverForces(t *testing.T) {
	r := newRemote(t)
	r.commit(t, "c.txt", "base moved")
	run(t, r.work, "push", "--quiet", "origin", "main")
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42"})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := g.FetchedHead(ctx, d, "main")
	tree, _, _ := g.MergeTree(ctx, d, d.Head, base)
	mc, err := g.CommitMerge(ctx, d, tree, []ports.Commit{d.Head, base}, "m", Signature{Name: "c", Email: "c@x"})
	if err != nil {
		t.Fatal(err)
	}
	// Someone pushes to the branch in between.
	run(t, r.work, "checkout", "--quiet", "agent/42")
	moved := r.commit(t, "d.txt", "owner push")
	run(t, r.work, "push", "--quiet", "origin", "agent/42")
	if err := g.Push(ctx, d, repo(r), mc, "agent/42", ports.Token{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("Push = %v, want ErrConflict", err)
	}
	if got := run(t, r.bare, "rev-parse", "refs/heads/agent/42"); got != moved {
		t.Fatalf("remote head = %s, want %s", got, moved)
	}
}

func TestMergeTreeConflictsAndResolvedPaths(t *testing.T) {
	r := newRemote(t)
	// The base and the branch both change a.txt.
	r.commit(t, "a.txt", "base side")
	run(t, r.work, "push", "--quiet", "origin", "main")
	run(t, r.work, "checkout", "--quiet", "agent/42")
	r.commit(t, "a.txt", "branch side")
	run(t, r.work, "push", "--quiet", "origin", "agent/42")
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42"})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := g.FetchedHead(ctx, d, "main")
	_, conflicts, err := g.MergeTree(ctx, d, d.Head, base)
	if err != nil || !reflect.DeepEqual(conflicts, []string{"a.txt"}) {
		t.Fatalf("conflicts = %v, %v; want [a.txt]", conflicts, err)
	}
	// A handler resolves the conflict, and also edits b.txt in the merge.
	g.gitCode(ctx, d.Path, []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"},
		"merge", "--quiet", "--no-commit", "origin/main") // conflicts, by design
	os.WriteFile(filepath.Join(d.Path, "a.txt"), []byte("resolved\n"), 0o644)
	os.WriteFile(filepath.Join(d.Path, "b.txt"), []byte("sneaky\n"), 0o644)
	run(t, d.Path, "add", "a.txt", "b.txt")
	run(t, d.Path, "commit", "--quiet", "-m", "merge")
	head := ports.Commit(run(t, d.Path, "rev-parse", "HEAD"))
	rp, err := g.ResolvedPaths(ctx, d, head)
	if err != nil || !reflect.DeepEqual(rp, []string{"a.txt", "b.txt"}) {
		t.Fatalf("ResolvedPaths = %v, %v; want [a.txt b.txt]", rp, err)
	}
	// A commit with one parent has none.
	if rp, err := g.ResolvedPaths(ctx, d, base); err != nil || rp != nil {
		t.Fatalf("ResolvedPaths(one parent) = %v, %v", rp, err)
	}
}

func TestFileLines(t *testing.T) {
	r := newRemote(t)
	os.WriteFile(filepath.Join(r.work, "n.txt"), []byte("1\n2\n3"), 0o644)
	os.MkdirAll(filepath.Join(r.work, "dir"), 0o755)
	os.WriteFile(filepath.Join(r.work, "dir", "x"), []byte("x\n"), 0o644)
	run(t, r.work, "add", "n.txt", "dir")
	run(t, r.work, "commit", "--quiet", "-m", "n")
	head := ports.Commit(run(t, r.work, "rev-parse", "HEAD"))
	g := newGit(t)
	d := ports.Dir{Path: r.work}
	ctx := context.Background()
	if n, err := g.FileLines(ctx, d, head, "n.txt"); err != nil || n != 3 {
		t.Fatalf("FileLines(n.txt) = %d, %v; want 3", n, err)
	}
	if n, err := g.FileLines(ctx, d, head, "a.txt"); err != nil || n != 1 {
		t.Fatalf("FileLines(a.txt) = %d, %v; want 1", n, err)
	}
	for _, p := range []string{"missing.txt", "dir"} {
		if _, err := g.FileLines(ctx, d, head, p); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("FileLines(%s) = %v, want ErrNotFound", p, err)
		}
	}
}
