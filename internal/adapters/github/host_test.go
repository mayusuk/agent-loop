package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

func changeRec(key ports.IdemKey, fields map[string]any) ports.Record {
	m := map[string]any{"type": "change", "v": 1, "key": key}
	for k, v := range fields {
		m[k] = v
	}
	data, _ := json.Marshal(m)
	return ports.Record{Type: ports.RecordChange, Key: key, Data: data}
}

func reviewRec(key ports.IdemKey, findings []map[string]any) ports.Record {
	m := map[string]any{"type": "review", "v": 1, "key": key}
	if findings != nil {
		m["findings"] = findings
	}
	data, _ := json.Marshal(m)
	return ports.Record{Type: ports.RecordReview, Key: key, Data: data}
}

func findingRec(key ports.IdemKey) ports.Record {
	data, _ := json.Marshal(map[string]any{"type": "finding", "v": 1, "key": key})
	return ports.Record{Type: ports.RecordFinding, Key: key, Data: data}
}

func replyRec(key ports.IdemKey) ports.Record {
	data, _ := json.Marshal(map[string]any{"type": "reply", "v": 1, "key": key, "finding": "F1", "kind": "fixed"})
	return ports.Record{Type: ports.RecordReply, Key: key, Data: data}
}

// open pushes branch as the coder and opens its change for item as the
// worker does.
func (p *stubProject) open(t *testing.T, item ports.ItemRef, branch string) (ports.ChangeRef, ports.Commit) {
	t.Helper()
	head, err := p.driver.Push(actorOf(p.coder), branch)
	must(t, err)
	key := ports.IdemKey("open:" + branch)
	ref, err := p.host.OpenChange(ctx, ports.NewChange{Item: item, Branch: branch, Base: "main", Record: changeRec(key, nil)}, ports.IdentityCoder, key)
	must(t, err)
	return ref, head
}

func TestNewHostChecksConfig(t *testing.T) {
	p := newStub(t, 100)
	if _, err := github.NewHost(p.client, github.HostConfig{Identities: github.Identities{Loop: actorOf(p.loop)}}); err == nil {
		t.Error("host without the coder and reviewer IDs accepted")
	}
	ids := github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)}
	if _, err := github.NewHost(p.client, github.HostConfig{Identities: ids, Labels: map[ports.Marker]string{ports.MarkerPaused: "ready"}}); err == nil {
		t.Error("two markers on one label accepted")
	}
	if caps := p.host.Capabilities(); !caps.PushRule || !caps.FileLevelComments || caps.MaxBody != github.MaxBody {
		t.Errorf("capabilities %+v", caps)
	}
}

// TestOpenChange: the record gets the item and n, the title a default,
// and the refusals (work plan D1).
func TestOpenChange(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	branch := "agent/" + string(it) + "-2"
	_, err := p.driver.Push(actorOf(p.coder), branch)
	must(t, err)
	nc := ports.NewChange{Item: it, Branch: branch, Base: "main", Body: untrusted.New("Does the thing."), Record: changeRec("k1", nil)}

	for name, c := range map[string]struct {
		n    ports.NewChange
		as   ports.Identity
		want error
	}{
		"as reviewer":    {nc, ports.IdentityReviewer, ports.ErrPermission},
		"no branch":      {ports.NewChange{Item: it, Branch: "agent/" + string(it) + "-3", Record: changeRec("k1", nil)}, ports.IdentityCoder, ports.ErrNotFound},
		"base branch":    {ports.NewChange{Item: it, Branch: "main", Record: changeRec("k1", nil)}, ports.IdentityCoder, ports.ErrConflict},
		"other item":     {ports.NewChange{Item: it, Branch: branch, Record: changeRec("k1", map[string]any{"item": "999"})}, ports.IdentityCoder, ports.ErrConflict},
		"not an item":    {ports.NewChange{Item: "x", Branch: branch, Record: changeRec("k1", nil)}, ports.IdentityCoder, ports.ErrNotFound},
		"no record type": {ports.NewChange{Item: it, Branch: branch, Record: ports.Record{Key: "k1"}}, ports.IdentityCoder, ports.ErrConflict},
	} {
		if _, err := p.host.OpenChange(ctx, c.n, c.as, "k1"); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	ref, err := p.host.OpenChange(ctx, nc, ports.IdentityCoder, "k1")
	must(t, err)
	c, err := p.host.Change(ctx, ref)
	must(t, err)
	if c.Item != it || c.Branch != branch || c.Base != "main" || c.State != ports.ChangeStateOpen || c.Fork || c.Author.ID != itoa(p.coder.ID) {
		t.Errorf("change %+v", c)
	}
	n, _ := strconv.Atoi(string(ref))
	body := p.srv.IssueBody(n)
	if !strings.HasPrefix(body, "Does the thing.") || !strings.Contains(body, `"item":"`+string(it)+`"`) || !strings.Contains(body, `"n":2`) {
		t.Errorf("body %q", body)
	}
	// Another key on a branch with an open change: GitHub refuses it.
	if _, err := p.host.OpenChange(ctx, ports.NewChange{Item: it, Branch: branch, Record: changeRec("k2", nil)}, ports.IdentityCoder, "k2"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("second change on the branch: %v", err)
	}
}

// TestChangeForItem: the record and branch link, -<n> branches after a
// closed change, closing references, and records that don't count (§6.2,
// work plan D2).
func TestChangeForItem(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	it := p.item(t, owner)
	if c, err := p.host.ChangeForItem(ctx, it); err != nil || c != nil {
		t.Fatalf("no change yet: %v, %v", c, err)
	}
	first, _ := p.open(t, it, "agent/"+string(it))
	if c, err := p.host.ChangeForItem(ctx, it); err != nil || c == nil || c.Ref != first || c.Item != it {
		t.Fatalf("first change: %+v, %v", c, err)
	}
	must(t, p.driver.CloseChange(owner, first))
	if c, err := p.host.ChangeForItem(ctx, it); err != nil || c != nil {
		t.Fatalf("after the owner closed it: %+v, %v", c, err)
	}
	second, _ := p.open(t, it, "agent/"+string(it)+"-2")
	if c, err := p.host.ChangeForItem(ctx, it); err != nil || c == nil || c.Ref != second {
		t.Fatalf("change on -2: %+v, %v", c, err)
	}

	// A human's change with a forged coder record isn't linked; one with a
	// closing reference is.
	other := p.item(t, owner)
	_, err := p.driver.Push(owner, "agent/"+string(other))
	must(t, err)
	forged, err := p.driver.OpenChange(owner, "", "agent/"+string(other))
	must(t, err)
	_ = forged
	if c, err := p.host.ChangeForItem(ctx, other); err != nil || c != nil {
		t.Fatalf("a human's change without a closing reference: %+v, %v", c, err)
	}
	_, err = p.driver.Push(owner, "feature-x")
	must(t, err)
	adopted, err := p.driver.OpenChange(owner, other, "feature-x")
	must(t, err)
	c, err := p.host.ChangeForItem(ctx, other)
	if err != nil || c == nil || c.Ref != adopted || c.Item != other || c.Author.ID != itoa(p.owner.ID) {
		t.Fatalf("owner-opened change: %+v, %v", c, err)
	}
	if _, err := p.host.ChangeForItem(ctx, "x"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("bad item ref: %v", err)
	}
}

// TestChangeStatesAndLabels: merged and closed states, merged by, and
// labels as markers (work plan D18).
func TestChangeStatesAndLabels(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	ref, head := p.open(t, it, "agent/"+string(it))
	if err := p.host.SetLabel(ctx, ref, ports.Marker("nope"), true, "k"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("unmapped marker: %v", err)
	}
	must(t, p.host.SetLabel(ctx, ref, ports.MarkerAborted, true, "k"))
	must(t, p.host.SetLabel(ctx, ref, ports.MarkerPaused, true, "k"))
	must(t, p.host.SetLabel(ctx, ref, ports.MarkerPaused, false, "k"))
	must(t, p.host.SetLabel(ctx, ref, ports.MarkerPaused, false, "k"))
	c, err := p.host.Change(ctx, ref)
	must(t, err)
	if len(c.Markers) != 1 || c.Markers[0] != ports.MarkerAborted || c.Head != head {
		t.Errorf("change %+v", c)
	}
	must(t, p.driver.SetGate(head, "ci", ports.GatePass))
	out, err := p.host.Merge(ctx, ref, head, "", "k")
	if err != nil || out != ports.MergeMerged {
		t.Fatalf("merge: %v, %v", out, err)
	}
	c, err = p.host.Change(ctx, ref)
	must(t, err)
	if c.State != ports.ChangeStateMerged || c.MergedBy == nil || c.MergedBy.ID != itoa(p.loop.ID) || c.MergedAt.IsZero() || c.Head != head {
		t.Errorf("merged change %+v", c)
	}
	if _, err := p.host.Change(ctx, "999"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing change: %v", err)
	}
	if _, err := p.host.Change(ctx, "x"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("bad change ref: %v", err)
	}
}

// TestMergeOutcomes: the response mapping of §9.7 (work plan D15).
func TestMergeOutcomes(t *testing.T) {
	cases := []struct {
		status int
		header http.Header
		want   ports.MergeOutcome
		err    error
	}{
		{409, nil, ports.MergeHeadMoved, nil},
		{405, nil, ports.MergeNotMergeable, nil},
		{403, nil, ports.MergeFailed, nil},
		{404, nil, ports.MergeFailed, nil},
		{422, nil, ports.MergeFailed, nil},
		{502, nil, "", ports.ErrTransient},
		{403, http.Header{"X-Ratelimit-Remaining": {"0"}}, "", ports.ErrRateLimited},
	}
	for _, c := range cases {
		t.Run(itoa(int64(c.status)), func(t *testing.T) {
			p := newStub(t, 100)
			it := p.item(t, actorOf(p.owner))
			ref, head := p.open(t, it, "agent/"+string(it))
			p.srv.Fail("PUT", `/merge$`, c.status, c.header, `{"message":"x"}`, 1)
			out, err := p.host.Merge(ctx, ref, head, ports.MergeMethodSquash, "k")
			if out != c.want || (c.err == nil) != (err == nil) || (c.err != nil && !errors.Is(err, c.err)) {
				t.Errorf("got %q, %v; want %q, %v", out, err, c.want, c.err)
			}
		})
	}

	p := newStub(t, 100)
	owner := actorOf(p.owner)
	it := p.item(t, owner)
	ref, head := p.open(t, it, "agent/"+string(it))
	// GitHub's own refusals: a required gate that hasn't passed.
	if out, err := p.host.Merge(ctx, ref, head, ports.MergeMethodMerge, "k"); err != nil || out != ports.MergeNotMergeable {
		t.Errorf("gate missing: %v, %v", out, err)
	}
	must(t, p.driver.SetGate(head, "ci", ports.GatePass))
	if out, err := p.host.Merge(ctx, ref, "0000000000000000000000000000000000000000", ports.MergeMethodMerge, "k"); err != nil || out != ports.MergeHeadMoved {
		t.Errorf("old pin: %v, %v", out, err)
	}
	if out, err := p.host.Merge(ctx, ref, head, ports.MergeMethodMerge, "k"); err != nil || out != ports.MergeMerged {
		t.Fatalf("merge: %v, %v", out, err)
	}
	// A repeat finds the merge; another pin on a merged change is refused.
	if out, err := p.host.Merge(ctx, ref, head, ports.MergeMethodMerge, "k"); err != nil || out != ports.MergeMerged {
		t.Errorf("repeat: %v, %v", out, err)
	}
	if out, err := p.host.Merge(ctx, ref, "0000000000000000000000000000000000000000", ports.MergeMethodMerge, "k"); err != nil || out != ports.MergeNotMergeable {
		t.Errorf("merged at another commit: %v, %v", out, err)
	}
	// A closed change is not mergeable.
	it2 := p.item(t, owner)
	ref2, head2 := p.open(t, it2, "agent/"+string(it2))
	must(t, p.driver.CloseChange(owner, ref2))
	if out, err := p.host.Merge(ctx, ref2, head2, ports.MergeMethodSquash, "k"); err != nil || out != ports.MergeNotMergeable {
		t.Errorf("closed: %v, %v", out, err)
	}
	// Branches must be up to date when the base requires it.
	it3 := p.item(t, owner)
	ref3, head3 := p.open(t, it3, "agent/"+string(it3))
	must(t, p.driver.SetGate(head3, "ci", ports.GatePass))
	p.srv.RequireUpToDate(true)
	_, err := p.driver.Push(owner, "main")
	must(t, err)
	if out, err := p.host.Merge(ctx, ref3, head3, ports.MergeMethodSquash, "k"); err != nil || out != ports.MergeNotMergeable {
		t.Errorf("behind the base: %v, %v", out, err)
	}
}

// TestGatesReadsEveryPage: a required gate whose check run or status is
// past the first page of 100 on the commit is reported, not Missing (#68,
// work plan #68 D1).
func TestGatesReadsEveryPage(t *testing.T) {
	p := newStub(t, 100)
	p.srv.RequireClassicChecks("lint", "ci")
	it := p.item(t, actorOf(p.owner))
	ref, head := p.open(t, it, "agent/"+string(it))
	sha := string(head)
	p.srv.AddCheckRun(sha, "lint", "completed", "success") // oldest: last page
	for i := range 120 {
		p.srv.AddCheckRun(sha, fmt.Sprintf("noise-%d", i), "completed", "success")
		must(t, p.driverCall("POST", "/statuses/"+sha, map[string]any{"state": "success", "context": fmt.Sprintf("noise/%d", i)}))
	}
	must(t, p.driver.SetGate(head, "ci", ports.GateFail)) // newest status: last page
	got, err := p.host.Gates(ctx, ref, head)
	must(t, err)
	want := []ports.GateResult{{Name: "ci", State: ports.GateFail, Commit: head}, {Name: "lint", State: ports.GatePass, Commit: head}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestGates: required gates from rules, classic protection and the
// profile; check runs and statuses on the commit asked for (§9.7, work
// plan D12).
func TestGates(t *testing.T) {
	p := newStub(t, 100)
	p.srv.RequireClassicChecks("lint", "ci") // ci is in the ruleset too
	it := p.item(t, actorOf(p.owner))
	ref, head := p.open(t, it, "agent/"+string(it))
	sha := string(head)
	p.srv.AddCheckRun(sha, "lint", "completed", "failure")
	p.srv.AddCheckRun(sha, "lint", "completed", "neutral") // the latest run counts
	must(t, p.driver.SetGate(head, "ci", ports.GateFail))
	got, err := p.host.Gates(ctx, ref, head)
	must(t, err)
	want := []ports.GateResult{{Name: "ci", State: ports.GateFail, Commit: head}, {Name: "lint", State: ports.GatePass, Commit: head}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
	p.srv.AddCheckRun(sha, "ci", "in_progress", "") // a check run wins over a status
	got, err = p.host.Gates(ctx, ref, head)
	must(t, err)
	if got[0].State != ports.GatePending {
		t.Errorf("in-progress run: %+v", got[0])
	}

	ids := github.Identities{Coder: actorOf(p.coder), Reviewer: actorOf(p.reviewer), Loop: actorOf(p.loop)}
	h, err := github.NewHost(p.client, github.HostConfig{Identities: ids, Gates: []string{"build", "lint"}})
	must(t, err)
	got, err = h.Gates(ctx, ref, head)
	must(t, err)
	if len(got) != 2 || got[0].Name != "build" || got[0].State != ports.GateMissing || got[1].State != ports.GatePass {
		t.Errorf("profile gates: %+v", got)
	}
	for _, c := range []struct {
		status, conclusion string
		want               ports.GateState
	}{
		{"queued", "", ports.GatePending}, {"completed", "success", ports.GatePass}, {"completed", "skipped", ports.GatePass},
		{"completed", "cancelled", ports.GateFail}, {"completed", "timed_out", ports.GateFail},
	} {
		p.srv.AddCheckRun(sha, "build", c.status, c.conclusion)
		got, err := h.Gates(ctx, ref, head)
		must(t, err)
		if got[0].State != c.want {
			t.Errorf("check run %s/%s: %s, want %s", c.status, c.conclusion, got[0].State, c.want)
		}
	}
	must(t, p.driver.SetGate(head, "lint2", ports.GatePending))
	h2, err := github.NewHost(p.client, github.HostConfig{Identities: ids, Gates: []string{"lint2"}})
	must(t, err)
	if got, err := h2.Gates(ctx, ref, head); err != nil || got[0].State != ports.GatePending {
		t.Errorf("pending status: %+v, %v", got, err)
	}
	if _, err := p.host.Gates(ctx, "999", head); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing change: %v", err)
	}
}

// TestContainsBase: compare behind_by.
func TestContainsBase(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	it := p.item(t, owner)
	ref, _ := p.open(t, it, "agent/"+string(it))
	ok, err := p.host.ContainsBase(ctx, ref)
	must(t, err)
	if !ok {
		t.Error("fresh branch doesn't contain the base")
	}
	_, err = p.driver.Push(owner, "main")
	must(t, err)
	if ok, err := p.host.ContainsBase(ctx, ref); err != nil || ok {
		t.Errorf("after the base moved: %v, %v", ok, err)
	}
	if _, err := p.host.ContainsBase(ctx, "999"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing change: %v", err)
	}
	if _, err := p.host.Trust(ctx, "999"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing change: %v", err)
	}
}

// TestHostValidate: each problem code (work plan D13).
func TestHostValidate(t *testing.T) {
	p := newStub(t, 100)
	p.cleanRepo()
	good := ports.ProfileCodeHost{Adapter: "github", Repo: "acme/widgets", BaseBranch: "main", MergeMethod: ports.MergeMethodSquash}
	if probs := p.host.Validate(ctx, good); len(probs) != 0 {
		t.Errorf("clean profile: %+v", probs)
	}
	codes := func(ps []ports.Problem) string {
		var out []string
		for _, x := range ps {
			out = append(out, x.Code)
		}
		return strings.Join(out, ",")
	}
	if got := codes(p.host.Validate(ctx, ports.ProfileCodeHost{Adapter: "gitlab"})); got != "wrong_adapter" {
		t.Errorf("adapter: %s", got)
	}
	bad := ports.ProfileCodeHost{Adapter: "github", Repo: "acme/other", BaseBranch: "trunk", MergeMethod: "fast-forward"}
	if got := codes(p.host.Validate(ctx, bad)); got != "wrong_repo,merge_method_unsupported,base_branch_missing" {
		t.Errorf("problems: %s", got)
	}
	p.srv.Fail("GET", `/branches/`, 500, nil, `{}`, 0)
	if got := codes(p.host.Validate(ctx, good)); got != "branches_unreadable" {
		t.Errorf("unreadable: %s", got)
	}
}

// TestHostMethodsClassifyErrors: each method returns the class of a failed
// call (tracker D11).
func TestHostMethodsClassifyErrors(t *testing.T) {
	type call func(p *stubProject, ch ports.ChangeRef, head ports.Commit) error
	methods := map[string]call{
		"ChangeForItem": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.ChangeForItem(ctx, "1")
			return err
		},
		"Change": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.Change(ctx, ch)
			return err
		},
		"Changes": func(p *stubProject, _ ports.ChangeRef, _ ports.Commit) error {
			_, _, err := p.host.Changes(ctx, "")
			return err
		},
		"OpenChange": func(p *stubProject, _ ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.OpenChange(ctx, ports.NewChange{Item: "1", Branch: "agent/1", Record: changeRec("k", nil)}, ports.IdentityCoder, "k")
			return err
		},
		"SetLabel": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			return p.host.SetLabel(ctx, ch, ports.MarkerPaused, true, "k")
		},
		"Trust": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.Trust(ctx, ch)
			return err
		},
		"ContainsBase": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.ContainsBase(ctx, ch)
			return err
		},
		"Gates": func(p *stubProject, ch ports.ChangeRef, h ports.Commit) error {
			_, err := p.host.Gates(ctx, ch, h)
			return err
		},
		"ReviewRecords": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.ReviewRecords(ctx, ch)
			return err
		},
		"PostReview": func(p *stubProject, ch ports.ChangeRef, h ports.Commit) error {
			_, err := p.host.PostReview(ctx, ch, ports.ReviewPost{Commit: h, Record: reviewRec("k", nil)}, "k")
			return err
		},
		"ResolveFinding": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			return p.host.ResolveFinding(ctx, ch, "F1", "k")
		},
		"ManualComments": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			_, err := p.host.ManualComments(ctx, ch)
			return err
		},
		"PostReply": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			return p.host.PostReply(ctx, ch, ports.ThreadRef(ch), replyRec("k"), untrusted.Text{}, "k")
		},
		"PostReplyThread": func(p *stubProject, ch ports.ChangeRef, _ ports.Commit) error {
			return p.host.PostReply(ctx, ch, "PRRT_x", replyRec("k"), untrusted.Text{}, "k")
		},
		"Merge": func(p *stubProject, ch ports.ChangeRef, h ports.Commit) error {
			_, err := p.host.Merge(ctx, ch, h, ports.MergeMethodSquash, "k")
			return err
		},
	}
	classes := []struct {
		status int
		header http.Header
		want   error
	}{
		{404, nil, ports.ErrNotFound},
		{403, nil, ports.ErrPermission},
		{429, http.Header{"Retry-After": {"1"}}, ports.ErrRateLimited},
		{503, nil, ports.ErrTransient},
	}
	for name, m := range methods {
		for _, c := range classes {
			t.Run(name+"/"+itoa(int64(c.status)), func(t *testing.T) {
				p := newStub(t, 100)
				it := p.item(t, actorOf(p.owner))
				ch, head := p.open(t, it, "agent/"+string(it))
				for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
					p.srv.Fail(method, `.`, c.status, c.header, `{"message":"x"}`, 0)
				}
				if err := m(p, ch, head); !errors.Is(err, c.want) {
					t.Errorf("got %v, want %v", err, c.want)
				}
			})
		}
	}
}

func findingRecID(key ports.IdemKey, id string) ports.Record {
	data, _ := json.Marshal(map[string]any{"type": "finding", "v": 1, "key": key, "id": id})
	return ports.Record{Type: ports.RecordFinding, Key: key, Data: data}
}
