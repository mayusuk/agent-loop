package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// hostCursor is the change feed's position (work plan D16): the window
// start, and per pull request still open or updated in the window what has
// been reported of it.
type hostCursor struct {
	W int64                  `json:"w"`
	P map[string]*pullCursor `json:"p,omitempty"`
}

type pullCursor struct {
	N int                  `json:"n"`           // timeline items seen
	T map[string]threadObs `json:"t,omitempty"` // threads, by node ID (work plan D9)
	G string               `json:"g,omitempty"` // the head's check digest
}

const hostCursorPrefix = "gh1h:"

func (c hostCursor) encode() ports.Cursor {
	b, _ := json.Marshal(c)
	return ports.Cursor(hostCursorPrefix + base64.RawURLEncoding.EncodeToString(b))
}

func parseHostCursor(s ports.Cursor) (hostCursor, error) {
	bad := fmt.Errorf("github: bad change-feed cursor: %w", ports.ErrConflict)
	raw, ok := strings.CutPrefix(string(s), hostCursorPrefix)
	if !ok {
		return hostCursor{}, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return hostCursor{}, bad
	}
	var c hostCursor
	if json.Unmarshal(b, &c) != nil || c.W <= 0 {
		return hostCursor{}, bad
	}
	if c.P == nil {
		c.P = map[string]*pullCursor{}
	}
	for _, p := range c.P {
		if p == nil || p.N < 0 {
			return hostCursor{}, bad
		}
	}
	return c, nil
}

// gateDigest is the head and its check contexts' states, sorted.
func gateDigest(d *prDetail) (string, bool) {
	ctxs := d.contexts()
	parts := make([]string, 0, len(ctxs))
	for _, c := range ctxs {
		if c.Typename == "CheckRun" {
			parts = append(parts, "run:"+c.Name+"="+c.Status+"/"+c.Conclusion)
		} else {
			parts = append(parts, "status:"+c.Context+"="+c.State)
		}
	}
	sort.Strings(parts)
	return d.HeadRefOID + "|" + strings.Join(parts, ","), len(parts) > 0
}

// pending is one step of a pull request's part of the feed: an event, or a
// silent advance (ev nil), and how it moves the cursor.
type pending struct {
	at    time.Time
	ev    *ports.ChangeEvent
	apply func(*pullCursor)
}

// Changes returns the change events after since, oldest first, at most one
// page (fake D2, work plan D16). An empty since returns no events and the
// current end of the feed.
func (h *Host) Changes(ctx context.Context, since ports.Cursor) ([]ports.ChangeEvent, ports.Cursor, error) {
	now := h.c.now()
	if since == "" {
		return h.feedEnd(ctx, now)
	}
	cur, err := parseHostCursor(since)
	if err != nil {
		return nil, since, err
	}
	w := time.Unix(cur.W, 0).UTC()
	open, err := List[pullRequest](ctx, h.c, asLoop, h.c.RepoPath("/pulls"), url.Values{"state": {"open"}})
	if err != nil {
		return nil, since, err
	}
	q := url.Values{"state": {"all"}, "since": {w.Format(time.RFC3339)}, "sort": {"updated"}, "direction": {"asc"}}
	updated, err := List[issue](ctx, h.c, asLoop, h.c.RepoPath("/issues"), q)
	if err != nil {
		return nil, since, err
	}
	isOpen := map[int]bool{}
	updatedAt := map[int]time.Time{}
	var nums []int
	for _, p := range open {
		if !isOpen[p.Number] {
			isOpen[p.Number] = true
			nums = append(nums, p.Number)
		}
	}
	newest := w
	for _, it := range updated {
		if it.PullRequest == nil {
			continue
		}
		updatedAt[it.Number] = it.UpdatedAt
		if it.UpdatedAt.After(newest) {
			newest = it.UpdatedAt
		}
		if !isOpen[it.Number] {
			nums = append(nums, it.Number)
		}
	}
	sort.Ints(nums)
	skips := map[int]int{}
	for _, n := range nums {
		if pc := cur.P[strconv.Itoa(n)]; pc != nil {
			skips[n] = pc.N
		}
	}
	ds, err := h.details(ctx, nums, skips)
	if err != nil {
		return nil, since, err
	}

	// Each pull request's steps, in its own order.
	seqs := make([][]pending, 0, len(nums))
	for _, n := range nums {
		pc := cur.P[strconv.Itoa(n)]
		var item ports.ItemRef
		if pc == nil && !ds[n].CreatedAt.Before(w) {
			// A pull request opened in the window: Opened names its item.
			c, err := h.Change(ctx, ports.ChangeRef(strconv.Itoa(n)))
			if err != nil {
				return nil, since, err
			}
			item = c.Item
		}
		seqs = append(seqs, h.steps(ds[n], pc, item, w, now))
	}
	next := hostCursor{W: cur.W, P: map[string]*pullCursor{}}
	for k, v := range cur.P {
		c := *v
		next.P[k] = &c
	}
	var out []ports.ChangeEvent
	heads := make([]int, len(seqs))
	drained := true
	for {
		best := -1
		for i, s := range seqs {
			if heads[i] >= len(s) {
				continue
			}
			if best < 0 || s[heads[i]].at.Before(seqs[best][heads[best]].at) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		st := seqs[best][heads[best]]
		if st.ev != nil && len(out) == h.pageSize {
			drained = false
			break
		}
		key := strconv.Itoa(nums[best])
		pc := next.P[key]
		if pc == nil {
			pc = &pullCursor{}
			next.P[key] = pc
		}
		st.apply(pc)
		if st.ev != nil {
			out = append(out, *st.ev)
		}
		heads[best]++
	}
	if drained {
		// Move the window to the newest update read, and keep only the
		// pull requests still open or updated in it.
		next.W = newest.Unix()
		for k := range next.P {
			n, _ := strconv.Atoi(k)
			if !isOpen[n] && updatedAt[n].Before(newest) {
				delete(next.P, k)
			}
		}
	}
	return out, next.encode(), nil
}

// feedEnd is the cursor at the current end: every open pull request as it
// is now.
func (h *Host) feedEnd(ctx context.Context, now time.Time) ([]ports.ChangeEvent, ports.Cursor, error) {
	open, err := List[pullRequest](ctx, h.c, asLoop, h.c.RepoPath("/pulls"), url.Values{"state": {"open"}})
	if err != nil {
		return nil, "", err
	}
	nums := make([]int, 0, len(open))
	for _, p := range open {
		nums = append(nums, p.Number)
	}
	ds, err := h.details(ctx, nums, nil)
	if err != nil {
		return nil, "", err
	}
	cur := hostCursor{W: now.Unix(), P: map[string]*pullCursor{}}
	for _, n := range nums {
		d := ds[n]
		pc := &pullCursor{N: d.Timeline.TotalCount, T: map[string]threadObs{}}
		for _, t := range d.Threads.Nodes {
			pc.T[t.ID] = h.see(t, nil)
		}
		pc.G, _ = gateDigest(d)
		cur.P[strconv.Itoa(n)] = pc
	}
	return nil, cur.encode(), nil
}

// steps returns a pull request's feed steps after pc (nil: not seen yet),
// in its own order: Opened, its timeline items, then its threads and gates.
func (h *Host) steps(d *prDetail, pc *pullCursor, item ports.ItemRef, w, now time.Time) []pending {
	ref := ports.ChangeRef(strconv.Itoa(d.Number))
	known := pc != nil
	skip := 0
	if known {
		skip = pc.N
	}
	fresh := !known && !d.CreatedAt.Before(w) // created in the window
	var out []pending
	ev := func(at time.Time, det ports.ChangeEventDetail) *ports.ChangeEvent {
		return &ports.ChangeEvent{Change: ref, At: at, Detail: det}
	}
	nodes := d.Timeline.Nodes
	lead := 0
	if fresh {
		// The commits already on the branch when it was opened come first
		// and aren't head moves; Opened steps past them, so a page that
		// ends after Opened never reports them later.
		for lead < len(nodes) && nodes[lead].Typename == "PullRequestCommit" && !nodes[lead].at().After(d.CreatedAt) {
			lead++
		}
		out = append(out, pending{at: d.CreatedAt, ev: ev(d.CreatedAt, ports.Opened{Actor: d.Author.actor(), Item: item}),
			apply: func(pc *pullCursor) { pc.N = max(pc.N, lead) }})
	}
	for i, x := range nodes {
		if i < lead {
			continue
		}
		idx := skip + i
		adv := func(pc *pullCursor) { pc.N = max(pc.N, idx+1) }
		var det ports.ChangeEventDetail
		switch x.Typename {
		case "PullRequestCommit":
			if x.Commit != nil {
				det = ports.HeadMoved{Commit: ports.Commit(x.Commit.OID), Pusher: ports.PusherUnknown}
			}
		case "PullRequestReview":
			det = ports.ReviewSubmitted{Actor: x.Author.actor()}
		case "ReviewDismissedEvent":
			det = ports.ReviewDismissed{Actor: x.Actor.actor()}
		case "MergedEvent":
			det = ports.Merged{Actor: x.Actor.actor()}
		case "ClosedEvent":
			merge := (i > 0 && nodes[i-1].Typename == "MergedEvent") || (i+1 < len(nodes) && nodes[i+1].Typename == "MergedEvent")
			if !merge {
				det = ports.ChangeClosed{Actor: x.Actor.actor()}
			}
		case "ReopenedEvent":
			det = ports.ChangeReopened{}
		}
		at := x.at()
		if !known {
			// Only items in the window are new; a new pull request's own
			// first commits are not head moves.
			if at.Before(w) || (x.Typename == "PullRequestCommit" && !at.After(d.CreatedAt)) {
				det = nil
			}
		}
		st := pending{at: at, apply: adv}
		if det != nil {
			st.ev = ev(at, det)
		}
		out = append(out, st)
	}
	var prevT map[string]threadObs
	if known {
		prevT = pc.T
	}
	for _, t := range d.Threads.Nodes {
		t := t
		prev, had := prevT[t.ID]
		var base *threadObs
		if had {
			base = &prev
		}
		o := h.see(t, base)
		set := func(pc *pullCursor) {
			if pc.T == nil {
				pc.T = map[string]threadObs{}
			}
			pc.T[t.ID] = o
		}
		var det ports.ChangeEventDetail
		switch {
		case !had && !known && !fresh:
			// A pull request seen again after it left the cursor: its old
			// resolves aren't news.
		case o.Resolved && (!prev.Resolved || prev.By != o.By):
			var by ports.Actor
			if t.ResolvedBy != nil {
				by = t.ResolvedBy.actor()
			}
			det = ports.ThreadResolved{Actor: by}
		case !o.Resolved && prev.Resolved:
			det = ports.ThreadUnresolved{}
		}
		st := pending{at: now, apply: set}
		if det != nil {
			st.ev = ev(now, det)
		}
		out = append(out, st)
	}
	g, any := gateDigest(d)
	prevG := ""
	if known {
		prevG = pc.G
	}
	if g != prevG {
		st := pending{at: now, apply: func(pc *pullCursor) { pc.G = g }}
		if any && (known || fresh) {
			st.ev = ev(now, ports.GateChanged{Commit: ports.Commit(d.HeadRefOID)})
		}
		out = append(out, st)
	}
	return out
}
