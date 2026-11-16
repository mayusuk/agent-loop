package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxTextFile is the largest text file a result may point to
// (INTEGRATION-SPEC §5.1).
const MaxTextFile = 64 << 10

// MaxFailedReason is the most characters failed.reason may hold.
const MaxFailedReason = 500

// ErrInvalidResult wraps every reason ValidateResult rejects a result.
var ErrInvalidResult = errors.New("invalid result")

var (
	shaRe     = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	planKeyRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// ValidateResult decodes result.json and applies every rule of
// INTEGRATION-SPEC §5 that can be decided from the input, the result and
// the AGENT_LOOP_OUT directory (work plan D3). A valid result is returned
// with a nil error. On any violation it returns a failed result whose
// reason names the rule, and the error, wrapping ErrInvalidResult and,
// for a version the core can't support, ErrUnsupportedVersion (FR-18 AC3).
func ValidateResult(in Input, data []byte, outDir string) (Result, error) {
	r, err := ParseResult(in.Stage, data)
	if err == nil {
		err = check(in, r, outDir)
	}
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrInvalidResult, err)
		return failedResult(in, err), err
	}
	return r, nil
}

// failedResult is the failed result that replaces an invalid one.
func failedResult(in Input, err error) Result {
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if utf8.RuneCountInString(reason) > MaxFailedReason {
		reason = string([]rune(reason)[:MaxFailedReason-1]) + "…"
	}
	return Result{
		ContractVersion: Version,
		RunID:           in.RunID,
		Result:          OutcomeFailed,
		Failed:          &Failed{Reason: reason},
	}
}

func check(in Input, r Result, outDir string) error {
	if _, ok := allowedOutcomes[in.Stage]; !ok {
		return fmt.Errorf("stage %q has no handler", in.Stage)
	}
	if r.RunID != in.RunID {
		return fmt.Errorf("run_id %q does not match the input's %q", r.RunID, in.RunID)
	}
	present := map[Outcome]bool{
		OutcomeDone:       r.Done != nil,
		OutcomeComplete:   r.Complete != nil,
		OutcomeBlocked:    r.Blocked != nil,
		OutcomeFailed:     r.Failed != nil,
		OutcomeNeedsOwner: r.NeedsOwner != nil,
	}
	if _, known := present[r.Result]; !known {
		return fmt.Errorf("result %q is not a known result", r.Result)
	}
	if !Allowed(in.Stage, r.Result) {
		return fmt.Errorf("result %q is not allowed from %s", r.Result, in.Stage)
	}
	for _, o := range []Outcome{OutcomeDone, OutcomeComplete, OutcomeBlocked, OutcomeFailed, OutcomeNeedsOwner} {
		p := present[o]
		if o == r.Result && !p {
			return fmt.Errorf("result is %q but there is no %s object", r.Result, o)
		}
		if o != r.Result && p {
			return fmt.Errorf("result is %q but a %s object is present", r.Result, o)
		}
	}
	if err := checkUsage(r.Usage); err != nil {
		return err
	}
	switch r.Result {
	case OutcomeBlocked:
		if err := checkFile(outDir, "blocked.reason_file", r.Blocked.ReasonFile); err != nil {
			return err
		}
		if d := r.Blocked.Dependency; d != nil && strings.TrimSpace(*d) == "" {
			return errors.New("blocked.dependency is empty")
		}
	case OutcomeFailed:
		return checkReason(r.Failed.Reason)
	case OutcomeNeedsOwner:
		return checkFile(outDir, "needs_owner.question_file", r.NeedsOwner.QuestionFile)
	case OutcomeDone:
		return checkDone(in, r.Done, outDir)
	}
	return nil
}

func checkUsage(u *Usage) error {
	if u == nil {
		return nil
	}
	if u.Cost != nil && *u.Cost < 0 {
		return errors.New("usage.cost is negative")
	}
	if u.Currency != nil && strings.TrimSpace(*u.Currency) == "" {
		return errors.New("usage.currency is empty")
	}
	if u.InputTokens != nil && *u.InputTokens < 0 {
		return errors.New("usage.input_tokens is negative")
	}
	if u.OutputTokens != nil && *u.OutputTokens < 0 {
		return errors.New("usage.output_tokens is negative")
	}
	return nil
}

func checkReason(s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return errors.New("failed.reason is empty")
	case strings.ContainsAny(s, "\r\n"):
		return errors.New("failed.reason is more than one line")
	case utf8.RuneCountInString(s) > MaxFailedReason:
		return fmt.Errorf("failed.reason is longer than %d characters", MaxFailedReason)
	}
	return nil
}

// checkFile checks a *_file path: relative, inside outDir after symlinks
// are resolved, a regular UTF-8 file of at most MaxTextFile bytes
// (INTEGRATION-SPEC §5.1, work plan D5).
func checkFile(outDir, field, rel string) error {
	if rel == "" {
		return fmt.Errorf("%s is missing", field)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return fmt.Errorf("%s %q is not relative to AGENT_LOOP_OUT", field, rel)
	}
	for _, seg := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return fmt.Errorf("%s %q is outside AGENT_LOOP_OUT", field, rel)
		}
	}
	root, err := filepath.EvalSymlinks(outDir)
	if err != nil {
		return fmt.Errorf("AGENT_LOOP_OUT: %w", err)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("%s %q can't be read: %w", field, rel, err)
	}
	if within, err := filepath.Rel(root, path); err != nil || within == ".." ||
		strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s %q is outside AGENT_LOOP_OUT", field, rel)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %q can't be read: %w", field, rel, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s %q is not a regular file", field, rel)
	}
	if fi.Size() > MaxTextFile {
		return fmt.Errorf("%s %q is larger than 64 KiB", field, rel)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s %q can't be read: %w", field, rel, err)
	}
	if len(b) > MaxTextFile {
		return fmt.Errorf("%s %q is larger than 64 KiB", field, rel)
	}
	if !utf8.Valid(b) {
		return fmt.Errorf("%s %q is not UTF-8", field, rel)
	}
	return nil
}

func checkSHA(field, s string) error {
	if !shaRe.MatchString(s) {
		return fmt.Errorf("%s %q is not a full commit SHA", field, s)
	}
	return nil
}

func checkDone(in Input, d *Done, outDir string) error {
	switch in.Stage {
	case StageImplement:
		return checkImplement(d.Implement, outDir)
	case StageReview:
		return checkReview(in, d.Review, outDir)
	case StageAddress:
		return checkAddress(in, d.Address, outDir)
	case StageMint:
		return checkMint(d.Mint, outDir)
	}
	return fmt.Errorf("stage %q has no handler", in.Stage)
}

func checkImplement(d *ImplementDone, outDir string) error {
	if err := checkSHA("done.head", d.Head); err != nil {
		return err
	}
	if d.Change == nil {
		return errors.New("done.change is missing")
	}
	if strings.TrimSpace(d.Change.Title) == "" {
		return errors.New("done.change.title is empty")
	}
	return checkFile(outDir, "done.change.body_file", d.Change.BodyFile)
}

func checkMint(d *MintDone, outDir string) error {
	if !planKeyRe.MatchString(d.PlanKey) {
		return fmt.Errorf("done.plan_key %q does not match [a-z0-9-]{1,64}", d.PlanKey)
	}
	if err := checkSHA("done.head", d.Head); err != nil {
		return err
	}
	if d.Proposal == nil {
		return errors.New("done.proposal is missing")
	}
	if strings.TrimSpace(d.Proposal.Title) == "" {
		return errors.New("done.proposal.title is empty")
	}
	return checkFile(outDir, "done.proposal.body_file", d.Proposal.BodyFile)
}

// idSet is a set of IDs that must each be matched exactly once.
type idSet map[string]bool // ID -> matched

func newIDSet(ids []string) idSet {
	s := idSet{}
	for _, id := range ids {
		s[id] = false
	}
	return s
}

// take marks id matched. It fails for an ID not in the set, or one already
// matched (work plan D8).
func (s idSet) take(field, id string) error {
	matched, ok := s[id]
	switch {
	case !ok:
		return fmt.Errorf("%s names %q, which is not in the input", field, id)
	case matched:
		return fmt.Errorf("%s names %q twice", field, id)
	}
	s[id] = true
	return nil
}

// missing returns the IDs not matched, in input order.
func (s idSet) missing(order []string) []string {
	var out []string
	for _, id := range order {
		if !s[id] {
			out = append(out, id)
		}
	}
	return out
}

func checkReview(in Input, d *ReviewDone, outDir string) error {
	ri := in.Review
	if ri == nil {
		return errors.New("input has no review object")
	}
	if d.Commit != ri.Commit {
		return fmt.Errorf("done.commit %q is not the input's review.commit %q", d.Commit, ri.Commit)
	}
	if d.Kind != ri.Kind {
		return fmt.Errorf("done.kind %q is not the input's review.kind %q", d.Kind, ri.Kind)
	}
	if err := checkFile(outDir, "done.summary_file", d.SummaryFile); err != nil {
		return err
	}
	rule := newIDSet(ri.RuleFindings)
	for i, r := range d.Rulings {
		field := fmt.Sprintf("done.rulings[%d]", i)
		if err := rule.take(field, r.Finding); err != nil {
			return err
		}
		switch r.Status {
		case StatusOpen, StatusPartiallyAddressed, StatusAddressed, StatusWithdrawn:
		default:
			return fmt.Errorf("%s.status %q is not a finding status", field, r.Status)
		}
		if r.NoteFile != "" {
			if err := checkFile(outDir, field+".note_file", r.NoteFile); err != nil {
				return err
			}
		}
	}
	if m := rule.missing(ri.RuleFindings); len(m) > 0 {
		return fmt.Errorf("done.rulings has no ruling for %s (FR-4 AC4)", strings.Join(m, ", "))
	}
	manual := newIDSet(ri.RuleManual)
	for i, r := range d.ManualRulings {
		field := fmt.Sprintf("done.manual_rulings[%d]", i)
		if err := manual.take(field, r.Comment); err != nil {
			return err
		}
		if r.Ruling != RulingAddressed && r.Ruling != RulingOpen {
			return fmt.Errorf("%s.ruling %q is not addressed or open", field, r.Ruling)
		}
		if r.NoteFile != "" {
			if err := checkFile(outDir, field+".note_file", r.NoteFile); err != nil {
				return err
			}
		}
	}
	if m := manual.missing(ri.RuleManual); len(m) > 0 {
		return fmt.Errorf("done.manual_rulings has no ruling for %s (FR-4 AC8)", strings.Join(m, ", "))
	}
	for i, f := range d.NewFindings {
		field := fmt.Sprintf("done.new_findings[%d]", i)
		if f.Score < 1 || f.Score > 5 {
			return fmt.Errorf("%s.score %d is not from 1 to 5", field, f.Score)
		}
		if f.Path != nil {
			if err := checkRepoPath(field+".path", *f.Path); err != nil {
				return err
			}
		}
		if f.Line != nil {
			if f.Path == nil {
				return fmt.Errorf("%s.line needs a path", field)
			}
			if *f.Line < 1 {
				return fmt.Errorf("%s.line %d is less than 1", field, *f.Line)
			}
		}
		if strings.TrimSpace(f.Title) == "" {
			return fmt.Errorf("%s.title is empty", field)
		}
		if err := checkFile(outDir, field+".body_file", f.BodyFile); err != nil {
			return err
		}
	}
	return nil
}

// checkRepoPath checks a repository-relative slash path (work plan D9).
func checkRepoPath(field, p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("%s %q is not a repository-relative path", field, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%s %q is not a repository-relative path", field, p)
		}
	}
	return nil
}

func checkReplyKind(field string, k ReplyKind, deferredTo *string) error {
	switch k {
	case ReplyFixed, ReplyExplained, ReplyNotValid, ReplyNeedsInfo:
		if deferredTo != nil {
			return fmt.Errorf("%s.deferred_to is set on a %s reply", field, k)
		}
	case ReplyDeferred:
		// A deferred reply without deferred_to is well formed but doesn't
		// count as a reply (FR-5 AC5): MissingReplies leaves it out, and
		// FR-5 AC2 decides (work plan D23).
	default:
		return fmt.Errorf("%s.kind %q is not a reply kind", field, k)
	}
	return nil
}

func checkAddress(in Input, d *AddressDone, outDir string) error {
	ai := in.Address
	if ai == nil {
		return errors.New("input has no address object")
	}
	if err := checkSHA("done.head", d.Head); err != nil {
		return err
	}
	if ai.Mode == ModeBaseUpdate {
		if len(d.Replies) > 0 || len(d.ManualReplies) > 0 {
			return errors.New("a base_update address run must not reply (FR-5 AC6)")
		}
		if d.Head == in.Workspace.Head {
			return errors.New("a base_update address run must push a merge commit (FR-5 AC6)")
		}
	}
	findings := newIDSet(ai.AnswerFindings)
	for i, r := range d.Replies {
		field := fmt.Sprintf("done.replies[%d]", i)
		if err := findings.take(field, r.Finding); err != nil {
			return err
		}
		if err := checkReplyKind(field, r.Kind, r.DeferredTo); err != nil {
			return err
		}
		if err := checkFile(outDir, field+".body_file", r.BodyFile); err != nil {
			return err
		}
	}
	manual := newIDSet(ai.AnswerManual)
	for i, r := range d.ManualReplies {
		field := fmt.Sprintf("done.manual_replies[%d]", i)
		if err := manual.take(field, r.Comment); err != nil {
			return err
		}
		if err := checkReplyKind(field, r.Kind, r.DeferredTo); err != nil {
			return err
		}
		if err := checkFile(outDir, field+".body_file", r.BodyFile); err != nil {
			return err
		}
	}
	mf, mm := MissingReplies(in, d, func(string) bool { return true })
	if len(mf) > 0 && d.Head == in.Workspace.Head {
		return fmt.Errorf("the head did not move and %s have no reply (FR-5 AC2)", strings.Join(mf, ", "))
	}
	if len(mm) > 0 {
		return fmt.Errorf("manual comments %s have no reply (FR-5 AC2)", strings.Join(mm, ", "))
	}
	return nil
}

// MissingReplies returns the IDs in the input's answer_findings and
// answer_manual that have no reply that counts, in input order. A deferred
// reply counts only when it names a deferred_to and deferredOK(deferred_to)
// is true (FR-5 AC5, work plan D23).
// ValidateResult passes a deferredOK that accepts every item; the worker,
// which can read the tracker, passes one that checks the item is open, and
// fails the result when FR-5 AC2 no longer holds (work plan D3).
func MissingReplies(in Input, d *AddressDone, deferredOK func(item string) bool) (findings, manual []string) {
	if in.Address == nil || d == nil {
		return nil, nil
	}
	counts := func(k ReplyKind, to *string) bool {
		return k != ReplyDeferred || (to != nil && strings.TrimSpace(*to) != "" && deferredOK(*to))
	}
	gotF := map[string]bool{}
	for _, r := range d.Replies {
		if counts(r.Kind, r.DeferredTo) {
			gotF[r.Finding] = true
		}
	}
	gotM := map[string]bool{}
	for _, r := range d.ManualReplies {
		if counts(r.Kind, r.DeferredTo) {
			gotM[r.Comment] = true
		}
	}
	for _, id := range in.Address.AnswerFindings {
		if !gotF[id] {
			findings = append(findings, id)
		}
	}
	for _, id := range in.Address.AnswerManual {
		if !gotM[id] {
			manual = append(manual, id)
		}
	}
	return findings, manual
}
