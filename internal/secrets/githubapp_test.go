package secrets

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

// mapBackend is an in-memory Backend.
type mapBackend map[string]string

func (m mapBackend) Get(_ context.Context, name string) (string, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return "", notFound(name, "the test map")
}

// fakeGitHub is a fake GitHub token endpoint. It checks every app JWT
// against the test key and records each token request.
type fakeGitHub struct {
	t   *testing.T
	key *rsa.PublicKey
	now time.Time

	mu        sync.Mutex
	requests  []tokenRequest
	revoked   []string
	lookups   int
	grantMore map[string]string // added to every grant
	status    int               // non-zero: answer every token request with it
	message   string            // the message of that answer; "JWT" is replaced by the request's JWT
	lastJWT   string            // the JWT of the last token request
	header    http.Header
	revokeErr int
	seq       int
}

type tokenRequest struct {
	Path         string
	AppID        string
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

func (f *fakeGitHub) checkJWT(r *http.Request) (string, bool) {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig) != nil {
		return "", false
	}
	var hdr struct{ Alg, Typ string }
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "RS256" {
		return "", false
	}
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(cb, &c) != nil {
		return "", false
	}
	if c.Iat != f.now.Add(-time.Minute).Unix() || c.Exp != f.now.Add(9*time.Minute).Unix() {
		f.t.Errorf("JWT iat/exp = %d/%d, want 60 s before and 9 min after now", c.Iat, c.Exp)
	}
	return c.Iss, true
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("Accept") != "application/vnd.github+json" {
		f.t.Errorf("%s %s: missing API headers", r.Method, r.URL.Path)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		iss, ok := f.checkJWT(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"id":%s,"slug":"app-%s"}`, strings.TrimSpace(iss), strings.TrimSpace(iss))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		if _, ok := f.checkJWT(r); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.lookups++
		if r.URL.Path != "/repos/owner/repo/installation" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":4242,"permissions":{"contents":"write","metadata":"read"}}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		iss, ok := f.checkJWT(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"bad JWT"}`)
			return
		}
		var req tokenRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			f.t.Errorf("token request body: %v", err)
		}
		req.Path, req.AppID = r.URL.Path, iss
		f.requests = append(f.requests, req)
		f.lastJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.status != 0 {
			for k, v := range f.header {
				w.Header()[k] = v
			}
			w.WriteHeader(f.status)
			msg := "refused"
			if f.message != "" {
				msg = strings.ReplaceAll(f.message, "JWT", f.lastJWT)
			}
			json.NewEncoder(w).Encode(map[string]string{"message": msg})
			return
		}
		perms := map[string]string{"metadata": "read"}
		for k, v := range req.Permissions {
			perms[k] = v
		}
		for k, v := range f.grantMore {
			perms[k] = v
		}
		f.seq++
		tok := fmt.Sprintf("ghs_%036d", f.seq)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":       tok,
			"expires_at":  f.now.Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": perms,
		})
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		iss, ok := f.checkJWT(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"id":%s,"slug":"app-%s"}`, iss, iss)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/"):
		if r.Header.Get("Authorization") != "" {
			f.t.Errorf("GET %s sent credentials", r.URL.Path)
		}
		switch r.URL.Path {
		case "/users/app-1[bot]":
			fmt.Fprint(w, `{"id":9001,"login":"app-1[bot]","type":"Bot"}`)
		case "/users/app-3[bot]":
			fmt.Fprint(w, `{"id":9003,"login":"app-3[bot]","type":"Bot"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.revokeErr != 0 {
			w.WriteHeader(f.revokeErr)
			return
		}
		f.revoked = append(f.revoked, tok)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func pkcs1(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// newStore returns a store with the three apps (IDs 1, 2, 3; installations
// 11, 22 and, for the loop, found by lookup) and its fake endpoint.
func newStore(t *testing.T) (*GitHubApps, *fakeGitHub) {
	t.Helper()
	k := rsaKey(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	f := &fakeGitHub{t: t, key: &k.PublicKey, now: now}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p8, _ := x509.MarshalPKCS8PrivateKey(k)
	b := mapBackend{
		"github-app/coder/app_id":             "1",
		"github-app/coder/private_key":        pkcs1(k),
		"github-app/coder/installation_id":    "11",
		"github-app/reviewer/app_id":          "2",
		"github-app/reviewer/private_key":     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})),
		"github-app/reviewer/installation_id": "22\n",
		"github-app/loop/app_id":              " 3 ",
		"github-app/loop/private_key":         pkcs1(k),
	}
	g, err := NewGitHubApps(b, profile.Identities{
		Coder: "secret://github-app/coder", Reviewer: "secret://github-app/reviewer", Loop: "secret://github-app/loop",
	}, NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	g.BaseURL = srv.URL
	g.Now = func() time.Time { return now }
	g.Redactor.now = g.Now
	return g, f
}

// The T12 acceptance test: the token request for each identity and stage
// matches INTEGRATION-SPEC §9.1.
func TestFR13AC4_TokenRequestsMatchIntegrationSpec91(t *testing.T) {
	g, f := newStore(t)
	rows := specTable(t)
	coderRead := map[string]string{}
	for k, v := range rows["Coder, handler"] {
		coderRead[k] = v
	}
	coderRead["contents"] = "read"
	type want struct {
		path, app string
		perms     map[string]string
	}
	cases := map[Use]want{
		UseCoderPush:      {"/app/installations/11/access_tokens", "1", rows["Coder, handler"]},
		UseCoderRead:      {"/app/installations/11/access_tokens", "1", coderRead},
		UseCoderWorker:    {"/app/installations/11/access_tokens", "1", rows["Coder, worker"]},
		UseReviewerRead:   {"/app/installations/22/access_tokens", "2", rows["Reviewer, handler"]},
		UseReviewerWorker: {"/app/installations/22/access_tokens", "2", rows["Reviewer, worker"]},
		UseLoop:           {"/app/installations/4242/access_tokens", "3", rows["Loop"]},
	}
	for u, w := range cases {
		id, sc, _ := ScopeFor(u, "owner/repo")
		tok, err := g.Token(context.Background(), id, sc)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		req := f.requests[len(f.requests)-1]
		if req.Path != w.path || req.AppID != w.app {
			t.Errorf("%s: request %s as app %s, want %s as app %s", u, req.Path, req.AppID, w.path, w.app)
		}
		if !equalPerms(req.Permissions, w.perms) {
			t.Errorf("%s: permissions %v, want §9.1 %v", u, req.Permissions, w.perms)
		}
		if len(req.Repositories) != 1 || req.Repositories[0] != "repo" {
			t.Errorf("%s: repositories %v, want [repo]", u, req.Repositories)
		}
		if tok.Identity != id || !tok.ExpiresAt.Equal(f.now.Add(time.Hour)) || !strings.HasPrefix(tok.Secret(), "ghs_") {
			t.Errorf("%s: token %v expires %v", u, tok, tok.ExpiresAt)
		}
		if got := g.Redactor.Redact("x " + tok.Secret()); got != "x "+Mask {
			t.Errorf("%s: the redactor doesn't know the token", u)
		}
	}
	// The loop's installation is looked up once, then cached.
	id, sc, _ := ScopeFor(UseLoop, "owner/repo")
	if _, err := g.Token(context.Background(), id, sc); err != nil {
		t.Fatal(err)
	}
	if f.lookups != 1 {
		t.Errorf("installation looked up %d times, want 1", f.lookups)
	}
}

// FR-13 AC2: the review stage's only token can't push.
func TestFR13AC2_ReviewStageGetsNoPushToken(t *testing.T) {
	g, f := newStore(t)
	push, read, _ := StageUses("review")
	if push != "" {
		t.Fatalf("review gets push token %q", push)
	}
	id, sc, _ := ScopeFor(read, "owner/repo")
	if _, err := g.Token(context.Background(), id, sc); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[0].Permissions["contents"]; got != "read" || f.requests[0].AppID != "2" {
		t.Errorf("review token: app %s contents %q", f.requests[0].AppID, got)
	}
}

func TestFR13AC4_GrantBeyondRequestIsRevokedAndRefused(t *testing.T) {
	g, f := newStore(t)
	f.grantMore = map[string]string{"workflows": "write"}
	id, sc, _ := ScopeFor(UseCoderPush, "owner/repo")
	_, err := g.Token(context.Background(), id, sc)
	if !errors.Is(err, ports.ErrPermission) || !strings.Contains(err.Error(), "workflows: write") {
		t.Fatalf("err = %v", err)
	}
	if len(f.revoked) != 1 {
		t.Errorf("the over-granted token wasn't revoked")
	}
	// read asked, write granted is also extra.
	f.grantMore = map[string]string{"issues": "write"}
	if _, err := g.Token(context.Background(), id, sc); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("issues write accepted: %v", err)
	}
}

func TestFR13AC4_UnnarrowedScopesAreRefused(t *testing.T) {
	g, f := newStore(t)
	ctx := context.Background()
	perms := map[string]string{"contents": "read"}
	for _, sc := range []ports.Scope{
		{Permissions: perms},
		{Repos: []string{"owner/repo"}},
		{Repos: []string{"repo"}, Permissions: perms},
		{Repos: []string{"owner/repo", "other/repo"}, Permissions: perms},
		{Repos: []string{"owner/a/b"}, Permissions: perms},
		{Repos: []string{"owner/repo"}, Permissions: map[string]string{"contents": "admin"}},
	} {
		if _, err := g.Token(ctx, ports.IdentityCoder, sc); err == nil {
			t.Errorf("scope %v accepted", sc)
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("refused scopes reached GitHub: %v", f.requests)
	}
	if _, err := g.Token(ctx, ports.IdentityCoder, ports.Scope{Repos: []string{"owner/a", "Owner/b"}, Permissions: perms}); err != nil {
		t.Errorf("two repositories of one owner: %v", err)
	} else if r := f.requests[0].Repositories; len(r) != 2 || r[0] != "a" || r[1] != "b" {
		t.Errorf("repositories = %v", r)
	}
}

func TestTokenErrorsAreClassified(t *testing.T) {
	g, f := newStore(t)
	id, sc, _ := ScopeFor(UseCoderPush, "owner/repo")
	reset := f.now.Add(10 * time.Minute)
	for _, c := range []struct {
		status int
		header http.Header
		class  error
		retry  time.Time
	}{
		{http.StatusNotFound, nil, ports.ErrNotFound, time.Time{}},
		{http.StatusUnauthorized, nil, ports.ErrPermission, time.Time{}},
		{http.StatusForbidden, nil, ports.ErrPermission, time.Time{}},
		{http.StatusUnprocessableEntity, nil, ports.ErrPermission, time.Time{}},
		{http.StatusBadGateway, nil, ports.ErrTransient, time.Time{}},
		{http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}, ports.ErrRateLimited, f.now.Add(30 * time.Second)},
		{http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, ports.ErrRateLimited, reset},
		{http.StatusTooManyRequests, nil, ports.ErrRateLimited, f.now.Add(time.Minute)},
		// A secondary rate limit: 403 with only Retry-After (D11, review R7).
		{http.StatusForbidden, http.Header{"Retry-After": {"45"}}, ports.ErrRateLimited, f.now.Add(45 * time.Second)},
		{http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, ports.ErrRateLimited, f.now.Add(time.Minute)},
	} {
		f.status, f.header = c.status, c.header
		_, err := g.Token(context.Background(), id, sc)
		if !errors.Is(err, c.class) {
			t.Errorf("%d %v: err = %v, want %v", c.status, c.header, err, c.class)
			continue
		}
		if !c.retry.IsZero() {
			if at, ok := ports.RetryAt(err); !ok || !at.Equal(c.retry) {
				t.Errorf("%d %v: retry at %v, want %v", c.status, c.header, at, c.retry)
			}
		}
	}
	g.BaseURL = "http://127.0.0.1:1"
	if _, err := g.Token(context.Background(), id, sc); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("network error: %v", err)
	}
}

func TestTokenFailsOnBadAppSecrets(t *testing.T) {
	ctx := context.Background()
	perms := ports.Scope{Repos: []string{"owner/repo"}, Permissions: map[string]string{"contents": "read"}}
	for name, edit := range map[string]func(mapBackend){
		"missing app ID": func(b mapBackend) { delete(b, "github-app/coder/app_id") },
		"bad app ID":     func(b mapBackend) { b["github-app/coder/app_id"] = "abc" },
		"missing key":    func(b mapBackend) { delete(b, "github-app/coder/private_key") },
		"not PEM":        func(b mapBackend) { b["github-app/coder/private_key"] = "nope" },
		"wrong PEM type": func(b mapBackend) {
			b["github-app/coder/private_key"] = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
		},
		"bad PKCS1": func(b mapBackend) {
			b["github-app/coder/private_key"] = "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"
		},
		"bad installation ID": func(b mapBackend) { b["github-app/coder/installation_id"] = "x" },
	} {
		g, f := newStore(t)
		edit(g.Backend.(mapBackend))
		if _, err := g.Token(ctx, ports.IdentityCoder, perms); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("%s: error shows the key: %v", name, err)
		}
		if len(f.requests) != 0 {
			t.Errorf("%s: reached GitHub", name)
		}
	}
	g, _ := newStore(t)
	delete(g.Refs, ports.IdentityLoop)
	if _, err := g.Token(ctx, ports.IdentityLoop, perms); err == nil {
		t.Error("identity without a reference accepted")
	}
	g, _ = newStore(t)
	if _, err := g.Token(ctx, ports.IdentityLoop, ports.Scope{Repos: []string{"owner/unknown"}, Permissions: perms.Permissions}); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("app not installed: %v", err)
	}
	g.Redactor = nil
	if _, err := g.Token(ctx, ports.IdentityCoder, perms); err == nil {
		t.Error("no redactor accepted")
	}
	if _, err := NewGitHubApps(mapBackend{}, profile.Identities{Coder: "x", Reviewer: "secret://r", Loop: "secret://l"}, NewRedactor()); err == nil {
		t.Error("bad reference accepted")
	}
}

func TestRevoke(t *testing.T) {
	g, f := newStore(t)
	ctx := context.Background()
	id, sc, _ := ScopeFor(UseCoderPush, "owner/repo")
	tok, err := g.Token(ctx, id, sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Revoke(ctx, tok); err != nil || len(f.revoked) != 1 || f.revoked[0] != tok.Secret() {
		t.Fatalf("Revoke: %v, revoked %d", err, len(f.revoked))
	}
	f.revokeErr = http.StatusUnauthorized // already revoked
	if err := g.Revoke(ctx, tok); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	f.revokeErr = http.StatusInternalServerError
	if err := g.Revoke(ctx, tok); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("server error: %v", err)
	}
	expired := ports.NewToken("ghs_x", id, sc, f.now.Add(-time.Second))
	if err := g.Revoke(ctx, expired); err != nil {
		t.Errorf("expired token: %v", err)
	}
	if err := g.Revoke(ctx, ports.Token{}); err == nil {
		t.Error("empty token revoked")
	}
}

// Review R2: an over-granted token whose revoke fails is handed back with
// the error, and the run revokes it at Close.
func TestFR13AC4_OverGrantedTokenWhoseRevokeFailsIsRevokedAtClose(t *testing.T) {
	g, f := newStore(t)
	f.grantMore = map[string]string{"workflows": "write"}
	f.revokeErr = http.StatusInternalServerError
	id, sc, _ := ScopeFor(UseCoderPush, "owner/repo")
	tok, err := g.Token(context.Background(), id, sc)
	if !errors.Is(err, ports.ErrPermission) || !errors.Is(err, ports.ErrTransient) || tok.Secret() == "" {
		t.Fatalf("Token = %v, %v; want the token back with ErrPermission and ErrTransient", tok, err)
	}
	r := NewRun(g)
	r.Now = g.Now
	if _, err := r.Issue(context.Background(), id, sc); !errors.Is(err, ports.ErrPermission) {
		t.Fatalf("Issue: %v", err)
	}
	f.revokeErr = 0
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.revoked) != 1 || f.revoked[0] != fmt.Sprintf("ghs_%036d", 2) {
		t.Errorf("revoked %v, want the run's over-granted token", f.revoked)
	}
}

// Review R5: error text from GitHub and the app JWT are masked (D9, D11,
// FR-13 AC3).
func TestFR13AC3_ErrorTextAndAppJWTAreMasked(t *testing.T) {
	g, f := newStore(t)
	now := f.now
	g.Now = func() time.Time { return now }
	g.Redactor.now = g.Now
	ctx := context.Background()
	id, sc, _ := ScopeFor(UseCoderPush, "owner/repo")
	earlier, err := g.Token(ctx, id, sc)
	if err != nil {
		t.Fatal(err)
	}
	jwt := f.lastJWT
	if jwt == "" || g.Redactor.Redact(jwt) != Mask {
		t.Fatalf("the app JWT isn't masked")
	}
	f.status, f.message = http.StatusUnprocessableEntity, "bad JWT and "+earlier.Secret()
	_, err = g.Token(ctx, id, sc)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), Mask) || strings.Contains(err.Error(), f.lastJWT) || strings.Contains(err.Error(), earlier.Secret()) {
		t.Errorf("error shows a credential: %v", err)
	}
	// The JWT is forgotten once its 10 minutes are over.
	now = now.Add(11 * time.Minute)
	g.Redactor.Add("prune-trigger-secret", time.Time{})
	if g.Redactor.Redact(jwt) != jwt {
		t.Errorf("an expired JWT is still masked")
	}
}

func TestGitHubAppsDefaults(t *testing.T) {
	g := &GitHubApps{}
	if g.base() != DefaultGitHubAPI || g.client().Timeout != 30*time.Second || time.Since(g.now()) > time.Minute {
		t.Errorf("defaults: %s %v", g.base(), g.client().Timeout)
	}
	g.BaseURL = "http://x/"
	if g.base() != "http://x" {
		t.Errorf("base = %s", g.base())
	}
	if r := NewRun(nil); time.Since(r.now()) > time.Minute {
		t.Error("run clock")
	}
}

// TestFR13AC4_InstalledPermissions reads an app installation's permissions
// with the app's JWT, for project validate (work plan #16 D8).
func TestFR13AC4_InstalledPermissions(t *testing.T) {
	g, _ := newStore(t)
	got, err := g.InstalledPermissions(context.Background(), ports.IdentityLoop, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["contents"] != "write" || got["metadata"] != "read" {
		t.Errorf("permissions %v", got)
	}
	if _, err := g.InstalledPermissions(context.Background(), ports.IdentityLoop, "owner/other"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("no installation: %v", err)
	}
	if _, err := g.InstalledPermissions(context.Background(), ports.IdentityLoop, "bad"); err == nil {
		t.Error("bad repository accepted")
	}
}

// Work plan #22 D12: the worker reads each identity's app ID and slug with
// the app's JWT.
func TestAppReadsIDAndSlug(t *testing.T) {
	g, _ := newStore(t)
	for id, want := range map[ports.Identity]AppInfo{
		ports.IdentityCoder: {1, "app-1"}, ports.IdentityReviewer: {2, "app-2"}, ports.IdentityLoop: {3, "app-3"},
	} {
		got, err := g.App(context.Background(), id)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", id, got, err, want)
		}
	}
	if _, err := g.App(context.Background(), "nobody"); err == nil {
		t.Error("an unknown identity was read")
	}
}

// TestFR13AC1_AccountReadsAppAndBotIDs: an identity's app ID comes from GET
// /app with its JWT, and its account ID from its bot user (#23 D6).
func TestFR13AC1_AccountReadsAppAndBotIDs(t *testing.T) {
	g, _ := newStore(t)
	got, err := g.Account(context.Background(), ports.IdentityCoder)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AppAccount{AppID: 1, Slug: "app-1", BotID: 9001, BotLogin: "app-1[bot]"}); got != want {
		t.Errorf("Account = %+v, want %+v", got, want)
	}
	if _, err := g.Account(context.Background(), ports.IdentityReviewer); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing bot user: %v", err)
	}
	g.Refs[ports.IdentityLoop] = "missing"
	if _, err := g.Account(context.Background(), ports.IdentityLoop); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing secret: %v", err)
	}
}
