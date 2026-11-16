package contract

import (
	"encoding/json"
	"fmt"
)

// Stage names a handler stage (INTEGRATION-SPEC §4.2).
type Stage string

// The handler stages. Materialise has no handler (work plan D19).
const (
	StageImplement Stage = "implement"
	StageReview    Stage = "review"
	StageAddress   Stage = "address"
	StageMint      Stage = "mint"
)

// ReviewKind is the kind of a review (FR-6 AC6).
type ReviewKind string

// The review kinds.
const (
	KindRound       ReviewKind = "round"
	KindBaseUpdate  ReviewKind = "base_update"
	KindManualReply ReviewKind = "manual_reply"
	KindOwnerPush   ReviewKind = "owner_push"
)

// AddressMode is the mode of an address run (INTEGRATION-SPEC §4.2).
type AddressMode string

// The address modes.
const (
	ModeFindings   AddressMode = "findings"
	ModeManual     AddressMode = "manual"
	ModeBaseUpdate AddressMode = "base_update"
)

// Input is input.json (INTEGRATION-SPEC §4). It holds references only.
type Input struct {
	ContractVersion string        `json:"contract_version"`
	RunID           string        `json:"run_id"`
	Stage           Stage         `json:"stage"`
	Attempt         int           `json:"attempt"`
	Deadline        string        `json:"deadline"`
	Identity        string        `json:"identity"`
	Project         InputProject  `json:"project"`
	Repo            InputRepo     `json:"repo"`
	Workspace       Workspace     `json:"workspace"`
	Item            ItemRef       `json:"item"`
	Change          *ChangeRef    `json:"change"`
	Review          *ReviewInput  `json:"review,omitempty"`
	Address         *AddressInput `json:"address,omitempty"`
	Mint            *MintInput    `json:"mint,omitempty"`
}

// InputProject names the project and its adapters.
type InputProject struct {
	Name     string `json:"name"`
	Tracker  string `json:"tracker"`
	CodeHost string `json:"code_host"`
}

// InputRepo is the project repository.
type InputRepo struct {
	URL        string `json:"url"`
	BaseBranch string `json:"base_branch"`
}

// Workspace is the run's worktree (INTEGRATION-SPEC §3.3).
type Workspace struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

// ItemRef refers to the tracker item.
type ItemRef struct {
	Ref  string `json:"ref"`
	URL  string `json:"url"`
	Type string `json:"type"`
}

// ChangeRef refers to the item's change.
type ChangeRef struct {
	Ref     string `json:"ref"`
	URL     string `json:"url"`
	Head    string `json:"head"`
	BaseTip string `json:"base_tip"`
}

// ReviewInput is the review stage's input.
type ReviewInput struct {
	Kind              ReviewKind `json:"kind"`
	Commit            string     `json:"commit"`
	Round             *int       `json:"round"`
	BlockingThreshold int        `json:"blocking_threshold"`
	RuleFindings      []string   `json:"rule_findings"`
	RuleManual        []string   `json:"rule_manual"`
	ResolvedPaths     []string   `json:"resolved_paths,omitempty"`
}

// AddressInput is the address stage's input.
type AddressInput struct {
	Mode           AddressMode `json:"mode"`
	AnswerFindings []string    `json:"answer_findings"`
	AnswerManual   []string    `json:"answer_manual"`
}

// MintInput is the mint stage's input.
type MintInput struct {
	PlanKey       string   `json:"plan_key"`
	SourceOfTruth []string `json:"source_of_truth"`
	PlansDir      string   `json:"plans_dir"`
}

// ParseInput decodes input.json. Unknown fields are ignored, so a handler
// written for 1.0 reads a 1.x input (INTEGRATION-SPEC §2). Another major
// version is rejected (work plan D2).
func ParseInput(data []byte) (Input, error) {
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return Input{}, fmt.Errorf("input.json: %w", err)
	}
	major, _, err := ParseVersion(in.ContractVersion)
	if err != nil {
		return Input{}, fmt.Errorf("input.json: %w: %v", ErrUnsupportedVersion, err)
	}
	if major != Major {
		return Input{}, fmt.Errorf("input.json: %w: %s", ErrUnsupportedVersion, in.ContractVersion)
	}
	return in, nil
}
