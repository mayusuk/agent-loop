package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"slices"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// The git operations the worker makes itself, outside any handler: the
// checks on a handler's result and the agent-free base update
// (INTEGRATION-SPEC §5.2, §5.3, work plan #20). Each one works in a run's
// tree from Prepare, whose origin is the repository URL and whose
// origin/<branch> refs are the remote's heads at the fetch.

// Signature is the author and committer of a commit the worker makes.
type Signature = ports.Signature

// RemoteHead returns the head of branch on the remote, fetched now with
// ls-remote. A branch the remote doesn't have is ports.ErrNotFound.
func (g *Git) RemoteHead(ctx context.Context, repo ports.RepoRef, branch string) (ports.Commit, error) {
	if err := g.checkBranch(ctx, branch); err != nil {
		return "", err
	}
	env, stop, err := g.authEnv(ctx, repo)
	if err != nil {
		return "", err
	}
	defer stop()
	out, err := g.git(ctx, "", env, "ls-remote", "--", repo.URL, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("workspace: ls-remote: %w: %w", ports.ErrTransient, err)
	}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && ref == "refs/heads/"+branch {
			return ports.Commit(sha), nil
		}
	}
	return "", fmt.Errorf("workspace: branch %s: %w", branch, ports.ErrNotFound)
}

// FetchedHead returns origin/<branch> in the run's tree: the remote's head
// of branch at the fetch Prepare made.
func (g *Git) FetchedHead(ctx context.Context, d ports.Dir, branch string) (ports.Commit, error) {
	if err := g.checkBranch(ctx, branch); err != nil {
		return "", err
	}
	out, err := g.git(ctx, d.Path, nil, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	if err != nil || out == "" {
		return "", fmt.Errorf("workspace: branch %s: %w", branch, ports.ErrNotFound)
	}
	return ports.Commit(out), nil
}

// MergeTree merges theirs into ours without a working tree
// (git merge-tree --write-tree). It returns the merged tree and the paths
// that conflicted, none for a clean merge. With conflicts the tree holds
// the conflict markers.
func (g *Git) MergeTree(ctx context.Context, d ports.Dir, ours, theirs ports.Commit) (tree string, conflicts []string, err error) {
	out, code, err := g.gitCode(ctx, d.Path, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", string(ours), string(theirs))
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	switch code {
	case 0:
		return strings.TrimSpace(lines[0]), nil, nil
	case 1:
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" && !slices.Contains(conflicts, l) {
				conflicts = append(conflicts, l)
			}
		}
		return strings.TrimSpace(lines[0]), conflicts, nil
	}
	return "", nil, fmt.Errorf("workspace: merge-tree exited %d: %s", code, strings.TrimSpace(out))
}

// CommitMerge makes a commit of tree with parents, authored and committed
// by sig, and returns it. Nothing is pushed.
func (g *Git) CommitMerge(ctx context.Context, d ports.Dir, tree string, parents []ports.Commit, msg string, sig Signature) (ports.Commit, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", string(p))
	}
	args = append(args, "-m", msg)
	env := []string{
		"GIT_AUTHOR_NAME=" + sig.Name, "GIT_AUTHOR_EMAIL=" + sig.Email,
		"GIT_COMMITTER_NAME=" + sig.Name, "GIT_COMMITTER_EMAIL=" + sig.Email,
	}
	out, err := g.git(ctx, d.Path, env, args...)
	if err != nil {
		return "", err
	}
	return ports.Commit(out), nil
}

// Push pushes commit to branch on the remote as a plain fast-forward,
// authenticated with tok on an https remote. It never forces: a push the
// remote refuses because the branch moved is ports.ErrConflict.
func (g *Git) Push(ctx context.Context, d ports.Dir, repo ports.RepoRef, commit ports.Commit, branch string, tok ports.Token) error {
	if err := g.checkBranch(ctx, branch); err != nil {
		return err
	}
	var env []string
	if u, err := url.Parse(repo.URL); err == nil && u.Scheme == "https" && u.Host != "" {
		e, stop, err := g.tokenEnv(ctx, u, tok)
		if err != nil {
			return err
		}
		defer stop()
		env = e
	}
	out, code, err := g.gitCode(ctx, d.Path, env, "push", "--porcelain", "--", repo.URL, string(commit)+":refs/heads/"+branch)
	if err != nil {
		return err
	}
	if code != 0 {
		if strings.Contains(out, "non-fast-forward") || strings.Contains(out, "fetch first") || strings.Contains(out, "[rejected]") {
			return fmt.Errorf("workspace: push to %s refused, the branch moved: %w", branch, ports.ErrConflict)
		}
		return fmt.Errorf("workspace: push to %s: %w: %s", branch, ports.ErrTransient, lastLine(out))
	}
	return nil
}

// Parents returns commit's parents in order.
func (g *Git) Parents(ctx context.Context, d ports.Dir, commit ports.Commit) ([]ports.Commit, error) {
	out, err := g.git(ctx, d.Path, nil, "rev-list", "--parents", "-n", "1", string(commit))
	if err != nil {
		return nil, fmt.Errorf("workspace: commit %s: %w", commit, ports.ErrNotFound)
	}
	f := strings.Fields(out)
	var ps []ports.Commit
	for _, p := range f[1:] {
		ps = append(ps, ports.Commit(p))
	}
	return ps, nil
}

// IsAncestor reports whether a is b or one of b's ancestors.
func (g *Git) IsAncestor(ctx context.Context, d ports.Dir, a, b ports.Commit) (bool, error) {
	out, code, err := g.gitCode(ctx, d.Path, nil, "merge-base", "--is-ancestor", string(a), string(b))
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("workspace: merge-base: %w: %s", ports.ErrNotFound, lastLine(out))
}

// FileLines returns the number of lines of the file at path in commit. A
// path that isn't a file there is ports.ErrNotFound.
func (g *Git) FileLines(ctx context.Context, d ports.Dir, commit ports.Commit, path string) (int, error) {
	obj := string(commit) + ":" + path
	if t, err := g.git(ctx, d.Path, nil, "cat-file", "-t", obj); err != nil || t != "blob" {
		return 0, fmt.Errorf("workspace: %s at %s: %w", path, commit, ports.ErrNotFound)
	}
	bin := g.GitPath
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, "cat-file", "blob", obj)
	cmd.Dir = d.Path
	cmd.Env = gitEnv()
	data, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n, nil
}

// ResolvedPaths returns, for a merge commit, the paths its own merge left
// for a reviewer to check (INTEGRATION-SPEC §5.2 address): the paths that
// conflicted when its two parents are merged without a working tree, and
// every path whose content differs from that merge. A commit that isn't a
// two-parent merge has none.
func (g *Git) ResolvedPaths(ctx context.Context, d ports.Dir, commit ports.Commit) ([]string, error) {
	ps, err := g.Parents(ctx, d, commit)
	if err != nil {
		return nil, err
	}
	if len(ps) != 2 {
		return nil, nil
	}
	tree, conflicts, err := g.MergeTree(ctx, d, ps[0], ps[1])
	if err != nil {
		return nil, err
	}
	out, err := g.git(ctx, d.Path, nil, "diff-tree", "-r", "--name-only", "--no-renames", tree, string(commit)+"^{tree}")
	if err != nil {
		return nil, err
	}
	paths := slices.Clone(conflicts)
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

// gitCode runs git and returns its combined output and exit code. Only a
// failure to start git is an error.
func (g *Git) gitCode(ctx context.Context, dir string, extraEnv []string, args ...string) (string, int, error) {
	bin := g.GitPath
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), extraEnv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out.String(), 0, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
