package secrets

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

// DefaultGitHubAPI is the GitHub REST API's base URL.
const DefaultGitHubAPI = "https://api.github.com"

// The fields of a GitHub App identity's secret (work plan D1).
const (
	FieldAppID          = "app_id"
	FieldPrivateKey     = "private_key"
	FieldInstallationID = "installation_id"
)

// GitHubApps is the SecretStore for GitHub: each identity is a GitHub App,
// and every token is an installation token narrowed with permissions and
// repositories (INTEGRATION-SPEC §9.1). Every token it issues is given to
// the Redactor until it expires.
type GitHubApps struct {
	// Backend holds the apps' IDs and private keys.
	Backend Backend
	// Refs maps each identity to its secret name (ParseRef of the profile's
	// reference).
	Refs map[ports.Identity]string
	// Redactor learns every token issued. Required.
	Redactor *Redactor
	// BaseURL is the REST API. Empty means DefaultGitHubAPI.
	BaseURL string
	// Client makes the calls. Nil means a client with a 30 s timeout.
	Client *http.Client
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu            sync.Mutex
	installations map[string]string // identity "|" owner -> installation ID
}

// NewGitHubApps returns the store for the profile's identities.
func NewGitHubApps(b Backend, ids profile.Identities, r *Redactor) (*GitHubApps, error) {
	refs := map[ports.Identity]string{}
	for id, ref := range map[ports.Identity]string{
		ports.IdentityCoder: ids.Coder, ports.IdentityReviewer: ids.Reviewer, ports.IdentityLoop: ids.Loop,
	} {
		name, err := ParseRef(ref)
		if err != nil {
			return nil, fmt.Errorf("identities.%s: %w", id, err)
		}
		refs[id] = name
	}
	return &GitHubApps{Backend: b, Refs: refs, Redactor: r}, nil
}

var _ ports.SecretStore = (*GitHubApps)(nil)

func (g *GitHubApps) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GitHubApps) base() string {
	if g.BaseURL != "" {
		return strings.TrimSuffix(g.BaseURL, "/")
	}
	return DefaultGitHubAPI
}

func (g *GitHubApps) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Token issues an installation token for identity id, narrowed to s. When
// GitHub grants more than s asks and revoking that token fails, Token
// returns the token together with an ErrPermission error, so the caller can
// revoke it later; on every other error the token is empty.
func (g *GitHubApps) Token(ctx context.Context, id ports.Identity, s ports.Scope) (ports.Token, error) {
	if g.Redactor == nil {
		return ports.Token{}, errors.New("github apps: no redactor")
	}
	owner, names, err := splitRepos(s.Repos)
	if err != nil {
		return ports.Token{}, err
	}
	if len(s.Permissions) == 0 {
		return ports.Token{}, errors.New("github apps: a token needs at least one permission")
	}
	for k, v := range s.Permissions {
		if v != "read" && v != "write" {
			return ports.Token{}, fmt.Errorf("github apps: permission %s: %q is not read or write", k, v)
		}
	}
	jwt, err := g.appJWT(ctx, id)
	if err != nil {
		return ports.Token{}, err
	}
	inst, err := g.installation(ctx, id, owner, names[0], jwt)
	if err != nil {
		return ports.Token{}, err
	}
	body, _ := json.Marshal(map[string]any{"repositories": names, "permissions": s.Permissions})
	var resp struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := g.call(ctx, http.MethodPost, "/app/installations/"+inst+"/access_tokens", "Bearer "+jwt, body, http.StatusCreated, &resp); err != nil {
		return ports.Token{}, fmt.Errorf("github apps: %s token: %w", id, err)
	}
	if resp.Token == "" || resp.ExpiresAt.IsZero() {
		return ports.Token{}, fmt.Errorf("github apps: %s token: response has no token or expiry: %w", id, ports.ErrTransient)
	}
	g.Redactor.Add(resp.Token, resp.ExpiresAt)
	tok := ports.NewToken(resp.Token, id, s, resp.ExpiresAt)
	if extra := extraPermissions(s.Permissions, resp.Permissions); len(extra) > 0 {
		err := fmt.Errorf("github apps: %s token was granted more than asked (%s): %w", id, strings.Join(extra, ", "), ports.ErrPermission)
		if rerr := g.Revoke(context.WithoutCancel(ctx), tok); rerr != nil {
			// Hand the live token back with the error, so the caller (Run)
			// can revoke it later.
			return tok, errors.Join(err, rerr)
		}
		return ports.Token{}, err
	}
	return tok, nil
}

// Revoke deletes the token. A token that is already revoked or expired
// counts as revoked.
func (g *GitHubApps) Revoke(ctx context.Context, t ports.Token) error {
	if t.Secret() == "" {
		return errors.New("github apps: revoking an empty token")
	}
	if !t.ExpiresAt.IsZero() && !g.now().Before(t.ExpiresAt) {
		return nil
	}
	err := g.call(ctx, http.MethodDelete, "/installation/token", "Bearer "+t.Secret(), nil, http.StatusNoContent, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusUnauthorized {
		return nil
	}
	if err != nil {
		return fmt.Errorf("github apps: revoking a %s token: %w", t.Identity, err)
	}
	return nil
}

// splitRepos checks that every repository is "owner/name" with one owner,
// and returns the owner and the names.
func splitRepos(repos []string) (string, []string, error) {
	if len(repos) == 0 {
		return "", nil, errors.New("github apps: a token must be narrowed to at least one repository")
	}
	var owner string
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		o, n, ok := strings.Cut(r, "/")
		if !ok || o == "" || n == "" || strings.Contains(n, "/") {
			return "", nil, fmt.Errorf("github apps: repository %q is not owner/name", r)
		}
		if owner != "" && !strings.EqualFold(o, owner) {
			return "", nil, fmt.Errorf("github apps: repositories %q and %q have different owners", repos[0], r)
		}
		owner = o
		names = append(names, n)
	}
	return owner, names, nil
}

// extraPermissions lists what got grants beyond want. metadata: read is
// granted to every token and is not extra.
func extraPermissions(want, got map[string]string) []string {
	var extra []string
	for k, v := range got {
		w, ok := want[k]
		switch {
		case k == "metadata" && v == "read":
		case !ok, w == "read" && v != "read":
			extra = append(extra, k+": "+v)
		}
	}
	return extra
}

// appJWT signs the app's JWT (work plan D10).
func (g *GitHubApps) appJWT(ctx context.Context, id ports.Identity) (string, error) {
	name, ok := g.Refs[id]
	if !ok {
		return "", fmt.Errorf("github apps: no secret reference for identity %q", id)
	}
	appID, err := g.Backend.Get(ctx, name+"/"+FieldAppID)
	if err != nil {
		return "", fmt.Errorf("github apps: %s app ID: %w", id, err)
	}
	appID = strings.TrimSpace(appID)
	if _, err := strconv.ParseUint(appID, 10, 64); err != nil {
		return "", fmt.Errorf("github apps: %s/%s is not a numeric app ID", name, FieldAppID)
	}
	keyPEM, err := g.Backend.Get(ctx, name+"/"+FieldPrivateKey)
	if err != nil {
		return "", fmt.Errorf("github apps: %s private key: %w", id, err)
	}
	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return "", fmt.Errorf("github apps: %s/%s: %v", name, FieldPrivateKey, err)
	}
	now := g.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github apps: signing the %s app JWT: %v", id, err)
	}
	jwt := signing + "." + enc.EncodeToString(sig)
	g.Redactor.Add(jwt, now.Add(10*time.Minute))
	return jwt, nil
}

func parseRSAKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("not a valid PKCS #1 RSA key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("not a valid PKCS #8 key")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		return rk, nil
	}
	return nil, fmt.Errorf("PEM block %q is not a private key", block.Type)
}

// InstalledPermissions returns the permissions the identity's app
// installation has on repo ("owner/name"), read with the app's JWT from
// GET /repos/{owner}/{repo}/installation, for project validate
// (INTEGRATION-SPEC §9.1, work plan #16 D8).
func (g *GitHubApps) InstalledPermissions(ctx context.Context, id ports.Identity, repo string) (map[string]string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("github apps: repository %q is not owner/name", repo)
	}
	jwt, err := g.appJWT(ctx, id)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := g.call(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/installation", "Bearer "+jwt, nil, http.StatusOK, &resp); err != nil {
		return nil, fmt.Errorf("github apps: reading the %s app's installation on %s: %w", id, repo, err)
	}
	if resp.Permissions == nil {
		resp.Permissions = map[string]string{}
	}
	return resp.Permissions, nil
}

// installation returns the identity's installation ID: the stored one, or
// the one found for the repository and cached.
func (g *GitHubApps) installation(ctx context.Context, id ports.Identity, owner, repo, jwt string) (string, error) {
	name := g.Refs[id]
	v, err := g.Backend.Get(ctx, name+"/"+FieldInstallationID)
	if err == nil {
		v = strings.TrimSpace(v)
		if _, perr := strconv.ParseUint(v, 10, 64); perr != nil {
			return "", fmt.Errorf("github apps: %s/%s is not a numeric installation ID", name, FieldInstallationID)
		}
		return v, nil
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return "", fmt.Errorf("github apps: %s installation ID: %w", id, err)
	}
	key := string(id) + "|" + strings.ToLower(owner)
	g.mu.Lock()
	cached, ok := g.installations[key]
	g.mu.Unlock()
	if ok {
		return cached, nil
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := g.call(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/installation", "Bearer "+jwt, nil, http.StatusOK, &resp); err != nil {
		return "", fmt.Errorf("github apps: finding the %s app's installation on %s/%s: %w", id, owner, repo, err)
	}
	if resp.ID <= 0 {
		return "", fmt.Errorf("github apps: %s installation has no ID: %w", id, ports.ErrTransient)
	}
	inst := strconv.FormatInt(resp.ID, 10)
	g.mu.Lock()
	if g.installations == nil {
		g.installations = map[string]string{}
	}
	g.installations[key] = inst
	g.mu.Unlock()
	return inst, nil
}

// statusError is a GitHub response with an unexpected status.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("GitHub answered %d: %s", e.code, e.msg)
	}
	return fmt.Sprintf("GitHub answered %d", e.code)
}

// call makes one REST call and decodes a JSON answer into out. Errors are
// classified (work plan D11) and redacted.
func (g *GitHubApps) call(ctx context.Context, method, path, auth string, body []byte, want int, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", auth)
	req.Header.Set("User-Agent", "agent-loop")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", g.Redactor.Redact(err.Error()), ports.ErrTransient)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the answer: %v: %w", err, ports.ErrTransient)
	}
	if res.StatusCode == want {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding the answer: %v: %w", err, ports.ErrTransient)
		}
		return nil
	}
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &m)
	se := &statusError{code: res.StatusCode, msg: g.Redactor.Redact(m.Message)}
	return classify(se, res.Header, g.now())
}

func classify(se *statusError, h http.Header, now time.Time) error {
	retry, limited := retryAt(h, now)
	switch {
	case se.code == http.StatusTooManyRequests || (se.code == http.StatusForbidden && limited):
		if retry.IsZero() {
			retry = now.Add(time.Minute)
		}
		return ports.RateLimited(retry, se)
	case se.code == http.StatusNotFound:
		return fmt.Errorf("%w: %w", se, ports.ErrNotFound)
	case se.code == http.StatusUnauthorized, se.code == http.StatusForbidden, se.code == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", se, ports.ErrPermission)
	case se.code >= 500:
		return fmt.Errorf("%w: %w", se, ports.ErrTransient)
	}
	return se
}

// retryAt reads GitHub's rate-limit headers. limited is true when they say
// the call was refused for a rate limit.
func retryAt(h http.Header, now time.Time) (t time.Time, limited bool) {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return now.Add(time.Duration(n) * time.Second), true
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
		return time.Time{}, true
	}
	return time.Time{}, false
}

// AppInfo is a GitHub App as GET /app reports it.
type AppInfo struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// App reads identity id's app: its ID and slug, from which its bot user
// is found (work plan #22 D12).
func (g *GitHubApps) App(ctx context.Context, id ports.Identity) (AppInfo, error) {
	if g.Redactor == nil {
		return AppInfo{}, errors.New("github apps: no redactor")
	}
	jwt, err := g.appJWT(ctx, id)
	if err != nil {
		return AppInfo{}, err
	}
	var a AppInfo
	if err := g.call(ctx, http.MethodGet, "/app", "Bearer "+jwt, nil, http.StatusOK, &a); err != nil {
		return AppInfo{}, fmt.Errorf("github apps: reading the %s app: %w", id, err)
	}
	if a.ID == 0 || a.Slug == "" {
		return AppInfo{}, fmt.Errorf("github apps: the %s app has no ID or slug", id)
	}
	return a, nil
}
