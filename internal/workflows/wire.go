package workflows

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

// Workflow IDs (TECHNICAL-SPEC §6.1).
const DeploymentID = "deployment"

// ProjectID is a project's ProjectWorkflow ID.
func ProjectID(project string) string { return "project/" + project }

// Signal, query and queue names.
const (
	// Item signals: an event for core.Decide, and the project's answer to
	// an admission request.
	SignalEvent     = "event"
	SignalAdmission = "admission"

	// Project signals.
	SignalAdmit           = "admit"
	SignalSpent           = "spent"
	SignalItemEnded       = "item_ended"
	SignalPause           = "pause"
	SignalDeploymentPause = "deployment_pause"

	// Deployment signals (SignalPause too).
	SignalRegister = "register"

	// QueryState is every workflow's state query, for the board.
	QueryState = "state"

	// CoreQueue is the task queue of the workflows and the adapter-only
	// activities; every worker polls it (TECHNICAL-SPEC §6.4).
	CoreQueue = "core"
)

// CapabilityQueue is the task queue of a stage that requires caps: "cap:"
// and the sorted set joined with "+", for example cap:agent (TECHNICAL-SPEC
// §6.4). No capabilities means the default, agent.
func CapabilityQueue(caps []string) string {
	if len(caps) == 0 {
		caps = []string{"agent"}
	}
	c := slices.Clone(caps)
	slices.Sort(c)
	return "cap:" + strings.Join(slices.Compact(c), "+")
}

// WorkerQueues are the queues a worker with caps polls: core, and every
// stage queue of the profile whose capability set is a subset of caps
// (TECHNICAL-SPEC §6.4).
func WorkerQueues(caps []string, stageCaps map[contract.Stage][]string) []string {
	out := []string{CoreQueue}
	for _, sc := range stageCaps {
		need := sc
		if len(need) == 0 {
			need = []string{"agent"}
		}
		ok := true
		for _, c := range need {
			if !slices.Contains(caps, c) {
				ok = false
			}
		}
		if q := CapabilityQueue(sc); ok && !slices.Contains(out, q) {
			out = append(out, q)
		}
	}
	slices.Sort(out[1:])
	return out
}

// ItemSignal is an event for an item's core.Decide, as a signal carries
// it. Exactly one field is set.
type ItemSignal struct {
	MarkerChanged        *core.MarkerChanged        `json:",omitempty"`
	OwnerCommand         *core.OwnerCommand         `json:",omitempty"`
	Assigned             *core.Assigned             `json:",omitempty"`
	ItemClosed           *core.ItemClosed           `json:",omitempty"`
	ChangeClosed         *core.ChangeClosed         `json:",omitempty"`
	ChangeMerged         *core.ChangeMerged         `json:",omitempty"`
	ChangeReopened       *core.ChangeReopened       `json:",omitempty"`
	Commented            *core.Commented            `json:",omitempty"`
	HeadMoved            *core.HeadMoved            `json:",omitempty"`
	ManualCommentChanged *core.ManualCommentChanged `json:",omitempty"`
	DependencyClosed     *core.DependencyClosed     `json:",omitempty"`
	BlockRemoved         *core.BlockRemoved         `json:",omitempty"`
	Resumed              *core.Resumed              `json:",omitempty"`
	DayBoundary          *core.DayBoundary          `json:",omitempty"`
	ProjectResumed       *core.ProjectResumed       `json:",omitempty"`
}

// Event returns the signal's event, or nil if none is set.
func (s ItemSignal) Event() core.Event {
	switch {
	case s.MarkerChanged != nil:
		return *s.MarkerChanged
	case s.OwnerCommand != nil:
		return *s.OwnerCommand
	case s.Assigned != nil:
		return *s.Assigned
	case s.ItemClosed != nil:
		return *s.ItemClosed
	case s.ChangeClosed != nil:
		return *s.ChangeClosed
	case s.ChangeMerged != nil:
		return *s.ChangeMerged
	case s.ChangeReopened != nil:
		return *s.ChangeReopened
	case s.Commented != nil:
		return *s.Commented
	case s.HeadMoved != nil:
		return *s.HeadMoved
	case s.ManualCommentChanged != nil:
		return *s.ManualCommentChanged
	case s.DependencyClosed != nil:
		return *s.DependencyClosed
	case s.BlockRemoved != nil:
		return *s.BlockRemoved
	case s.Resumed != nil:
		return *s.Resumed
	case s.DayBoundary != nil:
		return *s.DayBoundary
	case s.ProjectResumed != nil:
		return *s.ProjectResumed
	}
	return nil
}

// Signal wraps e for an item signal. An event that can't be signalled (a
// stage outcome, which only the item's own activities produce) is an error.
func Signal(e core.Event) (ItemSignal, error) {
	var s ItemSignal
	switch e := e.(type) {
	case core.MarkerChanged:
		s.MarkerChanged = &e
	case core.OwnerCommand:
		s.OwnerCommand = &e
	case core.Assigned:
		s.Assigned = &e
	case core.ItemClosed:
		s.ItemClosed = &e
	case core.ChangeClosed:
		s.ChangeClosed = &e
	case core.ChangeMerged:
		s.ChangeMerged = &e
	case core.ChangeReopened:
		s.ChangeReopened = &e
	case core.Commented:
		s.Commented = &e
	case core.HeadMoved:
		s.HeadMoved = &e
	case core.ManualCommentChanged:
		s.ManualCommentChanged = &e
	case core.DependencyClosed:
		s.DependencyClosed = &e
	case core.BlockRemoved:
		s.BlockRemoved = &e
	case core.Resumed:
		s.Resumed = &e
	case core.DayBoundary:
		s.DayBoundary = &e
	case core.ProjectResumed:
		s.ProjectResumed = &e
	default:
		return s, fmt.Errorf("workflows: %T can't be signalled", e)
	}
	return s, nil
}

// MustSignal is Signal for events known to be signallable.
func MustSignal(e core.Event) ItemSignal {
	s, err := Signal(e)
	if err != nil {
		panic(err)
	}
	return s
}

// AdmitRequest is an item asking the project to start a stage
// (TECHNICAL-SPEC §6.3). Agent is set for a handler stage, which needs a
// daily-limit unit.
type AdmitRequest struct {
	WorkflowID string
	Item       string
	RunID      string
	Agent      bool
}

// AdmissionReply is the project's answer: granted or waiting_limit. A
// paused project holds the request and answers when the pause ends (work
// plan D5).
type AdmissionReply struct {
	RunID     string
	Admission core.Admission
}

// SpendReport is a run's cost, added to the project's daily spend (FR-16
// AC3, work plan D2).
type SpendReport struct {
	Item  string
	RunID string
	Cost  float64
	At    time.Time
}

// ItemEnded tells the project an item workflow ended, which frees its WIP
// slot (FR-2 AC3).
type ItemEnded struct {
	WorkflowID string
	Item       string
	Reason     string
}

// PauseSignal sets or clears a pause.
type PauseSignal struct{ Paused bool }

// RegisterSignal registers a project with the deployment, which answers
// with its pause and signals every later change (work plan D6).
type RegisterSignal struct{ WorkflowID string }
