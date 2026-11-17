package githubtest

import (
	"context"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// RunLog is an in-memory github.RunLog: the worker's coder-run records, as
// the conformance driver starts and ends them.
type RunLog struct {
	Now func() time.Time // time.Now when nil

	mu   sync.Mutex
	runs []github.CoderRun
}

var _ github.RunLog = (*RunLog)(nil)

func (l *RunLog) now() time.Time {
	if l.Now == nil {
		return time.Now()
	}
	return l.Now()
}

// Start records a live coder run for item, and returns the function that
// ends it.
func (l *RunLog) Start(item ports.ItemRef) (end func()) {
	endAt := l.StartWithHead(item)
	return func() { endAt("") }
}

// StartWithHead records a live coder run for item, and returns the
// function that ends it with the run's head: its done.head, or the branch
// head after failed (INTEGRATION-SPEC §9.8 step 6). An empty head records
// none.
func (l *RunLog) StartWithHead(item ports.ItemRef) (end func(head ports.Commit)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := len(l.runs)
	l.runs = append(l.runs, github.CoderRun{Item: item, Start: l.now()})
	return func(head ports.Commit) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.runs[i].End.IsZero() {
			l.runs[i].End, l.runs[i].Head = l.now(), head
		}
	}
}

// CoderRuns returns the runs live at any time in [from, to].
func (l *RunLog) CoderRuns(_ context.Context, from, to time.Time) ([]github.CoderRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []github.CoderRun
	for _, r := range l.runs {
		if !r.Start.After(to) && (r.End.IsZero() || !r.End.Before(from)) {
			out = append(out, r)
		}
	}
	return out, nil
}
