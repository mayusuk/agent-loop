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
}

// readReviews reads the change's review records.
func (a *Activities) readReviews(ctx context.Context, change ports.ChangeRef) (reviewState, error) {
	rrs, err := a.Host.ReviewRecords(ctx, change)
	if err != nil {
		return reviewState{}, err
	}
	st := reviewState{next: 1, threads: map[string]ports.ThreadRef{}, byKey: map[ports.IdemKey]contract.Record{}}
	for _, rr := range rrs {
		var rec contract.Record
		if err := json.Unmarshal(rr.Record.Data, &rec); err != nil {
			continue
		}
		st.byKey[rr.Record.Key] = rec
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

// posted returns the review this run already posted, if a crash or retry
// came after the post (NFR-2: a stage that completed is a no-op).
func (r *run) posted(ctx context.Context) (StageReport, bool, error) {
	st, err := r.a.readReviews(ctx, ports.ChangeRef(r.req.Run.Change))
	if err != nil {
		return StageReport{}, false, err
	}
	rec, ok := st.byKey[r.key("review")]
	if !ok {
		return StageReport{}, false, nil
	}
	res := r.result(contract.OutcomeDone)
	res.Review = st.outcome(rec)
	return StageReport{Result: res}, true, nil
}

// postReview checks the new findings' paths and lines at the commit, gives
// each new finding the next ID on the change, and posts one review as the
// reviewer identity with every finding's state (INTEGRATION-SPEC §5.2
// review, §6.3). A review whose commit is no longer the head is posted
// stale (FR-4 AC3). Threads of findings this review ruled addressed or
// withdrawn are resolved.
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
	var resolve []string
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
		if ru.Status == contract.StatusAddressed || ru.Status == contract.StatusWithdrawn {
			resolve = append(resolve, ru.Finding)
		}
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
	key := r.key("review")
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
	// A stale review's statuses are not used (FR-4 AC3), so it resolves
	// nothing.
	if !stale {
		for _, id := range resolve {
			fid := ports.FindingID(id)
			if err := a.retry(ctx, func() error { return a.Host.ResolveFinding(ctx, change, fid, r.key("resolve:"+id)) }); err != nil {
				return StageReport{}, err
			}
		}
	}
	// Read the threads of the new findings back.
	if after, err := a.readReviews(ctx, change); err == nil {
		st.threads = after.threads
	}
	res := r.result(contract.OutcomeDone)
	res.Review = st.outcome(rr)
	return StageReport{Result: res}, nil
}
