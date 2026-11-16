package github_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/conformance"
	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// TestLiveConformance runs the tracker cases against a sandbox repository on
// GitHub. It is skipped unless AGENT_LOOP_LIVE=1 (plan D5). It needs:
//
//	AGENT_LOOP_GITHUB_REPO            owner/name of a sandbox repository nobody else uses
//	AGENT_LOOP_GITHUB_API             the API base (default https://api.github.com)
//	AGENT_LOOP_GITHUB_<ROLE>_TOKEN    a token for each role
//	AGENT_LOOP_GITHUB_<ROLE>_LOGIN    that account's login ("name[bot]" for an app)
//
// for the roles OWNER (an owner account, repository admin), HUMAN (another
// account with write access), BOT (a bot listed in manual_comment_bots),
// CODER, REVIEWER and LOOP (the three apps' installation tokens, with the
// permissions of INTEGRATION-SPEC §9.1; C3 forges item records only as
// identities that can write item comments, work plan D15).
func TestLiveConformance(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("live GitHub tests run only with AGENT_LOOP_LIVE=1")
	}
	repo := os.Getenv("AGENT_LOOP_GITHUB_REPO")
	api := os.Getenv("AGENT_LOOP_GITHUB_API")
	if api == "" {
		api = "https://api.github.com"
	}
	roles := []string{"OWNER", "HUMAN", "BOT", "CODER", "REVIEWER", "LOOP"}
	tokens, actors := map[string]string{}, map[string]ports.Actor{}
	for _, r := range roles {
		tok, login := os.Getenv("AGENT_LOOP_GITHUB_"+r+"_TOKEN"), os.Getenv("AGENT_LOOP_GITHUB_"+r+"_LOGIN")
		if repo == "" || tok == "" || login == "" {
			t.Fatalf("AGENT_LOOP_LIVE=1 needs AGENT_LOOP_GITHUB_REPO and AGENT_LOOP_GITHUB_%s_TOKEN and _LOGIN", r)
		}
		tokens[r] = tok
		actors[r] = liveUser(t, api, tokens["OWNER"], login)
	}
	d := &githubtest.Driver{BaseURL: api, Repo: repo, Tokens: map[string]string{}}
	for _, r := range roles {
		d.Tokens[actors[r].ID] = tokens[r]
	}
	for _, l := range github.DefaultLabels {
		liveLabel(t, api, repo, tokens["OWNER"], l)
	}
	conformance.RunTracker(t, func(t *testing.T) conformance.Env {
		c, err := github.NewClient(github.ClientConfig{
			BaseURL: api, Repo: repo,
			Tokens: map[ports.Identity]github.TokenSource{
				ports.IdentityLoop:     github.StaticToken(tokens["LOOP"]),
				ports.IdentityCoder:    github.StaticToken(tokens["CODER"]),
				ports.IdentityReviewer: github.StaticToken(tokens["REVIEWER"]),
			},
		})
		must(t, err)
		tr, err := github.NewTracker(c, github.TrackerConfig{
			Identities: github.Identities{Coder: actors["CODER"], Reviewer: actors["REVIEWER"], Loop: actors["LOOP"]},
			PageSize:   5,
		})
		must(t, err)
		return conformance.Env{
			Tracker: tr, Driver: d,
			Owner: actors["OWNER"], Human: actors["HUMAN"], Bot: actors["BOT"],
			Coder: actors["CODER"], Reviewer: actors["REVIEWER"], Loop: actors["LOOP"],
			BaseBranch: "main", BranchPrefix: "agent/", Gates: []string{"ci"}, FeedPage: 5,
		}
	})
}

func liveCall(t *testing.T, method, u, token string, in, out any) int {
	t.Helper()
	var body *strings.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, u, body)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

// liveUser resolves a login to its actor, with the numeric ID (I13).
func liveUser(t *testing.T, api, token, login string) ports.Actor {
	t.Helper()
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if code := liveCall(t, "GET", api+"/users/"+login, token, nil, &u); code != 200 {
		t.Fatalf("GET /users/%s: %d", login, code)
	}
	k := ports.ActorHuman
	if u.Type == "Bot" {
		k = ports.ActorBot
	}
	return ports.Actor{ID: itoa(u.ID), Login: u.Login, Kind: k}
}

// liveLabel creates a label if it is missing.
func liveLabel(t *testing.T, api, repo, token, name string) {
	t.Helper()
	code := liveCall(t, "POST", api+"/repos/"+repo+"/labels", token, map[string]any{"name": name}, nil)
	if code != 201 && code != 422 { // 422: it exists
		t.Fatalf("create label %q: %d", name, code)
	}
}
