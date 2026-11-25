package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// reviewState is the change's findings as its review records leave them
// (INTEGRATION-SPEC §6.3).
type reviewState struct {
	// latest is the findings of the latest review record that isn't stale.
	latest []contract.ReviewFindingState
	// next is the number of the next finding ID: one more than the highest
	// ID any review record on the change used, stale ones included.
	next int
	// threads maps each finding to its thread.
	threads map[string]ports.ThreadRef
	// byKey is every review record by key.
	byKey map[ports.IdemKey]contract.Record
	// count is the number of review records on the change.
	count int
	// before is, for each review record by key, the findings of the latest
	// review record before it that isn't stale.
	before map[ports.IdemKey][]contract.ReviewFindingState
}

// readReviews reads the change's review records.
func (a *Activities) readReviews(ctx context.Context, change ports.ChangeRef) (reviewState, error) {
	rrs, err := a.Host.ReviewRecords(ctx, change)
	if err != nil {
		return reviewState{}, err
	}
	st := reviewState{next: 1, threads: map[string]ports.ThreadRef{}, byKey: map[ports.IdemKey]contract.Record{},
		before: map[ports.IdemKey][]contract.ReviewFindingState{}}
	for _, rr := range rrs {
		var rec contract.Record
		if err := json.Unmarshal(rr.Record.Data, &rec); err != nil {
			continue
		}
		st.byKey[rr.Record.Key] = rec
		st.before[rr.Record.Key] = st.latest
		st.count++
		for id, t := range rr.Threads {
			st.threads[string(id)] = t
		}
		for _, f := range rec.Findings {
			if n, err := strconv.Atoi(strings.TrimPrefix(f.ID, "F")); err == nil && n >= st.next {
				st.next = n + 1
			}
		}
		if !rec.Stale {
			st.latest = rec.Findings
		}
	}
	return st, nil
}

// outcome is a review record as the core reads it.
func (st reviewState) outcome(rec contract.Record) *core.ReviewOutcome {
	o := &core.ReviewOutcome{Commit: rec.Commit, Kind: contract.ReviewKind(rec.Kind), Stale: rec.Stale}
	for _, f := range rec.Findings {
		c := f.Comment
		if t, ok := st.threads[f.ID]; ok && c == "" {
			c = string(t)
		}
		o.Findings = append(o.Findings, core.Finding{ID: f.ID, Score: f.Score, Status: f.Status, Comment: c})
	}
	for _, m := range rec.ManualRulings {
		o.ManualRulings = append(o.ManualRulings, core.ManualRuling{Comment: m.Comment, Ruling: m.Ruling})
	}
	return o
}

// reviewKey is the review record's key: the kind, the round and the commit
// the core asked for, and for a review that isn't a round its sequence
// number when it is above zero. Crash reruns keep all of them; a new round,
// a new commit or a later uncounted review changes it (NFR-2 AC1, D3,
// work plan #25 D4).
func (r *run) reviewKey() ports.IdemKey {
	rv := r.req.Run.Review
	parts := []string{"review", string(rv.Kind), "r" + strconv.Itoa(rv.Round), rv.Commit}
	if rv.Seq > 0 {
		parts = append(parts, "n"+strconv.Itoa(rv.Seq))
	}
	return r.key(parts...)
}

// posted returns the review this run already posted, if a crash or retry
// came after the post (NFR-2: a stage that completed is a no-op).
func (r *run) posted(ctx context.Context) (StageReport, bool, error) {
	st, err := r.a.readReviews(ctx, ports.ChangeRef(r.req.Run.Change))
	if err != nil {
		return StageReport{}, false, err
	}
	rec, ok := st.byKey[r.reviewKey()]
	if !ok {
		return StageReport{}, false, nil
	}
	res := r.result(contract.OutcomeDone)
	res.Review = st.outcome(rec)
	// The crash may have come before the resolves.
	return StageReport{Result: res, Resolve: r.resolveRequest(rec, st.before[r.reviewKey()])}, true, nil
}

// resolveRequest is the ResolveFindings request for the review record rec:
// the findings it rules addressed or withdrawn that the latest earlier
// review record that isn't stale, prev, didn't, so a thread someone
// unresolved later isn't resolved again (INTEGRATION-SPEC §6.3). A stale
// review's statuses are not used (FR-4 AC3), so it resolves nothing. Nil
// when there is nothing to resolve.
func (r *run) resolveRequest(rec contract.Record, prev []contract.ReviewFindingState) *ResolveRequest {
	if rec.Stale {
		return nil
	}
	closed := func(s contract.FindingStatus) bool {
		return s == contract.StatusAddressed || s == contract.StatusWithdrawn
	}
	was := map[string]bool{}
	for _, f := range prev {
		was[f.ID] = closed(f.Status)
	}
	req := &ResolveRequest{Change: r.req.Run.Change}
	for _, f := range rec.Findings {
		if closed(f.Status) && !was[f.ID] {
			req.Findings = append(req.Findings, ResolveFinding{ID: f.ID, Key: string(r.key("resolve", f.ID))})
		}
	}
	if len(req.Findings) == 0 {
		return nil
	}
	return req
}

// postReview checks the new findings' paths and lines at the commit, gives
// each new finding the next ID on the change, and posts one review as the
// reviewer identity with every finding's state (INTEGRATION-SPEC §5.2
// review, §6.3). A review whose commit is no longer the head is posted
// stale (FR-4 AC3). The threads of the findings this review rules
// addressed or withdrawn are resolved afterwards, by the ResolveFindings
// activity (StageReport.Resolve).
func (r *run) postReview(ctx context.Context, d *contract.ReviewDone) (StageReport, error) {
	a := r.a
	commit := ports.Commit(d.Commit)
	change := ports.ChangeRef(r.req.Run.Change)
	for i, f := range d.NewFindings {
		if f.Path == nil {
			continue
		}
		n, err := a.Git.FileLines(ctx, r.dir, commit, *f.Path)
		if errors.Is(err, ports.ErrNotFound) {
			return r.failed(fmt.Sprintf("done.new_findings[%d].path %q is not a file at %s", i, *f.Path, commit)), nil
		}
		if err != nil {
			return StageReport{}, err
		}
		if f.Line != nil && *f.Line > n {
			return r.failed(fmt.Sprintf("done.new_findings[%d].line %d is past the end of %s (%d lines)", i, *f.Line, *f.Path, n)), nil
		}
	}

	st, err := a.readReviews(ctx, change)
	if err != nil {
		return StageReport{}, err
	}
	findings := append([]contract.ReviewFindingState(nil), st.latest...)
	index := map[string]int{}
	for i, f := range findings {
		index[f.ID] = i
		if t, ok := st.threads[f.ID]; ok && f.Comment == "" {
			findings[i].Comment = string(t)
		}
	}
	var notes []string
	for _, ru := range d.Rulings {
		i, ok := index[ru.Finding]
		if !ok {
			// rule_findings named a finding no record holds: keep it with
			// the ruling, so the record still lists it.
			findings = append(findings, contract.ReviewFindingState{ID: ru.Finding, Status: ru.Status})
			i = len(findings) - 1
			index[ru.Finding] = i
		}
		findings[i].Status = ru.Status
		if ru.NoteFile != "" {
			s, err := readOut(r.outDir, ru.NoteFile)
			if err != nil {
				return StageReport{}, err
			}
			notes = append(notes, fmt.Sprintf("**%s** (%s): %s", ru.Finding, ru.Status, strings.TrimSpace(s)))
		}
	}
	var manual []contract.RecordManualRuling
	for _, m := range d.ManualRulings {
		manual = append(manual, contract.RecordManualRuling{Comment: m.Comment, Ruling: m.Ruling})
		if m.NoteFile != "" {
			s, err := readOut(r.outDir, m.NoteFile)
			if err != nil {
				return StageReport{}, err
			}
			notes = append(notes, fmt.Sprintf("**%s** (%s): %s", m.Comment, m.Ruling, strings.TrimSpace(s)))
		}
	}
	key := r.reviewKey()
	var posts []ports.FindingPost
	for i, f := range d.NewFindings {
		id := "F" + strconv.Itoa(st.next+i)
		findings = append(findings, contract.ReviewFindingState{ID: id, Score: f.Score, Status: contract.StatusOpen})
		body, err := readOut(r.outDir, f.BodyFile)
		if err != nil {
			return StageReport{}, err
		}
		frec, err := record(contract.Record{Type: contract.RecordFinding, Key: string(key) + ":" + id, Change: string(change), Finding: id, Score: f.Score})
		if err != nil {
			return StageReport{}, err
		}
		fp := ports.FindingPost{
			ID: ports.FindingID(id), Score: f.Score, Status: string(contract.StatusOpen),
			Body: a.text("**" + strings.TrimSpace(f.Title) + "**\n\n" + strings.TrimSpace(body)), Record: frec,
		}
		if f.Path != nil {
			fp.Path = *f.Path
		}
		if f.Line != nil {
			fp.Line = *f.Line
		}
		posts = append(posts, fp)
	}

	ch, err := a.Host.Change(ctx, change)
	if err != nil {
		return StageReport{}, err
	}
	stale := ch.Head != commit
	approve := a.RequireApproval && !stale
	for _, f := range findings {
		if (f.Status == contract.StatusOpen || f.Status == contract.StatusPartiallyAddressed) && f.Score >= a.threshold() {
			approve = false
		}
	}
	summary, err := readOut(r.outDir, d.SummaryFile)
	if err != nil {
		return StageReport{}, err
	}
	if len(notes) > 0 {
		summary = strings.TrimSpace(summary) + "\n\n" + strings.Join(notes, "\n\n")
	}
	rr := contract.Record{
		Type: contract.RecordReview, Key: string(key), Item: r.req.Item, Change: string(change),
		Commit: d.Commit, Kind: string(d.Kind), Stale: stale, Findings: findings, ManualRulings: manual,
	}
	if d.Kind == contract.KindRound {
		n := r.req.Run.Review.Round
		rr.Round = &n
	}
	prec, err := record(rr)
	if err != nil {
		return StageReport{}, err
	}
	post := ports.ReviewPost{Commit: commit, Approve: approve, Summary: a.text(summary), Findings: posts, Record: prec}
	err = a.retry(ctx, func() error {
		_, err := a.Host.PostReview(ctx, change, post, key)
		return err
	})
	if err != nil {
		return StageReport{}, err
	}
	// Read the threads of the new findings back.
	if after, err := a.readReviews(ctx, change); err == nil {
		st.threads = after.threads
	}
	res := r.result(contract.OutcomeDone)
	res.Review = st.outcome(rr)
	return StageReport{Result: res, Resolve: r.resolveRequest(rr, st.latest)}, nil
}
