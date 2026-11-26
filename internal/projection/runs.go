package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// StageRun is one handler run (stage_runs, FR-11 AC2). Ended is zero and
// Result empty while it runs.
type StageRun struct {
	RunID   string
	Project string
	Item    string
	Stage   contract.Stage
	Kind    string // the review kind or address mode, if any
	Attempt int
	Host    string
	Started time.Time
	Ended   time.Time
	// Result is the run's outcome (contract.Outcome), or how it ended
	// without one, for example "timed_out" or "crashed".
	Result  string
	LogPath string // the redacted log
}

// StartRun records a run's start. A retry with the same run ID changes
// nothing.
func (p *DB) StartRun(ctx context.Context, r StageRun) error {
	switch {
	case r.RunID == "" || r.Project == "" || r.Item == "" || r.Stage == "":
		return errors.New("projection: start run: needs a run ID, project, item and stage")
	case r.Started.IsZero():
		return errors.New("projection: start run: no start time")
	}
	_, err := p.db.ExecContext(ctx, `INSERT INTO stage_runs
		(run_id, project, item, stage, kind, attempt, host, started_at, ended_at, result, log_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?)
		ON CONFLICT (run_id) DO NOTHING`,
		r.RunID, r.Project, r.Item, string(r.Stage), r.Kind, r.Attempt, r.Host, nanos(r.Started), r.LogPath)
	if err != nil {
		return fmt.Errorf("projection: start run: %w", err)
	}
	return nil
}

// SetRunHost records the host running a started run (FR-11 AC1, work
// plan #105 D3). A run that was never started is ports.ErrNotFound.
func (p *DB) SetRunHost(ctx context.Context, runID, host string) error {
	if host == "" {
		return errors.New("projection: run host: no host")
	}
	res, err := p.db.ExecContext(ctx, `UPDATE stage_runs SET host = ? WHERE run_id = ?`, host, runID)
	if err != nil {
		return fmt.Errorf("projection: run host: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("projection: run host %s: %w", runID, ports.ErrNotFound)
	}
	return nil
}

// FinishRun records a run's end, its result and its log. Finishing a run
// again overwrites the end. A run that was never started is ports.ErrNotFound.
func (p *DB) FinishRun(ctx context.Context, runID string, ended time.Time, result, logPath string) error {
	if ended.IsZero() || result == "" {
		return errors.New("projection: finish run: needs an end time and a result")
	}
	res, err := p.db.ExecContext(ctx, `UPDATE stage_runs SET ended_at = ?, result = ?,
		log_path = CASE WHEN ? = '' THEN log_path ELSE ? END WHERE run_id = ?`,
		nanos(ended), result, logPath, logPath, runID)
	if err != nil {
		return fmt.Errorf("projection: finish run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("projection: finish run %s: %w", runID, ports.ErrNotFound)
	}
	return nil
}

// Runs returns an item's runs, oldest first.
func (p *DB) Runs(ctx context.Context, project, item string) ([]StageRun, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT run_id, stage, kind, attempt, host, started_at,
		ended_at, result, log_path FROM stage_runs WHERE project = ? AND item = ?
		ORDER BY started_at, run_id`, project, item)
	if err != nil {
		return nil, fmt.Errorf("projection: runs: %w", err)
	}
	defer rows.Close()
	var out []StageRun
	for rows.Next() {
		r := StageRun{Project: project, Item: item}
		var stage string
		var start, end int64
		if err := rows.Scan(&r.RunID, &stage, &r.Kind, &r.Attempt, &r.Host, &start, &end, &r.Result, &r.LogPath); err != nil {
			return nil, fmt.Errorf("projection: runs: %w", err)
		}
		r.Stage, r.Started, r.Ended = contract.Stage(stage), fromNanos(start), fromNanos(end)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: runs: %w", err)
	}
	return out, nil
}

// agentStages are the stages whose runs count toward the daily limit
// (FR-16 AC3): every handler stage. A base update with no agent and the
// merge stage don't count, even if a caller records them (work plan D7).
var agentStages = []contract.Stage{contract.StageImplement, contract.StageReview, contract.StageAddress, contract.StageMint}

// DailyCount rebuilds a project's daily agent-run count from stage_runs
// (TECHNICAL-SPEC §10): the agent runs started on day's date in loc, from
// 00:00 to the next 00:00 in loc (FR-16 AC4). A nil loc is the server's
// local time zone.
func (p *DB) DailyCount(ctx context.Context, project string, day time.Time, loc *time.Location) (int, error) {
	if loc == nil {
		loc = time.Local
	}
	d := day.In(loc)
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	var n int
	err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM stage_runs
		WHERE project = ? AND started_at >= ? AND started_at < ? AND stage IN (?, ?, ?, ?)`,
		project, start.UnixNano(), end.UnixNano(),
		string(agentStages[0]), string(agentStages[1]), string(agentStages[2]), string(agentStages[3])).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("projection: daily count: %w", err)
	}
	return n, nil
}

// LogEntry is one line of an item's log: a decision, a run start or a run
// end (FR-11 AC2).
type LogEntry struct {
	At       time.Time
	Decision *DecisionRow // set for a decision
	Run      *StageRun    // set for a run start or end
	RunEnd   bool         // the entry is Run's end, not its start
}

// ItemLog returns an item's decisions and run starts and ends merged in
// time order (FR-11 AC2). Escalations, merges and retries are the
// decisions that made them. Entries at the same time keep the order run
// start, decision, run end.
func (p *DB) ItemLog(ctx context.Context, project, item string) ([]LogEntry, error) {
	ds, err := p.Decisions(ctx, project, item)
	if err != nil {
		return nil, err
	}
	rs, err := p.Runs(ctx, project, item)
	if err != nil {
		return nil, err
	}
	var out []LogEntry
	for _, r := range rs {
		out = append(out, LogEntry{At: r.Started, Run: &r})
		if !r.Ended.IsZero() {
			out = append(out, LogEntry{At: r.Ended, Run: &r, RunEnd: true})
		}
	}
	for _, d := range ds {
		out = append(out, LogEntry{At: d.Record.At, Decision: &d})
	}
	rank := func(e LogEntry) int {
		switch {
		case e.Decision != nil:
			return 1
		case e.RunEnd:
			return 2
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b LogEntry) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return rank(a) - rank(b)
	})
	return out, nil
}

// Host is a worker host (hosts table, FR-11 AC4).
type Host struct {
	Name         string
	Capabilities []string
	LastSeen     time.Time
	LastCheck    string // the last host check's result
}

// HostHealthWindow is how recently a host must have been seen to be
// healthy (TECHNICAL-SPEC §6.4, work plan #162 D2).
const HostHealthWindow = time.Minute

// Healthy reports whether the host was seen within HostHealthWindow of
// now. The last host check doesn't count in v0.2 (plan D4, work plan #162
// D6).
func (h Host) Healthy(now time.Time) bool {
	return !h.LastSeen.IsZero() && now.Sub(h.LastSeen) <= HostHealthWindow
}

// Covers reports whether the host has every capability in caps. No caps
// means the default, agent (TECHNICAL-SPEC §6.4).
func (h Host) Covers(caps []string) bool {
	if len(caps) == 0 {
		caps = []string{"agent"}
	}
	for _, c := range caps {
		if !slices.Contains(h.Capabilities, c) {
			return false
		}
	}
	return true
}

// UpsertHost records a host's capabilities, last-seen time and last check.
func (p *DB) UpsertHost(ctx context.Context, h Host) error {
	if h.Name == "" {
		return errors.New("projection: host: no name")
	}
	caps, err := json.Marshal(orEmpty(h.Capabilities))
	if err != nil {
		return fmt.Errorf("projection: host: %w", err)
	}
	_, err = p.db.ExecContext(ctx, `INSERT INTO hosts (name, capabilities, last_seen, last_check)
		VALUES (?, ?, ?, ?) ON CONFLICT (name) DO UPDATE SET capabilities = excluded.capabilities,
		last_seen = excluded.last_seen, last_check = excluded.last_check`,
		h.Name, string(caps), nanos(h.LastSeen), h.LastCheck)
	if err != nil {
		return fmt.Errorf("projection: host: %w", err)
	}
	return nil
}

// Hosts returns every host, by name.
func (p *DB) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT name, capabilities, last_seen, last_check FROM hosts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("projection: hosts: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		var h Host
		var caps string
		var seen int64
		if err := rows.Scan(&h.Name, &caps, &seen, &h.LastCheck); err != nil {
			return nil, fmt.Errorf("projection: hosts: %w", err)
		}
		if err := json.Unmarshal([]byte(caps), &h.Capabilities); err != nil {
			return nil, fmt.Errorf("projection: hosts: %w", err)
		}
		h.Capabilities = orNil(h.Capabilities)
		h.LastSeen = fromNanos(seen)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: hosts: %w", err)
	}
	return out, nil
}
