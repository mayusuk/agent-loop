package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// prReview is a review as the REST API returns it.
type prReview struct {
	ID          int64     `json:"id"`
	User        user      `json:"user"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

func (h *Host) reviews(ctx context.Context, n int) ([]prReview, error) {
	return List[prReview](ctx, h.c, asLoop, h.c.RepoPath("/pulls/%d/reviews", n), nil)
}

// ReviewRecords returns the reviews the reviewer identity posted with a
// review record, with the thread of each finding posted with them (C3,
// work plan D5).
func (h *Host) ReviewRecords(ctx context.Context, ref ports.ChangeRef) ([]ports.ReviewRecord, error) {
	n, err := changeNumber(ref)
	if err != nil {
		return nil, err
	}
	rs, err := h.reviews(ctx, n)
	if err != nil {
		return nil, err
	}
	d, err := h.detail(ctx, n)
	if err != nil {
		return nil, err
	}
	threads := h.findingThreads(d)
	var out []ports.ReviewRecord
	for _, r := range rs {
		id := strconv.FormatInt(r.ID, 10)
		rec, ok := h.ids.recordIn(r.Body, r.User, r.SubmittedAt, ports.CommentRef(id), ports.RecordReview)
		if !ok {
			continue
		}
		rr := ports.ReviewRecord{Ref: ports.ReviewRef(id), Author: rec.Author, SubmittedAt: r.SubmittedAt, Record: rec,
			Threads: map[ports.FindingID]ports.ThreadRef{}}
		for fid, ft := range threads {
			if ft.review == r.ID {
				rr.Threads[fid] = ports.ThreadRef(ft.thread.ID)
			}
		}
		out = append(out, rr)
	}
	return out, nil
}

// diffLines maps each file in the pull request's diff to the lines on the
// new side of its hunks; a file with no patch maps to nil (work plan D3).
func (h *Host) diffLines(ctx context.Context, n int) (map[string]map[int]bool, error) {
	type file struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	}
	fs, err := List[file](ctx, h.c, asLoop, h.c.RepoPath("/pulls/%d/files", n), nil)
	if err != nil {
		return nil, err
	}
	out := map[string]map[int]bool{}
	for _, f := range fs {
		out[f.Filename] = patchLines(f.Patch)
	}
	return out, nil
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// patchLines returns the new-side line numbers of a unified diff, or nil
// for an empty patch.
func patchLines(patch string) map[int]bool {
	if patch == "" {
		return nil
	}
	out := map[int]bool{}
	line := 0
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			line, _ = strconv.Atoi(m[1])
			continue
		}
		if line == 0 || strings.HasPrefix(l, "-") || strings.HasPrefix(l, `\`) {
			continue
		}
		out[line] = true
		line++
	}
	return out
}

// placement is where a finding goes (I8, I16).
type placement int

const (
	placeInline placement = iota
	placeFile
	placeText
)

func place(f ports.FindingPost, diff map[string]map[int]bool) placement {
	lines, inDiff := diff[f.Path]
	switch {
	case f.Path == "" || !inDiff:
		return placeText
	case f.Line > 0 && lines[f.Line]:
		return placeInline
	}
	return placeFile
}

func location(f ports.FindingPost) string {
	switch {
	case f.Path == "":
		return "no file"
	case f.Line > 0:
		return f.Path + ":" + strconv.Itoa(f.Line)
	}
	return f.Path
}

func findingHead(f ports.FindingPost) string {
	return fmt.Sprintf("**%s** (score %d, %s)", f.ID, f.Score, f.Status)
}

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// PostReview posts the review as the reviewer, with each new finding at the
// nearest place GitHub accepts and the status table in the body (§6.3, I8,
// I16, work plan D3–D5). A reviewer review with the key is returned
// instead.
func (h *Host) PostReview(ctx context.Context, ref ports.ChangeRef, p ports.ReviewPost, key ports.IdemKey) (ports.ReviewRef, error) {
	n, err := changeNumber(ref)
	if err != nil {
		return "", err
	}
	rs, err := h.reviews(ctx, n)
	if err != nil {
		return "", err
	}
	for _, r := range rs {
		if rec, ok := h.ids.recordIn(r.Body, r.User, r.SubmittedAt, "", ports.RecordReview); ok && rec.Key == key {
			return ports.ReviewRef(strconv.FormatInt(r.ID, 10)), nil
		}
	}
	marker, err := encodeMarker(p.Record, key)
	if err != nil {
		return "", err
	}
	diff, err := h.diffLines(ctx, n)
	if err != nil {
		return "", err
	}
	d, err := h.detail(ctx, n)
	if err != nil {
		return "", err
	}
	earlier := h.findingThreads(d)

	var comments []map[string]any
	var text []string
	where := map[ports.FindingID]string{}
	for _, f := range p.Findings {
		rec, err := withFields(f.Record, func(m map[string]json.RawMessage) error {
			if raw, ok := m["id"]; ok {
				var have string
				if json.Unmarshal(raw, &have) != nil || have != string(f.ID) {
					return fmt.Errorf("github: finding record names %s, not %s: %w", raw, f.ID, ports.ErrConflict)
				}
				return nil
			}
			m["id"], _ = json.Marshal(f.ID)
			return nil
		})
		if err != nil {
			return "", err
		}
		body := strings.TrimSpace(untrusted.Read(f.Body))
		switch place(f, diff) {
		case placeInline:
			fm, err := encodeMarker(rec, rec.Key)
			if err != nil {
				return "", err
			}
			comments = append(comments, map[string]any{"path": f.Path, "line": f.Line, "side": "RIGHT",
				"body": withMarker(joinText(findingHead(f), body), fm)})
			where[f.ID] = location(f)
		case placeFile:
			fm, err := encodeMarker(rec, rec.Key)
			if err != nil {
				return "", err
			}
			head := findingHead(f)
			if f.Line > 0 {
				head = "`" + location(f) + "` " + head
			}
			comments = append(comments, map[string]any{"path": f.Path, "subject_type": "file",
				"body": withMarker(joinText(head, body), fm)})
			where[f.ID] = f.Path + " (file)"
		default:
			text = append(text, joinText(findingHead(f)+" · "+location(f), body))
			where[f.ID] = "this review (" + location(f) + ")"
		}
	}

	// The status table: every finding the record lists, then any new one
	// it leaves out (work plan D4).
	var rows []string
	listed := map[ports.FindingID]bool{}
	var data struct {
		Findings []struct {
			ID     string `json:"id"`
			Score  int    `json:"score"`
			Status string `json:"status"`
		} `json:"findings"`
	}
	_ = json.Unmarshal(p.Record.Data, &data)
	row := func(id ports.FindingID, score int, status string) {
		w, ok := where[id]
		switch {
		case ok:
		case earlier[id].url != "":
			w = "[" + cell(earlier[id].thread.where()) + "](" + earlier[id].url + ")"
		default:
			w = "earlier review (no file)"
		}
		if ok {
			w = cell(w)
		}
		rows = append(rows, fmt.Sprintf("| %s | %d | %s | %s |", cell(string(id)), score, cell(status), w))
	}
	for _, f := range data.Findings {
		listed[ports.FindingID(f.ID)] = true
		row(ports.FindingID(f.ID), f.Score, f.Status)
	}
	for _, f := range p.Findings {
		if !listed[f.ID] {
			row(f.ID, f.Score, f.Status)
		}
	}
	parts := []string{strings.TrimSpace(untrusted.Read(p.Summary))}
	parts = append(parts, text...)
	if len(rows) > 0 {
		parts = append(parts, "| Finding | Score | Status | Where |\n|---|---|---|---|\n"+strings.Join(rows, "\n"))
	}
	event := "COMMENT"
	if p.Approve {
		event = "APPROVE"
	}
	in := map[string]any{"event": event, "body": withMarker(joinText(parts...), marker)}
	if p.Commit != "" {
		in["commit_id"] = string(p.Commit)
	}
	if len(comments) > 0 {
		in["comments"] = comments
	}
	var out prReview
	if err := h.c.Send(ctx, asReviewer, http.MethodPost, h.c.RepoPath("/pulls/%d/reviews", n), in, &out); err != nil {
		return "", err
	}
	return ports.ReviewRef(strconv.FormatInt(out.ID, 10)), nil
}

// joinText joins the non-empty parts with blank lines.
func joinText(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, strings.TrimSpace(p))
		}
	}
	return strings.Join(keep, "\n\n")
}

const (
	resolveThreadMutation = `mutation ResolveThread($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } } }`
)

// ResolveFinding resolves the finding's thread as the reviewer (§6.3, work
// plan D6). A finding with no thread has nothing to resolve.
func (h *Host) ResolveFinding(ctx context.Context, ref ports.ChangeRef, f ports.FindingID, _ ports.IdemKey) error {
	n, err := changeNumber(ref)
	if err != nil {
		return err
	}
	d, err := h.detail(ctx, n)
	if err != nil {
		return err
	}
	if ft, ok := h.findingThreads(d)[f]; ok {
		if ft.thread.IsResolved {
			return nil
		}
		return h.c.GraphQL(ctx, asReviewer, resolveThreadMutation, map[string]any{"id": ft.thread.ID}, nil)
	}
	rrs, err := h.ReviewRecords(ctx, ref)
	if err != nil {
		return err
	}
	for _, rr := range rrs {
		var data struct {
			Findings []struct {
				ID string `json:"id"`
			} `json:"findings"`
		}
		if json.Unmarshal(rr.Record.Data, &data) == nil {
			for _, x := range data.Findings {
				if x.ID == string(f) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("github: finding %s on change %s: %w", f, ref, ports.ErrNotFound)
}

// PostReply posts a coder reply in a thread, or on the change itself for a
// finding with no thread (§6.4, fake D3, work plan D7).
func (h *Host) PostReply(ctx context.Context, ref ports.ChangeRef, to ports.ThreadRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) error {
	n, err := changeNumber(ref)
	if err != nil {
		return err
	}
	marker, err := encodeMarker(rec, key)
	if err != nil {
		return err
	}
	text := withMarker(untrusted.Read(body), marker)
	if to == ports.ThreadRef(ref) {
		cs, err := List[comment](ctx, h.c, asLoop, h.c.RepoPath("/issues/%d/comments", n), nil)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if r, ok := h.ids.recordIn(c.Body, c.User, c.CreatedAt, "", ""); ok && r.Key == key && c.User.actor().ID == h.ids.Coder.ID {
				return nil
			}
		}
		return h.c.Send(ctx, asCoder, http.MethodPost, h.c.RepoPath("/issues/%d/comments", n), map[string]any{"body": text}, nil)
	}
	d, err := h.detail(ctx, n)
	if err != nil {
		return err
	}
	for _, t := range d.Threads.Nodes {
		if t.ID != string(to) || len(t.Comments.Nodes) == 0 {
			continue
		}
		for _, c := range t.Comments.Nodes {
			if r, ok := findMarker(c.Body); ok && r.Key == key && c.Author != nil && c.Author.actor().ID == h.ids.Coder.ID {
				return nil
			}
		}
		return h.c.Send(ctx, asCoder, http.MethodPost, h.c.RepoPath("/pulls/%d/comments/%d/replies", n, t.Comments.Nodes[0].DatabaseID),
			map[string]any{"body": text}, nil)
	}
	return fmt.Errorf("github: thread %s on change %s: %w", to, ref, ports.ErrNotFound)
}

// isIdentity reports whether the account is one of the loop's identities.
func (h *Host) isIdentity(id int64) bool {
	s := strconv.FormatInt(id, 10)
	return s == h.ids.Coder.ID || s == h.ids.Reviewer.ID || s == h.ids.Loop.ID
}

// isHuman: a User that isn't an identity. A "[bot]" login is never human,
// whatever its type (work plan D10).
func (h *Host) isHuman(u *gqlUser) bool {
	return u != nil && u.Typename == "User" && !strings.HasSuffix(u.Login, "[bot]") && !h.bots[botLogin(u.Login)] && !h.isIdentity(u.DatabaseID)
}

// isManualCommenter: a human, or a bot listed in manual_comment_bots (§9.6,
// I7, work plan D8).
func (h *Host) isManualCommenter(u *gqlUser) bool {
	if u == nil || h.isIdentity(u.DatabaseID) {
		return false
	}
	if u.Typename == "Bot" || strings.HasSuffix(u.Login, "[bot]") {
		return h.bots[botLogin(u.Login)]
	}
	return u.Typename == "User"
}

// threadObs is what was last seen of a review thread (work plan D9).
type threadObs struct {
	Resolved bool   `json:"r,omitempty"`
	By       string `json:"b,omitempty"` // the resolver's ID
	Last     int64  `json:"l,omitempty"` // the newest comment ID seen
	Upto     int64  `json:"u,omitempty"` // a human resolve clears comments up to this ID
}

// observeThread folds what is seen of a thread now into what was seen
// before (had says whether it was seen at all).
func (h *Host) observeThread(prev threadObs, had bool, t prThread) threadObs {
	now := threadObs{Resolved: t.IsResolved}
	for _, c := range t.Comments.Nodes {
		now.Last = max(now.Last, c.DatabaseID)
	}
	if !t.IsResolved {
		return now
	}
	if t.ResolvedBy != nil {
		now.By = strconv.FormatInt(t.ResolvedBy.DatabaseID, 10)
	}
	if !h.isHuman(t.ResolvedBy) {
		return now
	}
	switch {
	case !had:
		now.Upto = 0 // first seen resolved: nothing is known to precede it
	case prev.Resolved && prev.By == now.By:
		now.Upto = prev.Upto
	default:
		now.Upto = prev.Last
	}
	return now
}

// see records an observation of t and returns it.
func (h *Host) see(t prThread, base *threadObs) threadObs {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev, had := h.obs[t.ID]
	if !had && base != nil {
		prev, had = *base, true
	}
	o := h.observeThread(prev, had, t)
	h.obs[t.ID] = o
	return o
}

// ManualComments returns the manual comments on the change and whether a
// human cleared each (§9.6, D43, work plan D8–D10).
func (h *Host) ManualComments(ctx context.Context, ref ports.ChangeRef) ([]ports.ManualComment, error) {
	n, err := changeNumber(ref)
	if err != nil {
		return nil, err
	}
	d, err := h.detail(ctx, n)
	if err != nil {
		return nil, err
	}
	rs, err := h.reviews(ctx, n)
	if err != nil {
		return nil, err
	}
	var out []ports.ManualComment
	for _, t := range d.Threads.Nodes {
		o := h.see(t, nil)
		for _, c := range t.Comments.Nodes {
			if !h.isManualCommenter(c.Author) {
				continue
			}
			mc := ports.ManualComment{ID: "MC-" + strconv.FormatInt(c.DatabaseID, 10), Author: c.Author.actor(),
				Kind: ports.ManualCommentThread, Thread: ports.ThreadRef(t.ID), Body: untrusted.New(c.Body), CreatedAt: c.CreatedAt}
			if o.Upto > 0 && c.DatabaseID <= o.Upto {
				mc.Cleared, mc.ClearedBy = true, ports.ClearedByHumanResolved
			}
			out = append(out, mc)
		}
	}
	// The latest dismissal of each review.
	type dismissal struct {
		by   *gqlUser
		prev string
	}
	dismissed := map[int64]dismissal{}
	for _, x := range d.Timeline.Nodes {
		if x.Typename == "ReviewDismissedEvent" && x.Review != nil {
			dismissed[x.Review.DatabaseID] = dismissal{by: x.Actor, prev: x.PreviousReviewState}
		}
	}
	for _, r := range rs {
		author := &gqlUser{Typename: r.User.Type, Login: r.User.Login, DatabaseID: r.User.ID}
		if !h.isManualCommenter(author) {
			continue
		}
		state := r.State
		dis, wasDismissed := dismissed[r.ID]
		if state == "DISMISSED" && wasDismissed && dis.prev != "" {
			state = dis.prev
		}
		kind := ports.ManualCommentReviewBody
		if strings.TrimSpace(r.Body) == "" {
			if state != "CHANGES_REQUESTED" {
				continue // an empty comment or approval is not a manual comment
			}
			kind = ports.ManualCommentEmptyChangesRequested
		}
		mc := ports.ManualComment{ID: "MC-R" + strconv.FormatInt(r.ID, 10), Author: r.User.actor(), Kind: kind,
			Body: untrusted.New(r.Body), CreatedAt: r.SubmittedAt}
		if r.State == "DISMISSED" && wasDismissed && h.isHuman(dis.by) {
			mc.Cleared, mc.ClearedBy = true, ports.ClearedByHumanDismissed
		}
		out = append(out, mc)
	}
	return out, nil
}
