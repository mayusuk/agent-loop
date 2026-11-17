package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// Git implements ports.Workspace with a cached bare clone per repository
// and a fresh clone per run that borrows the cache's objects (work plan D2).
//
// Layout under Root:
//
//	repos/<key>.git   the cached clone of one repository URL
//	repos/<key>.lock  the lock that serialises fetches of it
//	runs/<slug>-<id>  one run's working tree
type Git struct {
	// Root is the directory that holds the cached clones and the runs.
	Root string
	// Auth, when set, gives the token the fetch of repo authenticates with
	// (an https URL only). Nil fetches without credentials.
	Auth func(ctx context.Context, repo ports.RepoRef) (ports.Token, error)
	// GitPath is the git binary. Empty means "git" on PATH.
	GitPath string
}

// DefaultRoot is <user cache dir>/agent-loop/workspaces (work plan D5).
func DefaultRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-loop", "workspaces"), nil
}

// New returns a workspace rooted at root, creating it. An empty root means
// DefaultRoot. The root is stored with its symlinks resolved, so the paths
// it returns are the ones a sandbox sees.
func New(root string) (*Git, error) {
	if root == "" {
		r, err := DefaultRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	for _, d := range []string{root, filepath.Join(root, "repos"), filepath.Join(root, "runs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &Git{Root: real}, nil
}

var _ ports.Workspace = (*Git)(nil)

// Prepare fetches the cached clone of repo and makes a fresh tree for one
// run, checked out as INTEGRATION-SPEC §3.3 says:
//
//   - b.Detach set: that commit, detached (review);
//   - b.Name exists on the remote: its head, on a local branch of that name;
//   - otherwise: a new branch b.Name from the tip of repo.BaseBranch.
//
// A branch's upstream is the remote branch of the same name, so a plain
// `git push` pushes it. A commit or base branch the remote doesn't have is
// ports.ErrNotFound.
func (g *Git) Prepare(ctx context.Context, repo ports.RepoRef, b ports.Branch) (ports.Dir, error) {
	if repo.URL == "" {
		return ports.Dir{}, errors.New("workspace: repository URL is empty")
	}
	if b.Detach == "" {
		if err := g.checkBranch(ctx, b.Name); err != nil {
			return ports.Dir{}, err
		}
		if err := g.checkBranch(ctx, repo.BaseBranch); err != nil {
			return ports.Dir{}, fmt.Errorf("base branch: %w", err)
		}
	} else if !shaRe.MatchString(string(b.Detach)) {
		return ports.Dir{}, fmt.Errorf("workspace: %q is not a full commit SHA", b.Detach)
	}

	// The cache's lock is held until the run's clone is made, so another
	// fetch's --prune can't remove the start point in between.
	cache, unlock, err := g.fetch(ctx, repo)
	if err != nil {
		return ports.Dir{}, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	// Pick the start point in the cache, which mirrors the remote's heads.
	var start string
	switch {
	case b.Detach != "":
		if _, err := g.git(ctx, cache, nil, "cat-file", "-e", string(b.Detach)+"^{commit}"); err != nil {
			return ports.Dir{}, fmt.Errorf("workspace: commit %s: %w", b.Detach, ports.ErrNotFound)
		}
	case g.hasRef(ctx, cache, "refs/heads/"+b.Name):
		start = "origin/" + b.Name
	case g.hasRef(ctx, cache, "refs/heads/"+repo.BaseBranch):
		start = "origin/" + repo.BaseBranch
	default:
		return ports.Dir{}, fmt.Errorf("workspace: base branch %q: %w", repo.BaseBranch, ports.ErrNotFound)
	}

	slug := b.Name
	if b.Detach != "" {
		slug = string(b.Detach)[:12]
	}
	dir, err := g.newRunDir(slug)
	if err != nil {
		return ports.Dir{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	if beforeClone != nil {
		beforeClone()
	}
	if _, err := g.git(ctx, "", nil, "clone", "--quiet", "--shared", "--no-checkout", "--origin", "origin", cache, dir); err != nil {
		return ports.Dir{}, err
	}
	locked = false
	unlock()
	if _, err := g.git(ctx, dir, nil, "remote", "set-url", "origin", repo.URL); err != nil {
		return ports.Dir{}, err
	}
	switch {
	case b.Detach != "":
		_, err = g.git(ctx, dir, nil, "checkout", "--quiet", "--detach", string(b.Detach))
	case start == "origin/"+b.Name:
		_, err = g.git(ctx, dir, nil, "checkout", "--quiet", "-B", b.Name, "--track", start)
	default:
		if _, err = g.git(ctx, dir, nil, "checkout", "--quiet", "--no-track", "-B", b.Name, start); err == nil {
			if _, err = g.git(ctx, dir, nil, "config", "branch."+b.Name+".remote", "origin"); err == nil {
				_, err = g.git(ctx, dir, nil, "config", "branch."+b.Name+".merge", "refs/heads/"+b.Name)
			}
		}
	}
	if err != nil {
		return ports.Dir{}, err
	}
	head, err := g.git(ctx, dir, nil, "rev-parse", "HEAD")
	if err != nil {
		return ports.Dir{}, err
	}
	ok = true
	d := ports.Dir{Path: dir, Head: ports.Commit(head)}
	if b.Detach == "" {
		d.Branch = b.Name
	}
	return d, nil
}

// Release deletes a run's tree. The branch, on the remote and in the cache,
// stays. A tree that is already gone is not an error; a path outside the
// workspace's runs directory is refused.
func (g *Git) Release(_ context.Context, d ports.Dir) error {
	runs := filepath.Join(g.Root, "runs")
	p := filepath.Clean(d.Path)
	if rel, err := filepath.Rel(runs, p); err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return fmt.Errorf("workspace: %q is not a run directory of %s", d.Path, runs)
	}
	return os.RemoveAll(p)
}

// beforeClone, when set by a test, runs just before the run's clone.
var beforeClone func()

var shaRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func (g *Git) checkBranch(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("workspace: branch name is empty")
	}
	if _, err := g.git(ctx, "", nil, "check-ref-format", "--branch", name); err != nil || strings.HasPrefix(name, "-") {
		return fmt.Errorf("workspace: %q is not a valid branch name", name)
	}
	return nil
}

func (g *Git) hasRef(ctx context.Context, repo, ref string) bool {
	_, err := g.git(ctx, repo, nil, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// cacheKey names a repository's cached clone: a readable part of the URL
// and a hash of the whole URL, so different URLs never share a clone.
func cacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	name := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		name = u.Host + u.Path
	}
	name = strings.TrimSuffix(name, ".git")
	name = unsafeRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if len(name) > 60 {
		name = name[len(name)-60:]
	}
	return name + "-" + hex.EncodeToString(sum[:6])
}

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (g *Git) newRunDir(slug string) (string, error) {
	slug = strings.Trim(unsafeRe.ReplaceAllString(strings.ReplaceAll(slug, "/", "-"), "-"), "-.")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(g.Root, "runs", slug+"-"+hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// fetch brings the cached clone of repo up to date with every branch on the
// remote, cloning it first if needed, and returns its path with the
// clone's lock still held: the caller releases it with unlock.
func (g *Git) fetch(ctx context.Context, repo ports.RepoRef) (string, func(), error) {
	key := cacheKey(repo.URL)
	cache := filepath.Join(g.Root, "repos", key+".git")
	unlock, err := lock(filepath.Join(g.Root, "repos", key+".lock"))
	if err != nil {
		return "", nil, err
	}
	if err := g.update(ctx, repo, cache); err != nil {
		unlock()
		return "", nil, err
	}
	return cache, unlock, nil
}

// update makes or fetches the cached clone; the caller holds its lock.
func (g *Git) update(ctx context.Context, repo ports.RepoRef, cache string) error {
	env, stop, err := g.authEnv(ctx, repo)
	if err != nil {
		return err
	}
	defer stop()
	if _, err := os.Stat(filepath.Join(cache, "HEAD")); err != nil {
		os.RemoveAll(cache)
		if _, err := g.git(ctx, "", nil, "init", "--quiet", "--bare", cache); err != nil {
			return err
		}
		for _, kv := range [][2]string{{"remote.origin.url", repo.URL}, {"gc.auto", "0"}, {"maintenance.auto", "false"}} {
			if _, err := g.git(ctx, cache, nil, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	if _, err := g.git(ctx, cache, env, "fetch", "--quiet", "--prune", "--no-tags", "origin", "+refs/heads/*:refs/heads/*"); err != nil {
		return fmt.Errorf("workspace: fetch %s: %w: %w", repo.URL, ports.ErrTransient, err)
	}
	return nil
}

// authEnv gives the fetch its token through a credential-cache daemon of
// its own (review R4, work plan D6): the token goes to the daemon on stdin
// and git asks for it over a Unix socket in a private directory, so it is
// never in a process's environment or arguments. The returned environment
// names only the helper; stop ends the daemon and removes its socket.
func (g *Git) authEnv(ctx context.Context, repo ports.RepoRef) ([]string, func(), error) {
	nop := func() {}
	u, err := url.Parse(repo.URL)
	if g.Auth == nil || err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, nop, nil
	}
	tok, err := g.Auth(ctx, repo)
	if err != nil {
		return nil, nop, fmt.Errorf("workspace: fetch token: %w", err)
	}
	return g.tokenEnv(ctx, u, tok)
}

// tokenEnv serves tok to git for u's host through a credential-cache daemon
// of its own, as authEnv describes.
func (g *Git) tokenEnv(ctx context.Context, u *url.URL, tok ports.Token) ([]string, func(), error) {
	nop := func() {}
	if tok.Secret() == "" {
		return nil, nop, nil
	}
	if strings.ContainsAny(tok.Secret(), "\n\x00") {
		return nil, nop, errors.New("workspace: fetch token: invalid characters")
	}
	// Short, for the Unix socket path limit; MkdirTemp makes it 0700.
	dir, err := os.MkdirTemp("", "alc-")
	if err != nil {
		return nil, nop, err
	}
	socket := filepath.Join(dir, "s")
	helper := []string{"credential-cache", "--socket", socket, "--timeout", "600"}
	stop := func() {
		// A background context: the daemon must go even if ctx has ended.
		g.gitIn(context.Background(), "", nil, nil, append(helper[:3:3], "exit")...)
		os.RemoveAll(dir)
	}
	in := "protocol=https\nhost=" + u.Host + "\nusername=x-access-token\npassword=" + tok.Secret() + "\n\n"
	if _, err := g.gitIn(ctx, "", nil, strings.NewReader(in), append(slices.Clone(helper), "store")...); err != nil {
		stop()
		return nil, nop, fmt.Errorf("workspace: fetch credentials: %w", err)
	}
	return []string{
		"GIT_CONFIG_COUNT=2",
		// The empty entry clears any other helper.
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=cache --socket " + shellQuote(socket),
	}, stop, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// git runs one git command in dir (none when empty) and returns its trimmed
// stdout. Git reads no system or global config and never prompts (work plan
// D6). The error holds git's stderr, never the extra environment.
func (g *Git) git(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	return g.gitIn(ctx, dir, extraEnv, nil, args...)
}

// gitIn is git with stdin.
func (g *Git) gitIn(ctx context.Context, dir string, extraEnv []string, stdin io.Reader, args ...string) (string, error) {
	bin := g.GitPath
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), extraEnv...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitEnv is the worker's environment without any GIT_ variable, plus the
// settings that keep git to the repository's own config.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
}

// lock takes an exclusive flock on path, which serialises the cached
// clone's fetches across the processes of one host.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
