package profile

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA names validate the same on every host (D8)
)

// ProfileFileName is the project profile's name in a repository.
const ProfileFileName = ".agent-loop.toml"

// Stage names.
const (
	StageImplement = "implement"
	StageReview    = "review"
	StageAddress   = "address"
	StageMint      = "mint"
	StageMerge     = "merge"
)

// Runner names (TECHNICAL-SPEC §7.2).
const (
	RunnerLocal            = "local"
	RunnerLocalUnsandboxed = "local-unsandboxed"
	RunnerDocker           = "docker"
)

// LoopMarkers are the loop's own markers (INTEGRATION-SPEC §8.1), mapped to
// their default label names (PRODUCT-SPEC §5.1). Type markers are
// "type:<type>" and default to the type's name (D4).
var LoopMarkers = map[string]string{
	"ready":         "ready",
	"in_progress":   "in-progress",
	"blocked":       "blocked",
	"needs_owner":   "needs-owner",
	"paused":        "paused",
	"aborted":       "aborted",
	"follow_up":     "follow-up",
	"plan":          "plan",
	"plan_proposal": "plan-proposal",
}

// TypeMarkerPrefix starts every type marker, for example "type:bug".
const TypeMarkerPrefix = "type:"

// Defaults from TECHNICAL-SPEC §13 and INTEGRATION-SPEC §13.
const (
	DefaultBranchPrefix      = "agent/"
	DefaultPlansDir          = "plans/"
	DefaultWIP               = 2
	DefaultMaxRounds         = 3
	DefaultBlockingThreshold = 4
	DefaultResumeRounds      = 1
	DefaultMaxBaseUpdates    = 3
	DefaultMergeMethod       = "squash"
	DefaultFollowUpType      = "feature"
	DefaultDailyAgentRuns    = 20
	DefaultAgentRuns         = 2
	DefaultSupervisor        = 10 * time.Minute
	DefaultPoll              = 60 * time.Second
	DefaultTimeLimit         = 2 * time.Hour
	DefaultAttempts          = 2
	DefaultRunner            = RunnerLocal
	DefaultAdapter           = "github"
)

var (
	DefaultTypes        = []string{"feature", "bug"}
	DefaultPriority     = []string{"type:bug", "type:feature", "oldest"}
	DefaultCapabilities = []string{"agent"}
)

// Ranges that aren't in the schema because they are durations (D9, §13).
const (
	minSupervisor = time.Minute
	maxSupervisor = 60 * time.Minute
	minPoll       = 15 * time.Second
	maxPoll       = 10 * time.Minute
	minTimeLimit  = time.Minute
	maxTimeLimit  = 24 * time.Hour
)

// DefaultEgress is the egress allowlist of TECHNICAL-SPEC §7.2 that every
// project gets, before the code host's own hosts.
var DefaultEgress = []string{
	"api.anthropic.com",
	"registry.npmjs.org",
	"proxy.golang.org",
	"sum.golang.org",
	"pypi.org",
	"files.pythonhosted.org",
	"crates.io",
	"static.crates.io",
}

// CodeHostEgress lists each code-host adapter's hosts (TECHNICAL-SPEC §7.2).
var CodeHostEgress = map[string][]string{
	"github": {"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com"},
}

// Profile is a loaded, validated project profile with every default filled in.
type Profile struct {
	// Path is the file the profile was loaded from.
	Path string

	Tracker  Adapter
	CodeHost Adapter

	// BaseBranch is empty when the profile leaves it to the repository's
	// default branch.
	BaseBranch   string
	BranchPrefix string
	PlansDir     string

	Types    []string
	Priority []string

	WIP               int
	MaxRounds         int
	BlockingThreshold int
	ResumeRounds      int
	MaxBaseUpdates    int
	MergeMethod       string

	FollowUp FollowUp
	Limits   Limits

	// TimezoneName is empty when the profile uses the server's local time
	// zone. Location is always set.
	TimezoneName string
	Location     *time.Location

	Intervals Intervals

	// Stages holds implement, review, address and merge, and mint when the
	// profile names it.
	Stages map[string]Stage

	SourceOfTruth []string

	// Markers maps every marker, including a type marker for each type, to
	// its tracker representation.
	Markers map[string]MarkerMapping

	Identities Identities

	// Gates is nil when GatesFromBase is true: the base branch's required
	// checks are used (D11).
	Gates         []string
	GatesFromBase bool

	ManualCommentBots []string
	Egress            Egress
	// Caches are more cache variables the runner points into each run's
	// temp directory, after its default list (TECHNICAL-SPEC §7.2).
	Caches      []string
	ReleaseApps []int64
}

// Adapter names a tracker or code-host adapter and its settings.
type Adapter struct {
	Adapter string
	Repo    string
}

type FollowUp struct {
	Type  string
	Ready bool
}

type Limits struct {
	DailyAgentRuns int
	// DailySpend is 0 when there is no spend limit.
	DailySpend float64
	AgentRuns  int
}

type Intervals struct {
	Supervisor time.Duration
	Poll       time.Duration
}

// Stage is one stage's settings. Merge has only TimeLimit and Attempts.
type Stage struct {
	Command      []string
	Contract     string
	Capabilities []string
	Runner       string
	TimeLimit    time.Duration
	Attempts     int
}

// MarkerMapping is a marker's tracker representation: exactly one of Label
// and Status is set.
type MarkerMapping struct {
	Label  string
	Status string
}

// Identities are the loop's three identities, as secret references, and the
// owner accounts.
type Identities struct {
	Coder    string
	Reviewer string
	Loop     string
	Owners   []Owner
}

// Owner is an owner account. ID is the stable account ID and is what counts.
type Owner struct {
	Login string
	ID    string
}

type Egress struct {
	Allow   []string
	Replace bool
	// Effective is the allowlist a run gets (D12).
	Effective []string
}

// raw types mirror the file. Pointers tell "absent" from a zero value.
type rawProfile struct {
	Tracker           rawAdapter           `toml:"tracker"`
	CodeHost          rawAdapter           `toml:"code_host"`
	BaseBranch        *string              `toml:"base_branch"`
	BranchPrefix      *string              `toml:"branch_prefix"`
	PlansDir          *string              `toml:"plans_dir"`
	Types             []string             `toml:"types"`
	Priority          []string             `toml:"priority"`
	WIP               *int                 `toml:"wip"`
	MaxRounds         *int                 `toml:"max_rounds"`
	BlockingThreshold *int                 `toml:"blocking_threshold"`
	ResumeRounds      *int                 `toml:"resume_rounds"`
	MaxBaseUpdates    *int                 `toml:"max_base_updates"`
	MergeMethod       *string              `toml:"merge_method"`
	FollowUp          rawFollowUp          `toml:"follow_up"`
	Limits            rawLimits            `toml:"limits"`
	Timezone          *string              `toml:"timezone"`
	Intervals         rawIntervals         `toml:"intervals"`
	Stages            map[string]rawStage  `toml:"stages"`
	SourceOfTruth     []string             `toml:"source_of_truth"`
	Markers           map[string]rawMarker `toml:"markers"`
	Identities        rawIdentities        `toml:"identities"`
	Gates             *[]string            `toml:"gates"`
	ManualCommentBots []string             `toml:"manual_comment_bots"`
	Egress            rawEgress            `toml:"egress"`
	Caches            []string             `toml:"caches"`
	ReleaseApps       []int64              `toml:"release_apps"`
}

type rawAdapter struct {
	Adapter *string `toml:"adapter"`
	Repo    string  `toml:"repo"`
}

type rawFollowUp struct {
	Type  *string `toml:"type"`
	Ready *bool   `toml:"ready"`
}

type rawLimits struct {
	DailyAgentRuns *int `toml:"daily_agent_runs"`
	DailySpend     any  `toml:"daily_spend"` // integer or float
	AgentRuns      *int `toml:"agent_runs"`
}

type rawIntervals struct {
	Supervisor *string `toml:"supervisor"`
	Poll       *string `toml:"poll"`
}

type rawStage struct {
	Command      []string `toml:"command"`
	Contract     string   `toml:"contract"`
	Capabilities []string `toml:"capabilities"`
	Runner       *string  `toml:"runner"`
	TimeLimit    *string  `toml:"time_limit"`
	Attempts     *int     `toml:"attempts"`
}

type rawMarker struct {
	Label  string `toml:"label"`
	Status string `toml:"status"`
}

type rawIdentities struct {
	Coder    string     `toml:"coder"`
	Reviewer string     `toml:"reviewer"`
	Loop     string     `toml:"loop"`
	Owners   []rawOwner `toml:"owners"`
}

type rawOwner struct {
	Login string `toml:"login"`
	ID    any    `toml:"id"`
}

type rawEgress struct {
	Allow   []string `toml:"allow"`
	Replace *bool    `toml:"replace"`
}

// profileOverrides replace the schema's message for keys whose rule the
// owner should read in plain words.
var profileOverrides = map[string]string{
	"limits.daily_agent_runs": "the daily agent-run limit is always in force: set a whole number from 1 to 1000 (0 and \"off\" are not allowed, FR-16 AC3)",
}

var profileRules = fileRules{
	schema:    profileSchema,
	overrides: profileOverrides,
	fractions: map[string]bool{"limits.daily_spend": true},
}

// LoadProfile loads and validates the project profile at path. dep supplies
// the deployment limits that bound the profile's; nil means the deployment
// defaults. On failure the error is an *Error listing every problem.
func LoadProfile(path string, dep *Deployment) (*Profile, error) {
	ps := &problems{file: path}
	var raw rawProfile
	if !decodeFile(path, profileRules, &raw, ps) {
		return nil, ps.err()
	}
	p := buildProfile(&raw, dep, ps)
	p.Path = path
	if err := ps.err(); err != nil {
		return nil, err
	}
	return p, nil
}

// parseProfile is LoadProfile on bytes, for tests.
func parseProfile(file string, data []byte, dep *Deployment) (*Profile, error) {
	ps := &problems{file: file}
	var raw rawProfile
	if !decodeBytes(data, profileRules, &raw, ps) {
		return nil, ps.err()
	}
	p := buildProfile(&raw, dep, ps)
	p.Path = file
	if err := ps.err(); err != nil {
		return nil, err
	}
	return p, nil
}

func buildProfile(r *rawProfile, dep *Deployment, ps *problems) *Profile {
	if dep == nil {
		dep = DefaultDeployment()
	}
	p := &Profile{
		Tracker:           adapter(r.Tracker),
		CodeHost:          adapter(r.CodeHost),
		BaseBranch:        str(r.BaseBranch, ""),
		BranchPrefix:      str(r.BranchPrefix, DefaultBranchPrefix),
		PlansDir:          str(r.PlansDir, DefaultPlansDir),
		Types:             orDefault(r.Types, DefaultTypes),
		WIP:               num(r.WIP, DefaultWIP),
		MaxRounds:         num(r.MaxRounds, DefaultMaxRounds),
		BlockingThreshold: num(r.BlockingThreshold, DefaultBlockingThreshold),
		ResumeRounds:      num(r.ResumeRounds, DefaultResumeRounds),
		MaxBaseUpdates:    num(r.MaxBaseUpdates, DefaultMaxBaseUpdates),
		MergeMethod:       str(r.MergeMethod, DefaultMergeMethod),
		FollowUp: FollowUp{
			Type:  str(r.FollowUp.Type, DefaultFollowUpType),
			Ready: r.FollowUp.Ready != nil && *r.FollowUp.Ready,
		},
		SourceOfTruth:     slices.Clone(r.SourceOfTruth),
		ManualCommentBots: orDefault(r.ManualCommentBots, []string{}),
		Caches:            orDefault(r.Caches, []string{}),
		ReleaseApps:       orDefault(r.ReleaseApps, []int64{}),
	}

	p.Priority = priority(r.Priority, p.Types, ps)
	if !slices.Contains(p.Types, p.FollowUp.Type) {
		if r.FollowUp.Type == nil {
			ps.add("follow_up.type", fmt.Sprintf("the default type %q is not in types; set follow_up.type to one of %s", p.FollowUp.Type, list(p.Types)))
		} else {
			ps.add("follow_up.type", fmt.Sprintf("%q is not in types (%s)", p.FollowUp.Type, list(p.Types)))
		}
	}

	p.Limits = limits(r.Limits, dep, ps)
	p.TimezoneName, p.Location = timezone(r.Timezone, ps)
	p.Intervals = intervals(r.Intervals, ps)
	p.Stages = stages(r.Stages, ps)
	p.Markers = markers(r.Markers, p.Types, ps)
	p.Identities = identities(r.Identities, ps)

	if r.Gates == nil {
		p.GatesFromBase = true
	} else {
		p.Gates = slices.Clone(*r.Gates)
	}

	p.Egress.Allow = orDefault(r.Egress.Allow, []string{})
	p.Egress.Replace = r.Egress.Replace != nil && *r.Egress.Replace
	p.Egress.Effective = effectiveEgress(p.Egress, p.CodeHost.Adapter)
	return p
}

func adapter(r rawAdapter) Adapter {
	return Adapter{Adapter: str(r.Adapter, DefaultAdapter), Repo: r.Repo}
}

// priority applies D5.
func priority(given, types []string, ps *problems) []string {
	if given == nil {
		var out []string
		for _, m := range DefaultPriority[:len(DefaultPriority)-1] {
			if slices.Contains(types, strings.TrimPrefix(m, TypeMarkerPrefix)) {
				out = append(out, m)
			}
		}
		for _, t := range types {
			if m := TypeMarkerPrefix + t; !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
		return append(out, DefaultPriority[len(DefaultPriority)-1])
	}
	last := len(given) - 1
	if given[last] != "oldest" && given[last] != "newest" {
		ps.add(idx("priority", last), fmt.Sprintf("the last entry must be \"oldest\" or \"newest\", not %q", given[last]))
	}
	seen := map[string]bool{}
	for i, m := range given[:last] {
		t, ok := strings.CutPrefix(m, TypeMarkerPrefix)
		switch {
		case !ok:
			ps.add(idx("priority", i), fmt.Sprintf("%q is not a type marker; entries before the last are type markers such as \"type:bug\"", m))
		case !slices.Contains(types, t):
			ps.add(idx("priority", i), fmt.Sprintf("%q names a type that is not in types (%s)", m, list(types)))
		case seen[m]:
			ps.add(idx("priority", i), fmt.Sprintf("%q is listed twice", m))
		}
		seen[m] = true
	}
	return slices.Clone(given)
}

func limits(r rawLimits, dep *Deployment, ps *problems) Limits {
	l := Limits{DailyAgentRuns: num(r.DailyAgentRuns, DefaultDailyAgentRuns)}
	switch v := r.DailySpend.(type) {
	case int64:
		l.DailySpend = float64(v)
	case float64:
		l.DailySpend = v
	}
	max := dep.Limits.AgentRuns
	if r.AgentRuns == nil {
		l.AgentRuns = min(DefaultAgentRuns, max) // D7
	} else {
		l.AgentRuns = *r.AgentRuns
		if l.AgentRuns > max {
			ps.add("limits.agent_runs", fmt.Sprintf("%d is more than the deployment limit of %d", l.AgentRuns, max))
		}
	}
	return l
}

func timezone(name *string, ps *problems) (string, *time.Location) {
	if name == nil {
		return "", time.Local
	}
	if *name == "Local" {
		ps.add("timezone", "must be an IANA time zone name such as \"Europe/Berlin\"; omit the key for the server's local time zone")
		return *name, time.Local
	}
	loc, err := time.LoadLocation(*name)
	if err != nil {
		ps.add("timezone", fmt.Sprintf("%q is not an IANA time zone name", *name))
		return *name, time.Local
	}
	return *name, loc
}

func intervals(r rawIntervals, ps *problems) Intervals {
	iv := Intervals{
		Supervisor: duration(r.Supervisor, DefaultSupervisor, "intervals.supervisor", minSupervisor, maxSupervisor, ps),
		Poll:       duration(r.Poll, DefaultPoll, "intervals.poll", minPoll, maxPoll, ps),
	}
	if iv.Poll > iv.Supervisor {
		ps.add("intervals.poll", fmt.Sprintf("%s is longer than intervals.supervisor (%s)", iv.Poll, iv.Supervisor))
	}
	return iv
}

func duration(s *string, def time.Duration, key string, lo, hi time.Duration, ps *problems) time.Duration {
	if s == nil {
		return def
	}
	d, err := time.ParseDuration(*s)
	if err != nil {
		ps.add(key, fmt.Sprintf("%q is not a duration such as \"10m\"", *s))
		return def
	}
	if d < lo || d > hi {
		ps.add(key, fmt.Sprintf("%s is outside the range %s to %s", *s, lo, hi))
	}
	return d
}

func stages(r map[string]rawStage, ps *problems) map[string]Stage {
	out := map[string]Stage{}
	for name, rs := range r {
		key := "stages." + name
		st := Stage{
			TimeLimit: duration(rs.TimeLimit, DefaultTimeLimit, key+".time_limit", minTimeLimit, maxTimeLimit, ps),
			Attempts:  num(rs.Attempts, DefaultAttempts),
		}
		if name != StageMerge {
			st.Command = slices.Clone(rs.Command)
			st.Contract = rs.Contract
			st.Runner = str(rs.Runner, DefaultRunner)
			st.Capabilities = orDefault(rs.Capabilities, DefaultCapabilities)
			if st.Runner == RunnerDocker && !slices.Contains(st.Capabilities, "docker") {
				st.Capabilities = append(st.Capabilities, "docker") // TECHNICAL-SPEC §7.2
			}
		}
		out[name] = st
	}
	if _, ok := out[StageMerge]; !ok {
		out[StageMerge] = Stage{TimeLimit: DefaultTimeLimit, Attempts: DefaultAttempts}
	}
	return out
}

func markers(r map[string]rawMarker, types []string, ps *problems) map[string]MarkerMapping {
	out := map[string]MarkerMapping{}
	for m, label := range LoopMarkers {
		out[m] = MarkerMapping{Label: label}
	}
	for _, t := range types {
		out[TypeMarkerPrefix+t] = MarkerMapping{Label: t}
	}
	for m, rm := range r {
		if t, ok := strings.CutPrefix(m, TypeMarkerPrefix); ok && !slices.Contains(types, t) {
			ps.add("markers."+m, fmt.Sprintf("type %q is not in types (%s)", t, list(types)))
			continue
		}
		out[m] = MarkerMapping{Label: rm.Label, Status: rm.Status}
	}
	// Two markers can't share a representation (D4).
	byRep := map[string][]string{}
	for m, mm := range out {
		// Label names are compared without case: GitHub label names are
		// case-insensitive (review R4). Statuses are compared exactly.
		rep := "label " + strconv.Quote(strings.ToLower(mm.Label))
		if mm.Status != "" {
			rep = "status " + strconv.Quote(mm.Status)
		}
		byRep[rep] = append(byRep[rep], m)
	}
	for rep, ms := range byRep {
		sort.Strings(ms)
		for _, m := range ms[1:] {
			ps.add("markers."+m, fmt.Sprintf("maps to %s, which marker %s also uses", rep, ms[0]))
		}
	}
	return out
}

func identities(r rawIdentities, ps *problems) Identities {
	id := Identities{Coder: r.Coder, Reviewer: r.Reviewer, Loop: r.Loop}
	refs := map[string]string{}
	for _, e := range []struct{ name, ref string }{{"coder", r.Coder}, {"reviewer", r.Reviewer}, {"loop", r.Loop}} {
		if other, ok := refs[e.ref]; ok {
			ps.add("identities."+e.name, fmt.Sprintf("is the same as identities.%s: the three identities must be different (FR-13 AC1)", other))
			continue
		}
		refs[e.ref] = e.name
	}
	seen := map[string]int{}
	for i, o := range r.Owners {
		key := idx("identities.owners", i) + ".id"
		var oid string
		switch v := o.ID.(type) {
		case int64:
			oid = strconv.FormatInt(v, 10)
		case string:
			oid = v
		default: // the schema and checkFloats allow only the two above
			ps.add(key, "must be a positive whole number or a non-empty string")
		}
		if j, ok := seen[oid]; ok {
			ps.add(key, fmt.Sprintf("%s is also the id of identities.owners[%d]", oid, j))
		}
		seen[oid] = i
		id.Owners = append(id.Owners, Owner{Login: o.Login, ID: oid})
	}
	return id
}

func effectiveEgress(e Egress, codeHost string) []string {
	if e.Replace {
		return slices.Clone(e.Allow)
	}
	out := slices.Clone(DefaultEgress)
	out = append(out, CodeHostEgress[codeHost]...)
	for _, h := range e.Allow {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func str(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func num(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func orDefault[T any](v, def []T) []T {
	if v == nil {
		return slices.Clone(def)
	}
	return slices.Clone(v)
}

func list(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return strings.Join(q, ", ")
}

// DefaultProfilePath returns the profile's path in a repository directory.
func DefaultProfilePath(repoDir string) string {
	return filepath.Join(repoDir, ProfileFileName)
}
