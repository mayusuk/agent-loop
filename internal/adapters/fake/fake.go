package fake

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// The default actors. Their IDs are stable, as on a real code host (I13).
var (
	Owner    = ports.Actor{ID: "1001", Login: "owner", Kind: ports.ActorHuman}
	Human    = ports.Actor{ID: "2001", Login: "alice", Kind: ports.ActorHuman}               // another human with write access
	Bot      = ports.Actor{ID: "3001", Login: "scanner[bot]", Kind: ports.ActorBot}          // listed in manual_comment_bots
	OtherBot = ports.Actor{ID: "3002", Login: "dependabot[bot]", Kind: ports.ActorBot}       // not listed
	Coder    = ports.Actor{ID: "9001", Login: "project-coder[bot]", Kind: ports.ActorBot}    // coder identity
	Reviewer = ports.Actor{ID: "9002", Login: "project-reviewer[bot]", Kind: ports.ActorBot} // reviewer identity
	Loop     = ports.Actor{ID: "9003", Login: "project-loop[bot]", Kind: ports.ActorBot}     // loop identity
)

// Config describes the sandbox project the fake plays.
type Config struct {
	Owners                []ports.Actor // owner accounts; they are the repository's admins
	Coder, Reviewer, Loop ports.Actor
	ManualCommentBots     []string // bot logins whose comments are manual comments (I7)
	BaseBranch            string   // "main"
	BranchPrefix          string   // "agent/"
	Gates                 []string // required gates
	RequiredApprovals     int      // approving reviews the base branch requires
	// RequireUpToDate refuses a merge whose head lacks the base tip
	// ("require branches to be up to date", INTEGRATION-SPEC §9.7).
	RequireUpToDate bool
	PageSize        int       // events per Changes call
	Start           time.Time // the clock's start
}

// DefaultConfig returns a project with the default actors, base "main",
// prefix "agent/", one required gate "ci", and up-to-date merges required.
func DefaultConfig() Config {
	return Config{
		Owners:            []ports.Actor{Owner},
		Coder:             Coder,
		Reviewer:          Reviewer,
		Loop:              Loop,
		ManualCommentBots: []string{Bot.Login},
		BaseBranch:        "main",
		BranchPrefix:      "agent/",
		Gates:             []string{"ci"},
		RequireUpToDate:   true,
		PageSize:          100,
		Start:             time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
	}
}

// Fake is an in-memory tracker and code host. Tracker and Host return its two
// ports; its other exported methods script the sandbox as a given actor. It
// is safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	cfg Config
	now time.Time
	seq int

	items     map[ports.ItemRef]*item
	itemOrder []ports.ItemRef

	commits  map[ports.Commit]*commit
	branches map[string]ports.Commit
	tags     map[string]ports.Commit

	changes     map[ports.ChangeRef]*change
	changeOrder []ports.ChangeRef
	threads     map[ports.ThreadRef]*thread
	gates       map[ports.Commit]map[string]ports.GateState

	rulesets      map[string]*ruleset
	rulesetEvents []time.Time // every change, disable or removal (I9)
	loopInactive  []interval  // when the loop-branch ruleset wasn't active
	admins        map[string]*admin
	runs          []*coderRun

	trackerLog   []ports.ItemEvent
	hostLog      []hostEvent
	lastHostPoll time.Time

	keys  map[string]string // idempotency ledger for writes that leave no record
	kills map[string]Phase
}

// New returns a fake for cfg. Zero fields of cfg take DefaultConfig's
// values, except the slices and RequiredApprovals.
func New(cfg Config) *Fake {
	d := DefaultConfig()
	if cfg.Coder == (ports.Actor{}) {
		cfg.Coder = d.Coder
	}
	if cfg.Reviewer == (ports.Actor{}) {
		cfg.Reviewer = d.Reviewer
	}
	if cfg.Loop == (ports.Actor{}) {
		cfg.Loop = d.Loop
	}
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = d.BaseBranch
	}
	if cfg.BranchPrefix == "" {
		cfg.BranchPrefix = d.BranchPrefix
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = d.PageSize
	}
	if cfg.Start.IsZero() {
		cfg.Start = d.Start
	}
	f := &Fake{
		cfg:      cfg,
		now:      cfg.Start,
		items:    map[ports.ItemRef]*item{},
		commits:  map[ports.Commit]*commit{},
		branches: map[string]ports.Commit{},
		tags:     map[string]ports.Commit{},
		changes:  map[ports.ChangeRef]*change{},
		threads:  map[ports.ThreadRef]*thread{},
		gates:    map[ports.Commit]map[string]ports.GateState{},
		rulesets: map[string]*ruleset{},
		admins:   map[string]*admin{},
		keys:     map[string]string{},
		kills:    map[string]Phase{},
	}
	for _, r := range []string{RulesetLoopBranches, RulesetBaseBranch, RulesetTags} {
		f.rulesets[r] = &ruleset{exists: true, active: true, createdAt: f.now}
	}
	root := f.newCommit(nil, cfg.firstOwner(), "")
	f.branches[cfg.BaseBranch] = root.sha
	return f
}

// firstOwner is the first owner, or the default owner.
func (c Config) firstOwner() ports.Actor {
	if len(c.Owners) > 0 {
		return c.Owners[0]
	}
	return Owner
}

// Config returns the fake's configuration.
func (f *Fake) Config() Config { return f.cfg }

// Tracker returns the fake's WorkTracker.
func (f *Fake) Tracker() ports.WorkTracker { return (*Tracker)(f) }

// Host returns the fake's CodeHost.
func (f *Fake) Host() ports.CodeHost { return (*Host)(f) }

// Now returns the fake's clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// tick moves the clock one second forward, so every write has its own time.
func (f *Fake) tick() time.Time {
	f.now = f.now.Add(time.Second)
	return f.now
}

func (f *Fake) nextID() string {
	f.seq++
	return strconv.Itoa(f.seq)
}

// Phase is where in a write method a kill point fires.
type Phase int

const (
	BeforeWrite Phase = iota + 1 // the call fails and writes nothing
	AfterWrite                   // the call writes, then fails as if the reply was lost
)

// ErrKilled is returned by a write method at an armed kill point. It is
// ports.ErrTransient, so callers retry it like a crash.
var ErrKilled = fmt.Errorf("fake: killed at kill point: %w", ports.ErrTransient)

// writeMethods are the port methods that write, and so can be killed.
var writeMethods = map[string]bool{
	"Claim": true, "SetMarker": true, "Comment": true, "CreateItem": true, "AddDependency": true, "Close": true,
	"OpenChange": true, "SetLabel": true, "PostReview": true, "ResolveFinding": true, "PostReply": true, "Merge": true,
}

// KillAt arms a one-shot crash in the port write method named method (for
// example "Comment"). The next call returns ErrKilled before or after its
// write, as p says. It panics for a method that doesn't write.
func (f *Fake) KillAt(method string, p Phase) {
	if !writeMethods[method] {
		panic("fake: KillAt: not a write method: " + method)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills[method] = p
}

// killed reports whether the kill point for method fires at p, and disarms it.
func (f *Fake) killed(method string, p Phase) bool {
	if f.kills[method] == p {
		delete(f.kills, method)
		return true
	}
	return false
}

// begin locks the fake for a port call and checks the context.
func (f *Fake) begin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ports.ErrTransient, err)
	}
	f.mu.Lock()
	return nil
}

func (f *Fake) isIdentity(a ports.Actor) bool {
	return a.ID == f.cfg.Coder.ID || a.ID == f.cfg.Reviewer.ID || a.ID == f.cfg.Loop.ID
}

func (f *Fake) isOwner(a ports.Actor) bool {
	for _, o := range f.cfg.Owners {
		if o.ID == a.ID {
			return true
		}
	}
	return false
}

// isHuman: a human account that isn't one of the identities.
func (f *Fake) isHuman(a ports.Actor) bool { return a.Kind == ports.ActorHuman && !f.isIdentity(a) }

// isManualCommenter: a human, or a bot listed in manual_comment_bots (§9.6).
func (f *Fake) isManualCommenter(a ports.Actor) bool {
	if f.isIdentity(a) {
		return false
	}
	if a.Kind == ports.ActorHuman {
		return true
	}
	for _, l := range f.cfg.ManualCommentBots {
		if l == a.Login {
			return true
		}
	}
	return false
}

func (f *Fake) identity(id ports.Identity) (ports.Actor, error) {
	switch id {
	case ports.IdentityCoder:
		return f.cfg.Coder, nil
	case ports.IdentityReviewer:
		return f.cfg.Reviewer, nil
	case ports.IdentityLoop:
		return f.cfg.Loop, nil
	}
	return ports.Actor{}, fmt.Errorf("fake: unknown identity %q: %w", id, ports.ErrPermission)
}

// recordAuthor is the identity allowed to write each record type (§6.1).
func (f *Fake) recordAuthor(t ports.RecordType) ports.Actor {
	switch t {
	case ports.RecordChange, ports.RecordReply, ports.RecordManualReply, ports.RecordProposal:
		return f.cfg.Coder
	case ports.RecordReview, ports.RecordFinding:
		return f.cfg.Reviewer
	}
	return f.cfg.Loop
}

// counts reports whether rec, written by author, is a record (§6.1, C3).
func (f *Fake) counts(rec *ports.Record, author ports.Actor) bool {
	return rec != nil && f.recordAuthor(rec.Type).ID == author.ID
}

// comment is a comment on an item, a change or in a thread.
type comment struct {
	id     string
	author ports.Actor
	body   untrusted.Text
	rec    *ports.Record
	at     time.Time
}

func (c *comment) record() ports.Record {
	r := *c.rec
	r.Author, r.Ref, r.At = c.author, ports.CommentRef(c.id), c.at
	return r
}

func withKey(rec ports.Record, key ports.IdemKey) *ports.Record {
	rec.Key = key
	return &rec
}

// commit is one commit in the repository.
type commit struct {
	sha     ports.Commit
	parents []ports.Commit
	pusher  ports.Actor // who pushed it
	at      time.Time   // when it was pushed
	branch  string      // the branch it was pushed to
	seq     int         // creation order, for "latest" (#156 D2)
	files   map[string]string
	// The Pusher attribution, decided when the push is first observed (I11).
	attributed bool
	attr       ports.Pusher
}

func (f *Fake) newCommit(parents []ports.Commit, by ports.Actor, branch string) *commit {
	f.seq++
	sum := sha1.Sum([]byte("fake-commit-" + strconv.Itoa(f.seq)))
	c := &commit{sha: ports.Commit(hex.EncodeToString(sum[:])), parents: parents, pusher: by, at: f.now, branch: branch, seq: f.seq}
	switch len(parents) {
	case 0:
		c.files = map[string]string{}
	case 1:
		c.files = maps.Clone(f.commits[parents[0]].files)
	default:
		c.files = f.merge3(parents[0], parents[1])
	}
	f.commits[c.sha] = c
	return c
}

// mergeBase returns the newest common ancestor of a and b.
func (f *Fake) mergeBase(a, b ports.Commit) *commit {
	inB := f.reachable(b)
	var best *commit
	for x := range f.reachable(a) {
		if c := f.commits[x]; inB[x] && c != nil && (best == nil || c.seq > best.seq) {
			best = c
		}
	}
	return best
}

// merge3 merges the files of theirs into ours from their merge base: a file
// theirs changed takes theirs, any other keeps ours. The fake has no
// conflicts.
func (f *Fake) merge3(ours, theirs ports.Commit) map[string]string {
	o, t := f.commits[ours].files, f.commits[theirs].files
	base := map[string]string{}
	if mb := f.mergeBase(ours, theirs); mb != nil {
		base = mb.files
	}
	out := maps.Clone(o)
	for p := range unionKeys(base, t) {
		bv, inBase := base[p]
		tv, inTheirs := t[p]
		if inBase == inTheirs && bv == tv {
			continue
		}
		if inTheirs {
			out[p] = tv
		} else {
			delete(out, p)
		}
	}
	return out
}

func unionKeys(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// reachable returns every commit reachable from tip.
func (f *Fake) reachable(tip ports.Commit) map[ports.Commit]bool {
	seen := map[ports.Commit]bool{}
	stack := []ports.Commit{tip}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if cm := f.commits[c]; cm != nil {
			stack = append(stack, cm.parents...)
		}
	}
	return seen
}

// ownCommits returns the commits reachable from head but not from the base
// tip, in push order.
func (f *Fake) ownCommits(head ports.Commit) []*commit {
	base := f.reachable(f.branches[f.cfg.BaseBranch])
	var out []*commit
	for c := range f.reachable(head) {
		if !base[c] {
			out = append(out, f.commits[c])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

type interval struct{ from, to time.Time } // to is zero while open

func (iv interval) contains(t time.Time) bool {
	return !t.Before(iv.from) && (iv.to.IsZero() || !t.After(iv.to))
}

func (iv interval) overlaps(from, to time.Time) bool {
	return !iv.from.After(to) && (iv.to.IsZero() || !iv.to.Before(from))
}

// coderRun is a run that held a coder push token (I11): a handler run, or
// the worker's base merge.
type coderRun struct {
	item ports.ItemRef
	iv   interval
}

type ruleset struct {
	exists, active bool
	createdAt      time.Time
	extraBypass    bool // an actor §9.8 step 1 doesn't allow bypasses it (C17)
}

type admin struct {
	actor ports.Actor
	iv    []interval
}

func sortedMarkers(m map[ports.Marker]bool) []ports.Marker {
	var out []ports.Marker
	for k, on := range m {
		if on {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// page returns the page of log after cursor, and the next cursor (D2).
func page[T any](log []T, since ports.Cursor, size int) ([]T, ports.Cursor, error) {
	if since == "" {
		return nil, ports.Cursor(strconv.Itoa(len(log))), nil
	}
	i, err := strconv.Atoi(string(since))
	if err != nil || i < 0 || i > len(log) {
		return nil, since, fmt.Errorf("fake: bad cursor %q: %w", since, ports.ErrConflict)
	}
	j := min(i+size, len(log))
	out := append([]T(nil), log[i:j]...)
	return out, ports.Cursor(strconv.Itoa(j)), nil
}
