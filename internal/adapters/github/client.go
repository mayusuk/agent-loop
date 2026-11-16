package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// TokenSource gives the current token of one identity. *secrets.Lease is
// one; StaticToken is another, for tests and the live sandbox.
type TokenSource interface {
	Token(ctx context.Context) (ports.Token, error)
}

// StaticToken is a TokenSource that always returns the same secret.
type StaticToken string

// Token returns the secret as a token.
func (s StaticToken) Token(context.Context) (ports.Token, error) {
	return ports.NewToken(string(s), "", ports.Scope{}, time.Time{}), nil
}

// Rate-limit resources the budget tracks (work plan D12).
const (
	ResourceCore    = "core"
	ResourceGraphQL = "graphql"
)

// DefaultReserve is how much of each resource's limit the budget keeps back
// (work plan D12).
var DefaultReserve = map[string]int{ResourceCore: 100, ResourceGraphQL: 200}

// ClientConfig configures a Client.
type ClientConfig struct {
	BaseURL string // "https://api.github.com" when empty
	Repo    string // "owner/name"
	// Tokens holds a token source for each identity the client acts as.
	Tokens map[ports.Identity]TokenSource
	HTTP   *http.Client     // http.DefaultClient when nil
	Now    func() time.Time // time.Now when nil
	// Reserve is the remaining count per resource at which calls are
	// refused until the reset. DefaultReserve when nil.
	Reserve map[string]int
}

// Client is one GitHub repository's API client, shared by the WorkTracker
// and the CodeHost so they spend one rate-limit budget (INTEGRATION-SPEC
// §9.4). It is safe for concurrent use.
type Client struct {
	base        string
	owner, name string
	tokens      map[ports.Identity]TokenSource
	http        *http.Client
	now         func() time.Time
	reserve     map[string]int

	mu      sync.Mutex
	budgets map[budgetKey]*budget
	etags   map[string]cached
}

type budgetKey struct {
	id       ports.Identity
	resource string
}

// budget is what GitHub last said about one identity's limit on one
// resource.
type budget struct {
	known      bool
	remaining  int
	reset      time.Time
	retryAfter time.Time // a Retry-After (secondary limit) in force
}

type cached struct {
	etag string
	body []byte
	link string
}

// NewClient returns a client for cfg.Repo.
func NewClient(cfg ClientConfig) (*Client, error) {
	owner, name, ok := strings.Cut(cfg.Repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("github: repository %q is not owner/name", cfg.Repo)
	}
	c := &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		owner:   owner,
		name:    name,
		tokens:  cfg.Tokens,
		http:    cfg.HTTP,
		now:     cfg.Now,
		reserve: cfg.Reserve,
		budgets: map[budgetKey]*budget{},
		etags:   map[string]cached{},
	}
	if c.base == "" {
		c.base = "https://api.github.com"
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.reserve == nil {
		c.reserve = DefaultReserve
	}
	return c, nil
}

// Owner is the repository's owner login.
func (c *Client) Owner() string { return c.owner }

// Name is the repository's name.
func (c *Client) Name() string { return c.name }

// Repo is "owner/name".
func (c *Client) Repo() string { return c.owner + "/" + c.name }

// RepoPath returns "/repos/{owner}/{name}" followed by the formatted
// suffix, for example c.RepoPath("/issues/%d", 42).
func (c *Client) RepoPath(format string, args ...any) string {
	return "/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.name) + fmt.Sprintf(format, args...)
}

// StatusError is an HTTP error from GitHub. It wraps the port error class.
type StatusError struct {
	Method, Path string
	Code         int
	Message      string // GitHub's "message", if any
	class        error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Code, e.Message)
}

// Unwrap returns the port error class.
func (e *StatusError) Unwrap() error { return e.class }

// Get decodes the JSON response of a GET into out (nil to discard). GETs are
// conditional: a 304 serves the cached body (work plan D12).
func (c *Client) Get(ctx context.Context, id ports.Identity, path string, q url.Values, out any) error {
	body, _, err := c.do(ctx, id, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	return decode(body, out, path)
}

// Send makes a write call with in as the JSON body (nil for none) and decodes
// the response into out (nil to discard).
func (c *Client) Send(ctx context.Context, id ports.Identity, method, path string, in, out any) error {
	var raw []byte
	if in != nil {
		var err error
		if raw, err = json.Marshal(in); err != nil {
			return fmt.Errorf("github: %s %s: %w", method, path, err)
		}
	}
	body, _, err := c.do(ctx, id, method, path, nil, raw)
	if err != nil {
		return err
	}
	return decode(body, out, path)
}

// Pages calls fn with each page of a list GET, in order, following the Link
// header. fn returns false to stop early.
func (c *Client) Pages(ctx context.Context, id ports.Identity, path string, q url.Values, fn func(page json.RawMessage) (more bool, err error)) error {
	q = cloneValues(q)
	if q.Get("per_page") == "" {
		q.Set("per_page", "100")
	}
	next := ""
	for {
		var body []byte
		var link string
		var err error
		if next == "" {
			body, link, err = c.do(ctx, id, http.MethodGet, path, q, nil)
		} else {
			body, link, err = c.doURL(ctx, id, http.MethodGet, next, path, nil)
		}
		if err != nil {
			return err
		}
		more, err := fn(body)
		if err != nil || !more {
			return err
		}
		if next = nextLink(link); next == "" {
			return nil
		}
	}
}

// List returns every element of a list GET, across pages.
func List[T any](ctx context.Context, c *Client, id ports.Identity, path string, q url.Values) ([]T, error) {
	var all []T
	err := c.Pages(ctx, id, path, q, func(page json.RawMessage) (bool, error) {
		var xs []T
		if err := decode(page, &xs, path); err != nil {
			return false, err
		}
		all = append(all, xs...)
		return true, nil
	})
	return all, err
}

// GraphQL runs a query and decodes its "data" into out.
func (c *Client) GraphQL(ctx context.Context, id ports.Identity, query string, vars map[string]any, out any) error {
	raw, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("github: graphql: %w", err)
	}
	body, _, err := c.do(ctx, id, http.MethodPost, "/graphql", nil, raw)
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := decode(body, &res, "/graphql"); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		e := res.Errors[0]
		switch e.Type {
		case "NOT_FOUND":
			return fmt.Errorf("github: graphql: %s: %w", e.Message, ports.ErrNotFound)
		case "RATE_LIMITED":
			return ports.RateLimited(c.now().Add(time.Minute), fmt.Errorf("github: graphql: %s", e.Message))
		case "FORBIDDEN":
			return fmt.Errorf("github: graphql: %s: %w", e.Message, ports.ErrPermission)
		}
		return fmt.Errorf("github: graphql: %s: %w", e.Message, ports.ErrTransient)
	}
	return decode(res.Data, out, "/graphql")
}

func (c *Client) do(ctx context.Context, id ports.Identity, method, path string, q url.Values, body []byte) ([]byte, string, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return c.doURL(ctx, id, method, u, path, body)
}

func resourceOf(path string) string {
	if path == "/graphql" {
		return ResourceGraphQL
	}
	return ResourceCore
}

// doURL makes one call to the absolute URL u. path names the call in errors
// and picks the budget.
func (c *Client) doURL(ctx context.Context, id ports.Identity, method, u, path string, body []byte) ([]byte, string, error) {
	resource := resourceOf(path)
	if err := c.allow(id, resource); err != nil {
		return nil, "", err
	}
	src, ok := c.tokens[id]
	if !ok {
		return nil, "", fmt.Errorf("github: no token for identity %q: %w", id, ports.ErrPermission)
	}
	tok, err := src.Token(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("github: token for %q: %w", id, err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, "", fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+tok.Secret())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cacheKey := string(id) + " " + u
	var hit cached
	if method == http.MethodGet {
		c.mu.Lock()
		hit = c.etags[cacheKey]
		c.mu.Unlock()
		if hit.etag != "" {
			req.Header.Set("If-None-Match", hit.etag)
		}
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("github: %s %s: %v: %w", method, path, err, ports.ErrTransient)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", fmt.Errorf("github: %s %s: reading the response: %v: %w", method, path, err, ports.ErrTransient)
	}
	c.observe(id, resource, res)
	switch {
	case res.StatusCode == http.StatusNotModified && hit.etag != "":
		return hit.body, hit.link, nil
	case res.StatusCode >= 200 && res.StatusCode < 300:
		link := res.Header.Get("Link")
		if method == http.MethodGet {
			if et := res.Header.Get("ETag"); et != "" {
				c.mu.Lock()
				c.etags[cacheKey] = cached{etag: et, body: data, link: link}
				c.mu.Unlock()
			}
		}
		return data, link, nil
	}
	return nil, "", c.classify(method, path, res, data)
}

// allow refuses a call locally while the budget is spent (work plan D12).
func (c *Client) allow(id ports.Identity, resource string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.budgets[budgetKey{id, resource}]
	if b == nil {
		return nil
	}
	now := c.now()
	if now.Before(b.retryAfter) {
		return ports.RateLimited(b.retryAfter, fmt.Errorf("github: %s %s: secondary rate limit", id, resource))
	}
	if b.known && b.remaining <= c.reserve[resource] && now.Before(b.reset) {
		return ports.RateLimited(b.reset, fmt.Errorf("github: %s %s: %d calls left, reserve %d", id, resource, b.remaining, c.reserve[resource]))
	}
	return nil
}

// observe records the rate-limit headers of a response.
func (c *Client) observe(id ports.Identity, resource string, res *http.Response) {
	h := res.Header
	if r := h.Get("X-RateLimit-Resource"); r != "" {
		resource = r
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := budgetKey{id, resource}
	b := c.budgets[k]
	if b == nil {
		b = &budget{}
		c.budgets[k] = b
	}
	if n, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
		b.known, b.remaining = true, n
		if r, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			b.reset = time.Unix(r, 0)
		}
	}
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			b.retryAfter = c.now().Add(time.Duration(n) * time.Second)
		}
	}
}

// Budget reports what GitHub last said about an identity's limit on a
// resource: the calls left and when the limit resets.
func (c *Client) Budget(id ports.Identity, resource string) (remaining int, reset time.Time, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.budgets[budgetKey{id, resource}]
	if b == nil || !b.known {
		return 0, time.Time{}, false
	}
	return b.remaining, b.reset, true
}

// classify maps an error response to a port error class (work plan D11).
func (c *Client) classify(method, path string, res *http.Response, data []byte) error {
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &m)
	se := &StatusError{Method: method, Path: path, Code: res.StatusCode, Message: m.Message}
	h := res.Header
	limited := h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != ""
	switch {
	case res.StatusCode == http.StatusTooManyRequests || (res.StatusCode == http.StatusForbidden && limited):
		return ports.RateLimited(c.retryAt(h), se)
	case res.StatusCode == http.StatusNotFound, res.StatusCode == http.StatusGone:
		se.class = ports.ErrNotFound
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden:
		se.class = ports.ErrPermission
	case res.StatusCode == http.StatusConflict, res.StatusCode == http.StatusPreconditionFailed,
		res.StatusCode == http.StatusUnprocessableEntity:
		se.class = ports.ErrConflict
	default:
		se.class = ports.ErrTransient
	}
	return se
}

func (c *Client) retryAt(h http.Header) time.Time {
	now := c.now()
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return now.Add(time.Duration(n) * time.Second)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if r, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(r, 0)
		}
	}
	return now.Add(time.Minute)
}

func decode(body []byte, out any, path string) error {
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github: %s: unreadable response: %v: %w", path, err, ports.ErrTransient)
	}
	return nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(link string) string {
	if m := linkNext.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// isStatus reports whether err is a StatusError with one of the codes.
func isStatus(err error, codes ...int) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.Code == c {
			return true
		}
	}
	return false
}
