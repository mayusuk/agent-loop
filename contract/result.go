package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Outcome is a stage result (FR-18 AC1).
type Outcome string

// The stage results.
const (
	OutcomeDone       Outcome = "done"
	OutcomeComplete   Outcome = "complete"
	OutcomeBlocked    Outcome = "blocked"
	OutcomeFailed     Outcome = "failed"
	OutcomeNeedsOwner Outcome = "needs_owner"
)

// allowedOutcomes lists the results each handler stage may return
// (FR-18 AC1).
var allowedOutcomes = map[Stage][]Outcome{
	StageImplement: {OutcomeDone, OutcomeBlocked, OutcomeFailed, OutcomeNeedsOwner},
	StageReview:    {OutcomeDone, OutcomeFailed, OutcomeNeedsOwner},
	StageAddress:   {OutcomeDone, OutcomeBlocked, OutcomeFailed, OutcomeNeedsOwner},
	StageMint:      {OutcomeDone, OutcomeComplete, OutcomeFailed, OutcomeNeedsOwner},
}

// Allowed reports whether stage s may return outcome o (FR-18 AC1).
func Allowed(s Stage, o Outcome) bool {
	for _, a := range allowedOutcomes[s] {
		if a == o {
			return true
		}
	}
	return false
}

// Result is result.json (INTEGRATION-SPEC §5). Exactly one of the result
// objects is set: the one Result names.
type Result struct {
	ContractVersion string      `json:"contract_version"`
	RunID           string      `json:"run_id"`
	Result          Outcome     `json:"result"`
	Done            *Done       `json:"done,omitempty"`
	Complete        *Complete   `json:"complete,omitempty"`
	Blocked         *Blocked    `json:"blocked,omitempty"`
	Failed          *Failed     `json:"failed,omitempty"`
	NeedsOwner      *NeedsOwner `json:"needs_owner,omitempty"`
	Usage           *Usage      `json:"usage,omitempty"`
}

// Done holds the stage's done object. Exactly one field is set, for the
// input's stage. It is decoded by ParseResult, which knows the stage.
type Done struct {
	Implement *ImplementDone
	Review    *ReviewDone
	Address   *AddressDone
	Mint      *MintDone
}

// MarshalJSON writes the one stage object that is set.
func (d Done) MarshalJSON() ([]byte, error) {
	var v any
	n := 0
	if d.Implement != nil {
		v, n = d.Implement, n+1
	}
	if d.Review != nil {
		v, n = d.Review, n+1
	}
	if d.Address != nil {
		v, n = d.Address, n+1
	}
	if d.Mint != nil {
		v, n = d.Mint, n+1
	}
	if n != 1 {
		return nil, fmt.Errorf("done must hold exactly one stage object, has %d", n)
	}
	return json.Marshal(v)
}

// Complete is mint's complete object. It has no fields.
type Complete struct{}

// Blocked is the blocked object.
type Blocked struct {
	ReasonFile string  `json:"reason_file"`
	Dependency *string `json:"dependency,omitempty"`
}

// Failed is the failed object. Reason is one line, stored in the log only.
type Failed struct {
	Reason string `json:"reason"`
}

// NeedsOwner is the needs_owner object.
type NeedsOwner struct {
	QuestionFile string `json:"question_file"`
}

// Usage reports what a run spent (INTEGRATION-SPEC §5.1). Every field is
// optional.
type Usage struct {
	Cost         *float64 `json:"cost,omitempty"`
	Currency     *string  `json:"currency,omitempty"`
	InputTokens  *int64   `json:"input_tokens,omitempty"`
	OutputTokens *int64   `json:"output_tokens,omitempty"`
}

// ImplementDone is implement's done object.
type ImplementDone struct {
	Head   string     `json:"head"`
	Change *NewChange `json:"change"`
}

// NewChange is the change the worker opens.
type NewChange struct {
	Title    string `json:"title"`
	BodyFile string `json:"body_file"`
}

// ReviewDone is review's done object.
type ReviewDone struct {
	Commit        string         `json:"commit"`
	Kind          ReviewKind     `json:"kind"`
	SummaryFile   string         `json:"summary_file"`
	Rulings       []Ruling       `json:"rulings,omitempty"`
	ManualRulings []ManualRuling `json:"manual_rulings,omitempty"`
	NewFindings   []NewFinding   `json:"new_findings,omitempty"`
}

// FindingStatus is a finding's status (FR-4, FR-5 AC4).
type FindingStatus string

// The finding statuses.
const (
	StatusOpen               FindingStatus = "open"
	StatusPartiallyAddressed FindingStatus = "partially_addressed"
	StatusAddressed          FindingStatus = "addressed"
	StatusWithdrawn          FindingStatus = "withdrawn"
)

// ManualRulingValue is a ruling on a manual comment reply (FR-4 AC8).
type ManualRulingValue string

// The manual comment rulings.
const (
	RulingAddressed ManualRulingValue = "addressed"
	RulingOpen      ManualRulingValue = "open"
)

// Ruling rules on one earlier finding.
type Ruling struct {
	Finding  string        `json:"finding"`
	Status   FindingStatus `json:"status"`
	NoteFile string        `json:"note_file,omitempty"`
}

// ManualRuling rules on one manual comment's reply.
type ManualRuling struct {
	Comment  string            `json:"comment"`
	Ruling   ManualRulingValue `json:"ruling"`
	NoteFile string            `json:"note_file,omitempty"`
}

// NewFinding is a finding a review adds. Path and Line may be null.
type NewFinding struct {
	Score    int     `json:"score"`
	Path     *string `json:"path"`
	Line     *int    `json:"line"`
	Title    string  `json:"title"`
	BodyFile string  `json:"body_file"`
}

// ReplyKind is a coder reply kind (FR-5 AC1).
type ReplyKind string

// The reply kinds.
const (
	ReplyFixed     ReplyKind = "fixed"
	ReplyExplained ReplyKind = "explained"
	ReplyDeferred  ReplyKind = "deferred"
	ReplyNotValid  ReplyKind = "not_valid"
	ReplyNeedsInfo ReplyKind = "needs_info"
)

// AddressDone is address's done object.
type AddressDone struct {
	Head          string        `json:"head"`
	Replies       []Reply       `json:"replies,omitempty"`
	ManualReplies []ManualReply `json:"manual_replies,omitempty"`
}

// Reply answers one finding.
type Reply struct {
	Finding    string    `json:"finding"`
	Kind       ReplyKind `json:"kind"`
	DeferredTo *string   `json:"deferred_to,omitempty"`
	BodyFile   string    `json:"body_file"`
}

// ManualReply answers one manual comment.
type ManualReply struct {
	Comment    string    `json:"comment"`
	Kind       ReplyKind `json:"kind"`
	DeferredTo *string   `json:"deferred_to,omitempty"`
	BodyFile   string    `json:"body_file"`
}

// MintDone is mint's done object.
type MintDone struct {
	PlanKey  string       `json:"plan_key"`
	Head     string       `json:"head"`
	Proposal *NewProposal `json:"proposal"`
}

// NewProposal is the plan proposal the worker opens.
type NewProposal struct {
	Title    string `json:"title"`
	BodyFile string `json:"body_file"`
}

// wireResult is Result as read, before each object is decoded. Objects are
// kept raw so that an explicit null is seen as present, as the schema sees
// it.
type wireResult struct {
	ContractVersion string          `json:"contract_version"`
	RunID           string          `json:"run_id"`
	Result          Outcome         `json:"result"`
	Done            json.RawMessage `json:"done"`
	Complete        json.RawMessage `json:"complete"`
	Blocked         json.RawMessage `json:"blocked"`
	Failed          json.RawMessage `json:"failed"`
	NeedsOwner      json.RawMessage `json:"needs_owner"`
	Usage           json.RawMessage `json:"usage"`
}

// decodeStrict decodes exactly one JSON value into v, rejecting unknown
// fields and trailing data (INTEGRATION-SPEC §2).
//
// encoding/json matches keys to fields without regard to case, so the keys
// are first checked against the exact json tag names (review R1).
func decodeStrict(data []byte, v any) error {
	if err := checkKeys(data, reflect.TypeOf(v)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	return nil
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// checkKeys walks data along type t and fails on the first object key, in
// sorted order, that isn't exactly a json tag name of the struct it decodes
// into. Values that don't fit t are left for the decoder to report.
func checkKeys(data []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var m map[string]json.RawMessage
		if json.Unmarshal(data, &m) != nil || m == nil {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ft, ok := fields[k]
			if !ok {
				return fmt.Errorf("json: unknown field %q", k)
			}
			if err := checkKeys(m[k], ft); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return nil
		}
		for _, it := range items {
			if err := checkKeys(it, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeObject decodes an optional JSON object into a new T. It returns nil
// when raw is absent, and an error for null or anything but an object.
func decodeObject[T any](name string, raw json.RawMessage) (*T, error) {
	if raw == nil {
		return nil, nil
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return nil, fmt.Errorf("%s is not an object", name)
	}
	v := new(T)
	if err := decodeStrict(raw, v); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return v, nil
}

// ParseResult decodes result.json for stage s. Unknown fields anywhere are
// rejected (INTEGRATION-SPEC §2). It checks the version and the shape only;
// ValidateResult applies the rules.
func ParseResult(s Stage, data []byte) (Result, error) {
	if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' {
		return Result{}, errors.New("result is not a JSON object")
	}
	// Peek at the exact key contract_version (not a case variant, R1).
	var peek map[string]json.RawMessage
	if err := json.Unmarshal(data, &peek); err != nil {
		return Result{}, fmt.Errorf("result is not valid JSON: %w", err)
	}
	rawVersion, ok := peek["contract_version"]
	if !ok {
		return Result{}, errors.New("contract_version is missing")
	}
	var version string
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return Result{}, errors.New("contract_version is not a string")
	}
	// Check the version before the strict decode, so a newer minor's extra
	// fields are reported as an unsupported version, not as unknown fields.
	if err := CheckVersion(version); err != nil {
		return Result{}, err
	}
	var w wireResult
	if err := decodeStrict(data, &w); err != nil {
		return Result{}, fmt.Errorf("result does not match the schema: %w", err)
	}
	r := Result{ContractVersion: w.ContractVersion, RunID: w.RunID, Result: w.Result}
	var err error
	if r.Complete, err = decodeObject[Complete]("complete", w.Complete); err != nil {
		return Result{}, err
	}
	if r.Blocked, err = decodeObject[Blocked]("blocked", w.Blocked); err != nil {
		return Result{}, err
	}
	if r.Failed, err = decodeObject[Failed]("failed", w.Failed); err != nil {
		return Result{}, err
	}
	if r.NeedsOwner, err = decodeObject[NeedsOwner]("needs_owner", w.NeedsOwner); err != nil {
		return Result{}, err
	}
	if r.Usage, err = decodeObject[Usage]("usage", w.Usage); err != nil {
		return Result{}, err
	}
	if w.Done != nil {
		if t := bytes.TrimSpace(w.Done); len(t) == 0 || t[0] != '{' {
			return Result{}, errors.New("done is not an object")
		}
		if r.Done, err = parseDone(s, w.Done); err != nil {
			return Result{}, fmt.Errorf("done does not match the %s schema: %w", s, err)
		}
	}
	return r, nil
}

func parseDone(s Stage, raw json.RawMessage) (*Done, error) {
	var d Done
	var v any
	switch s {
	case StageImplement:
		d.Implement = &ImplementDone{}
		v = d.Implement
	case StageReview:
		d.Review = &ReviewDone{}
		v = d.Review
	case StageAddress:
		d.Address = &AddressDone{}
		v = d.Address
	case StageMint:
		d.Mint = &MintDone{}
		v = d.Mint
	default:
		return nil, fmt.Errorf("stage %q has no handler", s)
	}
	if err := decodeStrict(raw, v); err != nil {
		return nil, err
	}
	return &d, nil
}
