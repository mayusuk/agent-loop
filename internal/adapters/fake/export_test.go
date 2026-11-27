package fake

import "github.com/mayusuk/agent-loop/internal/ports"

// PusherOf returns the pusher the fake attributes to commit c, the same
// value a HeadMoved event for it would carry. Tests only: it lets a test
// check a push that no open change reports.
func (f *Fake) PusherOf(c ports.Commit) ports.Pusher {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attribute(f.commits[c])
}

// DeleteFiles pushes a commit to branch that deletes paths, creating the
// branch from the base tip if it doesn't exist. Tests only.
func (f *Fake) DeleteFiles(as ports.Actor, branch string, paths ...string) (ports.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.push(as, branch, nil, func(fs map[string]string) {
		for _, p := range paths {
			delete(fs, p)
		}
	})
}

// BranchFrom points a new branch at from's tip. Tests only.
func (f *Fake) BranchFrom(branch, from string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches[branch] = f.branches[from]
}
