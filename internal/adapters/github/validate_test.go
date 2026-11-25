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

// TestFR1AC5_TextDependencyHintNamesTheFix: the warning about "Blocked by"
// text (INTEGRATION-SPEC §9.5) names the gh call that adds each native
// dependency, and the GitHub UI (work plan #104 D3).
func TestFR1AC5_TextDependencyHintNamesTheFix(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	_, err := p.driver.NewItem(owner, "t", "Blocked by #2, #3\n- blocked by: #4\nblocked by #2", ports.MarkerReady)
	must(t, err)
	var hint string
	for _, pr := range p.tracker.Validate(ctx, goodProfile()) {
		if pr.Code == "text_dependency" {
			hint = pr.Hint
		}
	}
	for _, b := range []string{"2", "3", "4"} {
		want := "gh api -X POST repos/acme/widgets/issues/1/dependencies/blocked_by -F issue_id=$(gh api repos/acme/widgets/issues/" + b + " --jq .id)"
		if strings.Count(hint, want) != 1 {
			t.Errorf("hint lacks (once) %q:\n%s", want, hint)
		}
	}
	if !strings.Contains(hint, `open issue #1 and add it under Relationships, "Mark as blocked by"`) {
		t.Errorf("hint lacks the GitHub UI: %s", hint)
	}
}

// TestFR1AC5_TextDependencyHintIgnoresTrailingText: only the leading list
// of issue references on a "Blocked by" line are blockers; other issues the
// line mentions after it aren't suggested as dependencies (work plan #104
// D3).
func TestFR1AC5_TextDependencyHintIgnoresTrailingText(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"Blocked by #2 (see #9 for background)", []string{"2"}},
		{"Blocked by #2, #3 and #4; also related to #9", []string{"2", "3", "4"}},
		{"blocked by: #5 #6 because #7 broke it", []string{"5", "6"}},
	}
	for _, c := range cases {
		p := newStub(t, 100)
		_, err := p.driver.NewItem(actorOf(p.owner), "t", c.body, ports.MarkerReady)
		must(t, err)
		var hint string
		for _, pr := range p.tracker.Validate(ctx, goodProfile()) {
			if pr.Code == "text_dependency" {
				hint = pr.Hint
			}
		}
		if got := strings.Count(hint, "gh api -X POST"); got != len(c.want) {
			t.Errorf("%q: %d commands, want %d:\n%s", c.body, got, len(c.want), hint)
		}
		for _, b := range c.want {
			if !strings.Contains(hint, "repos/acme/widgets/issues/"+b+" --jq .id") {
				t.Errorf("%q: hint lacks #%s:\n%s", c.body, b, hint)
			}
		}
		for _, b := range []string{"7", "9"} {
			if strings.Contains(hint, "repos/acme/widgets/issues/"+b+" --jq .id") {
				t.Errorf("%q: hint names #%s:\n%s", c.body, b, hint)
			}
		}
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

// TestFR13AC1_ValidateChecksOwnerAccounts: each owner account must exist
// and be a human; a changed login only warns (#23 D7).
func TestFR13AC1_ValidateChecksOwnerAccounts(t *testing.T) {
	p := newStub(t, 100)
	pr := goodProfile()
	pr.Owners = []ports.OwnerAccount{
		{Login: "owner", ID: "1001"},
		{Login: "old-name", ID: "2001"},
		{Login: "ghost", ID: "4242"},
		{Login: "scanner[bot]", ID: "3001"},
	}
	got := strings.Join(codes(p.tracker.Validate(ctx, pr)), " ")
	want := "error:owner_missing error:owner_not_human warning:owner_login_changed"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
