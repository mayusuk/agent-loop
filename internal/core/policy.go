package core

import (
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// Policy is the profile's limits as the core uses them (TECHNICAL-SPEC §4).
// The workflow fills it from profile.Policy, which core can't import. A zero
// value uses the TECHNICAL-SPEC §13 and FR-16 AC1 defaults (work plan D2).
type Policy struct {
	MaxRounds         int
	ResumeRounds      int
	BlockingThreshold int
	MaxBaseUpdates    int
	// Stages holds each stage's limits, keyed by stage (implement, review,
	// address, merge, mint, materialise; work plan #155 D9).
	Stages map[contract.Stage]StageLimits
}

// StageLimits are a stage's time and attempt limits (FR-16 AC1).
type StageLimits struct {
	TimeLimit time.Duration
	Attempts  int
}

// The defaults (TECHNICAL-SPEC §13, FR-16 AC1).
const (
	DefaultMaxRounds         = 3
	DefaultResumeRounds      = 1
	DefaultBlockingThreshold = 4
	DefaultMaxBaseUpdates    = 3
	DefaultTimeLimit         = 2 * time.Hour
	DefaultAttempts          = 2
)

func orDefault(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func (p Policy) maxRounds() int    { return orDefault(p.MaxRounds, DefaultMaxRounds) }
func (p Policy) resumeRounds() int { return orDefault(p.ResumeRounds, DefaultResumeRounds) }
func (p Policy) blockingThreshold() int {
	return orDefault(p.BlockingThreshold, DefaultBlockingThreshold)
}
func (p Policy) maxBaseUpdates() int { return orDefault(p.MaxBaseUpdates, DefaultMaxBaseUpdates) }

func (p Policy) limits(st contract.Stage) StageLimits {
	l := p.Stages[st]
	if l.TimeLimit <= 0 {
		l.TimeLimit = DefaultTimeLimit
	}
	if l.Attempts <= 0 {
		l.Attempts = DefaultAttempts
	}
	return l
}
