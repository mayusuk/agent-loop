// Package recording wraps a WorkTracker and a CodeHost so that reads go to
// the real adapter and every write is recorded instead of sent. It is the
// write-recording adapter of `agent-loop run --dry-run` (INTEGRATION-SPEC
// §12.3).
package recording

import (
	"context"
	"fmt"
	"sync"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// Write is one write a caller made that was recorded instead of sent.
type Write struct {
	Port   string // "tracker" or "host"
	Method string // the port method, for example "Claim"
	Target string // the item or change it is on
	Key    ports.IdemKey
	Detail string // what it would have changed
}

func (w Write) String() string {
	s := fmt.Sprintf("%s.%s %s", w.Port, w.Method, w.Target)
	if w.Detail != "" {
		s += ": " + w.Detail
	}
	if w.Key != "" {
		s += " (key " + string(w.Key) + ")"
	}
	return s
}

// Log holds the recorded writes, in order. It is safe for concurrent use.
type Log struct {
	mu     sync.Mutex
	writes []Write
}

func (l *Log) add(w Write) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, w)
}

// Writes returns the writes recorded so far.
func (l *Log) Writes() []Write {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Write(nil), l.writes...)
}

// Tracker is a WorkTracker whose writes go to its Log.
type Tracker struct {
	ports.WorkTracker
	Log *Log
}

// Host is a CodeHost whose writes go to its Log.
type Host struct {
	ports.CodeHost
	Log *Log
}

var (
	_ ports.WorkTracker = Tracker{}
	_ ports.CodeHost    = Host{}
)

func (t Tracker) rec(method string, target any, key ports.IdemKey, detail string, a ...any) {
	t.Log.add(Write{Port: "tracker", Method: method, Target: fmt.Sprint(target), Key: key, Detail: fmt.Sprintf(detail, a...)})
}

func (h Host) rec(method string, target any, key ports.IdemKey, detail string, a ...any) {
	h.Log.add(Write{Port: "host", Method: method, Target: fmt.Sprint(target), Key: key, Detail: fmt.Sprintf(detail, a...)})
}

func onOff(on bool) string {
	if on {
		return "set"
	}
	return "clear"
}

// Claim records the claim and returns the expected version unchanged.
func (t Tracker) Claim(_ context.Context, ref ports.ItemRef, expect ports.Version, key ports.IdemKey) (ports.Version, error) {
	t.rec("Claim", ref, key, "add %s, remove %s", ports.MarkerInProgress, ports.MarkerReady)
	return expect, nil
}

func (t Tracker) SetMarker(_ context.Context, ref ports.ItemRef, m ports.Marker, on bool, key ports.IdemKey) error {
	t.rec("SetMarker", ref, key, "%s %s", onOff(on), m)
	return nil
}

func (t Tracker) Comment(_ context.Context, ref ports.ItemRef, rec ports.Record, _ ports.UntrustedText, key ports.IdemKey) (ports.CommentRef, error) {
	t.rec("Comment", ref, key, "%s record", rec.Type)
	return "", nil
}

func (t Tracker) CreateItem(_ context.Context, n ports.NewItem, key ports.IdemKey) (ports.ItemRef, error) {
	t.rec("CreateItem", "-", key, "%s record, markers %v", n.Record.Type, n.Markers)
	return "", nil
}

func (t Tracker) AddDependency(_ context.Context, item, blockedBy ports.ItemRef, key ports.IdemKey) error {
	t.rec("AddDependency", item, key, "blocked by %s", blockedBy)
	return nil
}

func (t Tracker) Close(_ context.Context, ref ports.ItemRef, reason ports.CloseReason, key ports.IdemKey) error {
	t.rec("Close", ref, key, "as %s", reason)
	return nil
}

func (h Host) OpenChange(_ context.Context, n ports.NewChange, as ports.Identity, key ports.IdemKey) (ports.ChangeRef, error) {
	h.rec("OpenChange", n.Item, key, "from %s as %s", n.Branch, as)
	return "", nil
}

func (h Host) SetLabel(_ context.Context, ref ports.ChangeRef, m ports.Marker, on bool, key ports.IdemKey) error {
	h.rec("SetLabel", ref, key, "%s %s", onOff(on), m)
	return nil
}

func (h Host) PostReview(_ context.Context, ref ports.ChangeRef, r ports.ReviewPost, key ports.IdemKey) (ports.ReviewRef, error) {
	h.rec("PostReview", ref, key, "on %s, approve %t", r.Commit, r.Approve)
	return "", nil
}

func (h Host) ResolveFinding(_ context.Context, ref ports.ChangeRef, f ports.FindingID, key ports.IdemKey) error {
	h.rec("ResolveFinding", ref, key, "%s", f)
	return nil
}

func (h Host) PostReply(_ context.Context, ref ports.ChangeRef, to ports.ThreadRef, rec ports.Record, _ ports.UntrustedText, key ports.IdemKey) error {
	h.rec("PostReply", ref, key, "%s record in thread %s", rec.Type, to)
	return nil
}

// Merge records the merge and reports it as not merged.
func (h Host) Merge(_ context.Context, ref ports.ChangeRef, pinned ports.Commit, m ports.MergeMethod, key ports.IdemKey) (ports.MergeOutcome, error) {
	h.rec("Merge", ref, key, "%s at %s", m, pinned)
	return ports.MergeFailed, nil
}
