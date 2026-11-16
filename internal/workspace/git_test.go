package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// run runs git in dir with a fixed identity and no user config.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote is a bare repository with main and agent/42, and a working clone
// to push more commits from.
type remote struct {
	bare, work   string
	main, branch string // head commits
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	root := t.TempDir()
	r := &remote{bare: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work")}
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", r.bare)
	run(t, root, "clone", "--quiet", r.bare, r.work)
	run(t, r.work, "checkout", "--quiet", "-b", "main")
	r.commit(t, "a.txt", "base")
	run(t, r.work, "push", "--quiet", "origin", "main")
	r.main = run(t, r.work, "rev-parse", "HEAD")
	run(t, r.work, "checkout", "--quiet", "-b", "agent/42")
	r.commit(t, "b.txt", "agent work")
	run(t, r.work, "push", "--quiet", "origin", "agent/42")
	r.branch = run(t, r.work, "rev-parse", "HEAD")
	run(t, r.work, "checkout", "--quiet", "main")
	return r
}

func (r *remote) commit(t *testing.T, file, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, file), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, r.work, "add", file)
	run(t, r.work, "commit", "--quiet", "-m", msg)
	return run(t, r.work, "rev-parse", "HEAD")
}

func newGit(t *testing.T) *Git {
	t.Helper()
	g, err := New(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func repo(r *remote) ports.RepoRef { return ports.RepoRef{URL: r.bare, BaseBranch: "main"} }

func TestPrepareNewBranchStartsFromBaseTip(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/7"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Branch != "agent/7" || string(d.Head) != r.main {
		t.Fatalf("dir = %+v, want agent/7 at %s", d, r.main)
	}
	if got := run(t, d.Path, "symbolic-ref", "--short", "HEAD"); got != "agent/7" {
		t.Errorf("checked out %q", got)
	}
	// The upstream is the remote branch of the same name, so a plain push
	// creates it, and the remote is the repository's URL, not the cache.
	if got := run(t, d.Path, "remote", "get-url", "origin"); got != r.bare {
		t.Errorf("origin = %q, want %q", got, r.bare)
	}
	if got := run(t, d.Path, "config", "branch.agent/7.merge"); got != "refs/heads/agent/7" {
		t.Errorf("upstream = %q", got)
	}
	os.WriteFile(filepath.Join(d.Path, "c.txt"), []byte("c\n"), 0o644)
	run(t, d.Path, "add", "c.txt")
	run(t, d.Path, "commit", "--quiet", "-m", "c")
	run(t, d.Path, "push", "--quiet")
	want := run(t, d.Path, "rev-parse", "HEAD")
	if got := run(t, r.bare, "rev-parse", "refs/heads/agent/7"); got != want {
		t.Errorf("remote agent/7 = %s, want %s", got, want)
	}
	// Nothing was written to the cache by the run's commit (work plan D2).
	cache := filepath.Join(g.Root, "repos", cacheKey(r.bare)+".git")
	if _, err := exec.Command("git", "-C", cache, "cat-file", "-e", want).CombinedOutput(); err == nil {
		t.Error("the run's commit is in the cached clone")
	}
	if err := g.Release(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
		t.Errorf("tree still there: %v", err)
	}
}

func TestPrepareExistingBranchChecksOutItsHead(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42"})
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Head) != r.branch || d.Branch != "agent/42" {
		t.Fatalf("dir = %+v, want agent/42 at %s", d, r.branch)
	}
	g.Release(ctx, d)

	// The cache is fetched again before each run: a new push is seen.
	run(t, r.work, "checkout", "--quiet", "agent/42")
	newHead := r.commit(t, "b.txt", "more")
	run(t, r.work, "push", "--quiet", "origin", "agent/42")
	d, err = g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release(ctx, d)
	if string(d.Head) != newHead {
		t.Errorf("head = %s, want the new push %s", d.Head, newHead)
	}
}

// TestPrepareNextBranchAfterClosedChange: after the owner closes a change,
// implement runs on the next branch name from the base tip, and the old
// branch stays as it is (INTEGRATION-SPEC §3.3, D44).
func TestPrepareNextBranchAfterClosedChange(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release(ctx, d)
	if string(d.Head) != r.main {
		t.Errorf("head = %s, want base tip %s", d.Head, r.main)
	}
	if got := run(t, r.bare, "rev-parse", "refs/heads/agent/42"); got != r.branch {
		t.Errorf("old branch moved to %s", got)
	}
}

func TestPrepareDetachedForReview(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	d, err := g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42", Detach: ports.Commit(r.main)})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release(ctx, d)
	if d.Branch != "" || string(d.Head) != r.main {
		t.Fatalf("dir = %+v, want detached at %s", d, r.main)
	}
	cmd := exec.Command("git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = d.Path
	if err := cmd.Run(); err == nil {
		t.Error("HEAD is on a branch, want detached")
	}
}

func TestPrepareErrors(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	missing := strings.Repeat("ab", 20)
	if _, err := g.Prepare(ctx, repo(r), ports.Branch{Detach: ports.Commit(missing)}); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("unknown commit: %v, want ErrNotFound", err)
	}
	if _, err := g.Prepare(ctx, ports.RepoRef{URL: r.bare, BaseBranch: "nope"}, ports.Branch{Name: "agent/1"}); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("unknown base: %v, want ErrNotFound", err)
	}
	for _, name := range []string{"", "a..b", "-x", "a b", "agent/"} {
		if _, err := g.Prepare(ctx, repo(r), ports.Branch{Name: name}); err == nil {
			t.Errorf("branch %q accepted", name)
		}
	}
	if _, err := g.Prepare(ctx, repo(r), ports.Branch{Detach: "HEAD"}); err == nil {
		t.Error("a detach that isn't a SHA was accepted")
	}
	if _, err := g.Prepare(ctx, ports.RepoRef{URL: filepath.Join(t.TempDir(), "none.git"), BaseBranch: "main"}, ports.Branch{Name: "agent/1"}); err == nil || !errors.Is(err, ports.ErrTransient) {
		t.Errorf("unreachable remote: %v, want ErrTransient", err)
	}
	if _, err := g.Prepare(ctx, ports.RepoRef{BaseBranch: "main"}, ports.Branch{Name: "agent/1"}); err == nil {
		t.Error("empty URL accepted")
	}
	// No run directory is left behind by a failed Prepare.
	entries, _ := os.ReadDir(filepath.Join(g.Root, "runs"))
	if len(entries) != 0 {
		t.Errorf("runs left behind: %v", entries)
	}
}

func TestReleaseRefusesPathsOutsideRuns(t *testing.T) {
	g := newGit(t)
	ctx := context.Background()
	outside := t.TempDir()
	for _, p := range []string{outside, g.Root, filepath.Join(g.Root, "runs"), filepath.Join(g.Root, "repos", "x.git"), filepath.Join(g.Root, "runs", "..", "repos"), filepath.Join(g.Root, "runs", "a", "b")} {
		if err := g.Release(ctx, ports.Dir{Path: p}); err == nil {
			t.Errorf("Release(%q) accepted", p)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside dir touched: %v", err)
	}
	// An already deleted run is fine.
	if err := g.Release(ctx, ports.Dir{Path: filepath.Join(g.Root, "runs", "gone-1")}); err != nil {
		t.Errorf("Release of a deleted run: %v", err)
	}
}

func TestConcurrentPreparesShareTheCache(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	dirs := make([]ports.Dir, 4)
	errs := make([]error, 4)
	for i := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dirs[i], errs[i] = g.Prepare(ctx, repo(r), ports.Branch{Name: "agent/42"})
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, d := range dirs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if seen[d.Path] || string(d.Head) != r.branch {
			t.Errorf("dir %d = %+v", i, d)
		}
		seen[d.Path] = true
		g.Release(ctx, d)
	}
	repos, _ := filepath.Glob(filepath.Join(g.Root, "repos", "*.git"))
	if len(repos) != 1 {
		t.Errorf("cached clones = %v, want one", repos)
	}
}

func TestFetchTokenGoesInTheEnvironmentNotTheArguments(t *testing.T) {
	g := &Git{Auth: func(context.Context, ports.RepoRef) (ports.Token, error) {
		return ports.NewToken("ghs_secret", ports.IdentityCoder, ports.Scope{}, time.Now().Add(time.Hour)), nil
	}}
	env, err := g.authEnv(context.Background(), ports.RepoRef{URL: "https://github.com/o/r"})
	if err != nil {
		t.Fatal(err)
	}
	want := "GIT_CONFIG_VALUE_0=Authorization: Basic eC1hY2Nlc3MtdG9rZW46Z2hzX3NlY3JldA=="
	if len(env) != 3 || env[2] != want {
		t.Errorf("env = %q", env)
	}
	// No token for a non-https remote.
	if env, _ := g.authEnv(context.Background(), ports.RepoRef{URL: "/local/repo"}); env != nil {
		t.Errorf("env for a local remote = %q", env)
	}
	g.Auth = func(context.Context, ports.RepoRef) (ports.Token, error) { return ports.Token{}, ports.ErrPermission }
	if _, err := g.authEnv(context.Background(), ports.RepoRef{URL: "https://github.com/o/r"}); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("auth error = %v", err)
	}
}

func TestCacheKey(t *testing.T) {
	a := cacheKey("https://github.com/o/r")
	b := cacheKey("https://github.com/o/r.git")
	if a == b || !strings.HasPrefix(a, "github.com-o-r-") {
		t.Errorf("keys %q, %q", a, b)
	}
	if k := cacheKey("/tmp/x y/../origin.git"); strings.ContainsAny(k, "/ ") {
		t.Errorf("key %q has unsafe characters", k)
	}
}

// TestCacheLockIsHeldUntilTheRunsCloneIsDone checks that a concurrent fetch
// (whose --prune could remove the start branch) can't run between picking
// the start point and cloning from the cache (review R5).
func TestCacheLockIsHeldUntilTheRunsCloneIsDone(t *testing.T) {
	r := newRemote(t)
	g := newGit(t)
	lockPath := filepath.Join(g.Root, "repos", cacheKey(r.bare)+".lock")
	var held []bool
	beforeClone = func() {
		f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		held = append(held, errors.Is(err, syscall.EWOULDBLOCK))
		if err == nil {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
	}
	t.Cleanup(func() { beforeClone = nil })
	d, err := g.Prepare(context.Background(), repo(r), ports.Branch{Name: "agent/42"})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || !held[0] {
		t.Errorf("lock held at the clone = %v, want [true]", held)
	}
	// And it is released once Prepare returns.
	if _, err := g.Prepare(context.Background(), repo(r), ports.Branch{Name: "agent/42"}); err != nil {
		t.Fatal(err)
	}
	g.Release(context.Background(), d)
}
