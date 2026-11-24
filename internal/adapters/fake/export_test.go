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
