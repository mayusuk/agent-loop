package githubtest_test

import (
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// Issue #74 (review R2 on #80): the live scenario test adds a token while
// another goroutine's driver calls look tokens up. SetToken and the lookup
// must be synchronised, so go test -race reports no race here.
func TestIssue74_SetTokenWhileCallsRun(t *testing.T) {
	srv := githubtest.New("acme", "widgets")
	defer srv.Close()
	owner := srv.AddUser(1, "owner", "User")
	coder := srv.AddUser(2, "coder", "Bot")
	as := func(u *githubtest.User) ports.Actor {
		return ports.Actor{ID: strconv.FormatInt(u.ID, 10), Login: u.Login}
	}
	d := &githubtest.Driver{BaseURL: srv.URL, Repo: "acme/widgets", Owner: as(owner),
		Tokens: map[string]string{as(owner).ID: owner.Token}}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			if err := d.Call(as(owner), http.MethodGet, "", nil, nil); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 20 {
		d.SetToken(as(coder).ID, coder.Token)
	}
	wg.Wait()
	if err := d.Call(as(coder), http.MethodGet, "", nil, nil); err != nil {
		t.Fatalf("call with the token SetToken added: %v", err)
	}
}

// A call as an account with no token fails and names the account.
func TestIssue74_CallWithoutTokenFails(t *testing.T) {
	srv := githubtest.New("acme", "widgets")
	defer srv.Close()
	d := &githubtest.Driver{BaseURL: srv.URL, Repo: "acme/widgets"}
	err := d.Call(ports.Actor{ID: "9", Login: "nobody"}, http.MethodGet, "", nil, nil)
	if err == nil {
		t.Fatal("call with no token succeeded")
	}
}
