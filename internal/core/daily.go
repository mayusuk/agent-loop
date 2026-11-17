package core

import "time"

// DefaultDailyAgentRuns is the default daily agent-run limit (FR-16 AC3,
// D35). The limit can't be turned off, so a zero or negative limit uses it.
const DefaultDailyAgentRuns = 20

// DailyCount is the project's agent runs on one day in the profile's time
// zone (FR-16 AC3, AC4). It lives in ProjectWorkflow (TECHNICAL-SPEC §6.2).
type DailyCount struct {
	Day  string // YYYY-MM-DD in the profile time zone
	Runs int
	// Notified: the owner was told the limit was reached today.
	Notified bool
}

// Admission is the project's answer before a stage starts (TECHNICAL-SPEC
// §6.3, work plan #12 D12).
type Admission string

// The admissions. AdmitPaused holds the start until the project and the
// deployment resume; the item stays in its state.
const (
	AdmitGranted      Admission = "granted"
	AdmitWaitingLimit Admission = "waiting_limit"
	AdmitPaused       Admission = "paused"
)

// AdmitRequest asks to start a stage.
type AdmitRequest struct {
	Now time.Time
	// Agent is set for an agent stage, which needs a daily-limit unit.
	// Merge and the agent-free base merge don't (TECHNICAL-SPEC §8.1).
	Agent bool
	// Paused: the project or the deployment is paused (FR-10 AC3).
	Paused bool
	Limit  int
	Loc    *time.Location
}

func dayOf(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format(time.DateOnly)
}

func limitOr(l int) int {
	if l <= 0 {
		return DefaultDailyAgentRuns
	}
	return l
}

// roll starts a new day's count when now is on a later day.
func (c DailyCount) roll(now time.Time, loc *time.Location) DailyCount {
	if d := dayOf(now, loc); d != c.Day {
		return DailyCount{Day: d}
	}
	return c
}

// Reached reports whether the daily limit is reached at now (FR-2 AC1).
func (c DailyCount) Reached(now time.Time, loc *time.Location, limit int) bool {
	return c.roll(now, loc).Runs >= limitOr(limit)
}

// Admit decides a start request. A pause is checked first and uses no
// unit. An agent stage at the limit waits, and the owner is notified once
// a day (FR-16 AC3). A granted agent stage uses one unit.
func Admit(c DailyCount, r AdmitRequest) (DailyCount, Admission, bool) {
	if r.Paused {
		return c, AdmitPaused, false
	}
	if !r.Agent {
		return c, AdmitGranted, false
	}
	c = c.roll(r.Now, r.Loc)
	if c.Runs >= limitOr(r.Limit) {
		notify := !c.Notified
		c.Notified = true
		return c, AdmitWaitingLimit, notify
	}
	c.Runs++
	return c, AdmitGranted, false
}

// DayReset is the day-boundary timer (FR-16 AC4, TECHNICAL-SPEC §6.5): it
// resets the count. Wake says to signal DayBoundary to every item in
// waiting_limit, and Notify to tell the owner, when the limit was reached
// on the day that ended (work plan #12 D13).
func DayReset(c DailyCount, now time.Time, loc *time.Location) (next DailyCount, wake, notify bool) {
	next = c.roll(now, loc)
	if next.Day == c.Day {
		return c, false, false
	}
	return next, true, c.Notified
}

// NextDayBoundary is the next 00:00 after now in loc (FR-16 AC4).
func NextDayBoundary(now time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}
