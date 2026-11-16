package github_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

func codes(ps []ports.Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, string(p.Severity)+":"+p.Code)
		if p.Message == "" || p.Hint == "" {
			out = append(out, "missing message or hint: "+p.Code)
		}
	}
	sort.Strings(out)
	return out
}

func goodProfile() ports.ProfileTracker {
	return ports.ProfileTracker{
		Adapter: "github", Repo: "acme/widgets",
		Markers: map[ports.Marker]ports.MarkerMapping{ports.MarkerReady: {Label: "ready"}},
		Owners:  []ports.OwnerAccount{{Login: "owner", ID: "1001"}},
	}
}

func TestValidateClean(t *testing.T) {
	p := newStub(t, 100)
	if ps := p.tracker.Validate(ctx, goodProfile()); len(ps) != 0 {
		t.Errorf("problems on a clean profile: %v", codes(ps))
	}
}

func TestValidateReportsEachProblem(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	text, err := p.driver.NewItem(owner, "t", "Needs the API.\n\nBlocked by #1", ports.MarkerReady)
	must(t, err)
	native, err := p.driver.NewItem(owner, "t", "Blocked by #1", ports.MarkerReady)
	must(t, err)
	must(t, p.tracker.AddDependency(ctx, native, text, "d"))
	pr := p.profileWithProblems()
	got := strings.Join(codes(p.tracker.Validate(ctx, pr)), " ")
	want := "error:marker_unmapped error:owner_id_not_numeric error:status_marker_unsupported error:wrong_repo warning:text_dependency"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// Labels that don't exist.
	bare := newStubWith(t, 100, false)
	got = strings.Join(codes(bare.tracker.Validate(ctx, goodProfile())), " ")
	if strings.Count(got, "error:label_missing") != 11 {
		t.Errorf("missing labels: %s", got)
	}

	pr.Adapter = "jira"
	if got := codes(p.tracker.Validate(ctx, pr)); len(got) != 1 || got[0] != "error:wrong_adapter" {
		t.Errorf("wrong adapter: %v", got)
	}
	p.srv.Fail("GET", `/labels$`, 403, nil, `{"message":"no"}`, 1)
	if got := strings.Join(codes(p.tracker.Validate(ctx, goodProfile())), " "); got != "error:labels_unreadable" {
		t.Errorf("unreadable labels: %s", got)
	}
	p.srv.Fail("GET", `/issues$`, 500, nil, "", 1)
	if got := strings.Join(codes(p.tracker.Validate(ctx, goodProfile())), " "); got != "error:issues_unreadable" {
		t.Errorf("unreadable issues: %s", got)
	}
}

func (p *stubProject) profileWithProblems() ports.ProfileTracker {
	pr := goodProfile()
	pr.Repo = "acme/other"
	pr.Markers = map[ports.Marker]ports.MarkerMapping{
		ports.MarkerReady:      {Status: "Selected"},
		ports.MarkerInProgress: {},
	}
	pr.Owners = append(pr.Owners, ports.OwnerAccount{Login: "bob", ID: "bob"})
	return pr
}
