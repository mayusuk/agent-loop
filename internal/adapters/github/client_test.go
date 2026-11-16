package github_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (p *stubProject) item(t *testing.T, as ports.Actor, markers ...ports.Marker) ports.ItemRef {
	t.Helper()
	ref, err := p.driver.NewItem(as, "title", "body", markers...)
	must(t, err)
	return ref
}

func TestNewClientRejectsBadRepo(t *testing.T) {
	for _, r := range []string{"", "acme", "/x", "acme/", "a/b/c"} {
		if _, err := github.NewClient(github.ClientConfig{Repo: r}); err == nil {
			t.Errorf("repo %q accepted", r)
		}
	}
}

// TestErrorClasses: every HTTP error class maps to its port class (work
// plan D11).
func TestErrorClasses(t *testing.T) {
	reset := strconv.FormatInt(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).Unix(), 10)
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   error
	}{
		{"404", 404, nil, `{"message":"Not Found"}`, ports.ErrNotFound},
		{"410", 410, nil, `{"message":"Gone"}`, ports.ErrNotFound},
		{"401", 401, nil, `{"message":"Bad credentials"}`, ports.ErrPermission},
		{"403", 403, nil, `{"message":"Resource not accessible by integration"}`, ports.ErrPermission},
		{"403 rate limited", 403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, `{"message":"API rate limit exceeded"}`, ports.ErrRateLimited},
		{"403 secondary", 403, http.Header{"Retry-After": {"30"}}, `{"message":"secondary rate limit"}`, ports.ErrRateLimited},
		{"429", 429, nil, ``, ports.ErrRateLimited},
		{"409", 409, nil, `{"message":"Conflict"}`, ports.ErrConflict},
		{"412", 412, nil, ``, ports.ErrConflict},
		{"422", 422, nil, `{"message":"Validation Failed"}`, ports.ErrConflict},
		{"500", 500, nil, ``, ports.ErrTransient},
		{"502", 502, nil, `<html>bad gateway</html>`, ports.ErrTransient},
		{"200 unreadable", 200, nil, `{"number":`, ports.ErrTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newStub(t, 100)
			it := p.item(t, actorOf(p.owner))
			p.srv.Fail("GET", `/issues/\d+$`, c.status, c.header, c.body, 1)
			_, err := p.tracker.Item(ctx, it)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if c.want == ports.ErrRateLimited {
				at, ok := ports.RetryAt(err)
				if !ok || !at.After(p.srv.Now()) {
					t.Errorf("retry at %v (%v), want after %v", at, ok, p.srv.Now())
				}
				if c.name == "403 rate limited" && at.Unix() != time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).Unix() {
					t.Errorf("retry at %v, want the reset", at)
				}
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		p.srv.Close()
		if _, err := p.tracker.Item(ctx, it); !errors.Is(err, ports.ErrTransient) {
			t.Errorf("got %v, want ErrTransient", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		p := newStub(t, 100)
		it := p.item(t, actorOf(p.owner))
		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.tracker.Item(c, it); !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	})
	t.Run("no token", func(t *testing.T) {
		p := newStub(t, 100)
		c, _ := github.NewClient(github.ClientConfig{BaseURL: p.srv.URL, Repo: "acme/widgets"})
		if err := c.Get(ctx, ports.IdentityLoop, "/x", nil, nil); !errors.Is(err, ports.ErrPermission) {
			t.Errorf("got %v, want ErrPermission", err)
		}
	})
}

func TestGraphQLErrorClasses(t *testing.T) {
	p := newStub(t, 100)
	for typ, want := range map[string]error{
		"NOT_FOUND": ports.ErrNotFound, "RATE_LIMITED": ports.ErrRateLimited,
		"FORBIDDEN": ports.ErrPermission, "SOMETHING": ports.ErrTransient,
	} {
		p.srv.Fail("POST", `^/graphql$`, 200, nil, `{"errors":[{"type":"`+typ+`","message":"x"}]}`, 1)
		if err := p.client.GraphQL(ctx, ports.IdentityLoop, "query { x }", nil, nil); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", typ, err, want)
		}
	}
}

// TestConditionalRequests: an unchanged GET is answered 304 from the cache
// and doesn't count against the rate limit (§9.4, work plan D12).
func TestConditionalRequests(t *testing.T) {
	p := newStub(t, 100)
	p.srv.RateLimit = 1000
	it := p.item(t, actorOf(p.owner), ports.MarkerReady)
	a, err := p.tracker.Item(ctx, it)
	must(t, err)
	left, _, known := p.client.Budget(ports.IdentityLoop, github.ResourceCore)
	b, err := p.tracker.Item(ctx, it)
	must(t, err)
	if a.Version != b.Version || len(b.Markers) != 1 {
		t.Errorf("cached read: %+v, want %+v", b, a)
	}
	reqs := p.srv.Requests()
	if last := reqs[len(reqs)-1]; last.Status != http.StatusNotModified {
		t.Errorf("second read answered %d, want 304", last.Status)
	}
	if now, _, _ := p.client.Budget(ports.IdentityLoop, github.ResourceCore); !known || now != left {
		t.Errorf("budget went from %d to %d on a 304", left, now)
	}
	must(t, p.driver.SetMarker(actorOf(p.owner), it, ports.MarkerPaused, true))
	c, err := p.tracker.Item(ctx, it)
	must(t, err)
	if len(c.Markers) != 2 {
		t.Errorf("a changed issue was served from the cache: %v", c.Markers)
	}
}

// TestRateBudget: once the remaining count reaches the reserve, calls are
// refused locally until the reset, and a 403 at the limit is ErrRateLimited
// with the reset time (work plan D12).
func TestRateBudget(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	p.srv.RateLimit = 5
	c, err := github.NewClient(github.ClientConfig{
		BaseURL: p.srv.URL, Repo: "acme/widgets", Now: p.srv.Now,
		Tokens:  map[ports.Identity]github.TokenSource{ports.IdentityLoop: github.StaticToken(p.loop.Token)},
		Reserve: map[string]int{github.ResourceCore: 2},
	})
	must(t, err)
	var it2 struct{ Number int }
	for i := 0; i < 3; i++ { // 5, then 4, 3, 2 left: the third leaves 2
		must(t, c.Send(ctx, ports.IdentityLoop, "PATCH", c.RepoPath("/issues/%s", it), map[string]any{"title": "t" + strconv.Itoa(i)}, &it2))
	}
	before := len(p.srv.Requests())
	err = c.Send(ctx, ports.IdentityLoop, "PATCH", c.RepoPath("/issues/%s", it), map[string]any{"title": "x"}, nil)
	if !errors.Is(err, ports.ErrRateLimited) {
		t.Fatalf("got %v, want ErrRateLimited", err)
	}
	if len(p.srv.Requests()) != before {
		t.Error("a call over the reserve reached GitHub")
	}
	if at, _ := ports.RetryAt(err); !at.Equal(p.srv.Now().Add(time.Hour).Truncate(time.Second)) {
		t.Errorf("retry at %v, want the reset %v", at, p.srv.Now().Add(time.Hour))
	}
	// Another identity has its own budget.
	if left, _, ok := c.Budget(ports.IdentityCoder, github.ResourceCore); ok {
		t.Errorf("coder budget known (%d) before any call", left)
	}

	// The limit itself: a client with no reserve hits GitHub's 403.
	c2, _ := github.NewClient(github.ClientConfig{
		BaseURL: p.srv.URL, Repo: "acme/widgets", Now: p.srv.Now,
		Tokens:  map[ports.Identity]github.TokenSource{ports.IdentityLoop: github.StaticToken(p.loop.Token)},
		Reserve: map[string]int{github.ResourceCore: -1},
	})
	for i := 0; i < 3; i++ {
		err = c2.Send(ctx, ports.IdentityLoop, "PATCH", c2.RepoPath("/issues/%s", it), map[string]any{"title": "y" + strconv.Itoa(i)}, nil)
	}
	if !errors.Is(err, ports.ErrRateLimited) {
		t.Errorf("at the limit: got %v, want ErrRateLimited", err)
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	p.srv.Fail("GET", `/issues/\d+$`, 403, http.Header{"Retry-After": {"60"}}, `{"message":"secondary rate limit"}`, 1)
	if _, err := p.tracker.Item(ctx, it); !errors.Is(err, ports.ErrRateLimited) {
		t.Fatalf("got %v", err)
	}
	before := len(p.srv.Requests())
	if _, err := p.tracker.Item(ctx, it); !errors.Is(err, ports.ErrRateLimited) {
		t.Errorf("during Retry-After: got %v, want ErrRateLimited", err)
	}
	if len(p.srv.Requests()) != before {
		t.Error("a call during Retry-After reached GitHub")
	}
	p.srv.Advance(61 * time.Second)
	if _, err := p.tracker.Item(ctx, it); err != nil {
		t.Errorf("after Retry-After: %v", err)
	}
}

// TestPaging: lists follow the Link header across pages.
func TestPaging(t *testing.T) {
	p := newStub(t, 100)
	for i := 0; i < 130; i++ {
		p.item(t, actorOf(p.owner), ports.MarkerReady)
	}
	cs, err := p.tracker.Candidates(ctx)
	must(t, err)
	if len(cs) != 130 {
		t.Fatalf("got %d candidates, want 130", len(cs))
	}
	pages := 0
	for _, r := range p.srv.Requests() {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/issues") {
			pages++
		}
	}
	if pages != 2 {
		t.Errorf("%d list pages, want 2", pages)
	}
}
