package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RecordType is a record's type (INTEGRATION-SPEC §6.1).
type RecordType string

// The record types.
const (
	RecordChange          RecordType = "change"
	RecordReview          RecordType = "review"
	RecordFinding         RecordType = "finding"
	RecordReply           RecordType = "reply"
	RecordManualReply     RecordType = "manual_reply"
	RecordStatus          RecordType = "status"
	RecordApprovedCommits RecordType = "approved_commits"
	RecordFollowup        RecordType = "followup"
	RecordTask            RecordType = "task"
	RecordPlan            RecordType = "plan"
	RecordProposal        RecordType = "proposal"
)

var recordTypes = map[RecordType]bool{
	RecordChange: true, RecordReview: true, RecordFinding: true, RecordReply: true,
	RecordManualReply: true, RecordStatus: true, RecordApprovedCommits: true,
	RecordFollowup: true, RecordTask: true, RecordPlan: true, RecordProposal: true,
}

// StatusEvent is a status record's event (INTEGRATION-SPEC §6.5).
type StatusEvent string

// The status events.
const (
	EventClaim        StatusEvent = "claim"
	EventEscalate     StatusEvent = "escalate"
	EventResume       StatusEvent = "resume"
	EventPause        StatusEvent = "pause"
	EventAbort        StatusEvent = "abort"
	EventRelease      StatusEvent = "release"
	EventBlocked      StatusEvent = "blocked"
	EventManualAction StatusEvent = "manual_action"
)

// Status reason codes the specs name (INTEGRATION-SPEC §6.5, work plan
// D16). The core tasks add the others.
const (
	ReasonRoundLimit                = "round_limit"
	ReasonUntrustedCommit           = "untrusted_commit"
	ReasonBranchOutsidePrefix       = "branch_outside_prefix"
	ReasonEmptyReviewNeedsDismissal = "empty_review_needs_dismissal"
	ReasonWorkerLost                = "worker_lost"
	ReasonGateFailed                = "gate_failed"
	ReasonPusherUnknown             = "pusher_unknown"
)

// Record is the JSON in a record marker (INTEGRATION-SPEC §6). Type and Key
// are always set; the other fields belong to particular types (work plan
// D15) and are left out when empty.
type Record struct {
	Type RecordType `json:"type"`
	V    int        `json:"v,omitempty"`
	Key  string     `json:"key"`

	Item   string `json:"item,omitempty"`
	Change string `json:"change,omitempty"`

	// change
	N int `json:"n,omitempty"`

	// review
	Commit        string               `json:"commit,omitempty"`
	Kind          string               `json:"kind,omitempty"` // review kind, or reply kind for reply and manual_reply
	Round         *int                 `json:"round,omitempty"`
	Stale         bool                 `json:"stale,omitempty"`
	Findings      []ReviewFindingState `json:"findings,omitempty"`
	ManualRulings []RecordManualRuling `json:"manual_rulings,omitempty"`

	// finding, reply
	Finding    string  `json:"finding,omitempty"`
	Score      int     `json:"score,omitempty"`
	DeferredTo *string `json:"deferred_to,omitempty"`
	Head       string  `json:"head,omitempty"`

	// manual_reply
	Comment string `json:"comment,omitempty"`

	// status, approved_commits
	Event       StatusEvent `json:"event,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	Claim       int         `json:"claim,omitempty"`
	FindingIDs  []string    `json:"finding_ids,omitempty"`
	CommentIDs  []string    `json:"comment_ids,omitempty"`
	Commits     []string    `json:"commits,omitempty"`
	RoundsAdded *int        `json:"rounds_added,omitempty"`

	// task, plan, proposal
	Plan  string         `json:"plan,omitempty"`
	Task  string         `json:"task,omitempty"`
	Tasks []PlanTaskLink `json:"tasks,omitempty"`
}

// ReviewFindingState is one finding's state in a review record (§6.3).
type ReviewFindingState struct {
	ID      string        `json:"id"`
	Score   int           `json:"score"`
	Status  FindingStatus `json:"status"`
	Comment string        `json:"comment,omitempty"`
}

// RecordManualRuling is one manual comment ruling in a review record.
type RecordManualRuling struct {
	Comment string            `json:"comment"`
	Ruling  ManualRulingValue `json:"ruling"`
}

// PlanTaskLink links a plan task to its created item (§7, D15).
type PlanTaskLink struct {
	Task string `json:"task"`
	Item string `json:"item"`
}

const (
	markerPrefix = "<!-- agent-loop:v1 "
	markerSuffix = " -->"
)

// ErrMalformedMarker is returned for text that isn't a valid record marker.
var ErrMalformedMarker = errors.New("malformed record marker")

func (r Record) check() error {
	if !recordTypes[r.Type] {
		return fmt.Errorf("unknown record type %q", r.Type)
	}
	if r.Key == "" {
		return errors.New("record has no key")
	}
	if r.V != 0 && r.V != 1 {
		return fmt.Errorf("record v is %d, not 1", r.V)
	}
	return nil
}

// EncodeMarker returns the one-line marker for rec (INTEGRATION-SPEC §6.1):
// <!-- agent-loop:v1 {compact json} -->. JSON's HTML escaping keeps "-->"
// and "<" out of the JSON, so the comment can't be closed early (D14).
func EncodeMarker(rec Record) (string, error) {
	if err := rec.check(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformedMarker, err)
	}
	b, err := json.Marshal(rec) // json.Marshal escapes <, > and &
	if err != nil {
		return "", err
	}
	return markerPrefix + string(b) + markerSuffix, nil
}

// DecodeMarker parses one marker line, exactly as EncodeMarker writes it.
// Surrounding whitespace is ignored. Anything else is ErrMalformedMarker.
func DecodeMarker(line string) (Record, error) {
	s := strings.TrimSpace(line)
	if strings.ContainsAny(s, "\r\n") {
		return Record{}, fmt.Errorf("%w: more than one line", ErrMalformedMarker)
	}
	if !strings.HasPrefix(s, markerPrefix) || !strings.HasSuffix(s, markerSuffix) ||
		len(s) < len(markerPrefix)+len(markerSuffix) {
		return Record{}, fmt.Errorf("%w: not <!-- agent-loop:v1 {json} -->", ErrMalformedMarker)
	}
	js := s[len(markerPrefix) : len(s)-len(markerSuffix)]
	if !strings.HasPrefix(js, "{") || !json.Valid([]byte(js)) {
		return Record{}, fmt.Errorf("%w: not a JSON object", ErrMalformedMarker)
	}
	var rec Record
	if err := json.Unmarshal([]byte(js), &rec); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrMalformedMarker, err)
	}
	if err := rec.check(); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrMalformedMarker, err)
	}
	return rec, nil
}

// ErrNoMarker is returned by FindMarker for a body that doesn't end with a
// marker.
var ErrNoMarker = errors.New("no record marker")

// AppendMarker returns body with rec's marker as its last line.
func AppendMarker(body string, rec Record) (string, error) {
	m, err := EncodeMarker(rec)
	if err != nil {
		return "", err
	}
	body = strings.TrimRight(body, " \t\r\n")
	if body == "" {
		return m, nil
	}
	return body + "\n\n" + m, nil
}

// FindMarker reads the record from a body's last non-blank line, where every
// record's marker is (§6.1). A marker anywhere else in the text is ignored
// (D14). A body whose last line isn't a marker returns ErrNoMarker; one whose
// last line looks like a marker but isn't valid returns ErrMalformedMarker.
// The caller still checks the author (§6.1).
func FindMarker(body string) (Record, error) {
	body = strings.TrimRight(body, " \t\r\n")
	last := body
	if i := strings.LastIndexAny(body, "\r\n"); i >= 0 {
		last = body[i+1:]
	}
	last = strings.TrimSpace(last)
	if !strings.HasPrefix(last, "<!-- agent-loop:") {
		return Record{}, ErrNoMarker
	}
	return DecodeMarker(last)
}
