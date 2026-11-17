package githubtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// The parts of the stub agent-loop init uses: the authenticated user, the
// repository's permissions, label creation, the GitHub App manifest flow
// and the "up to date" setting of classic protection.

// setupState is the manifest flow's pending manifests and the
// repository's owner type.
type setupState struct {
	manifests map[string]map[string]any // conversion code to manifest
	ownerType string
}

var (
	appKeyOnce sync.Once
	appKeyPEM  string
)

// appKey is the private key every manifest app gets. The stub doesn't
// check JWT signatures, but the key must parse.
func appKey() string {
	appKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		appKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return appKeyPEM
}

// SetOwnerType sets the repository owner's account type: "User" (the
// default) or "Organization".
func (s *Server) SetOwnerType(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerType = t
}

// CreateFromManifest is the browser half of the manifest flow: the owner
// posts the manifest to GitHub and confirms, and GitHub redirects with a
// one-time code, which it returns.
func (s *Server) CreateFromManifest(manifest []byte) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		return "", fmt.Errorf("githubtest: manifest: %w", err)
	}
	if name, _ := m["name"].(string); name == "" {
		return "", fmt.Errorf("githubtest: manifest has no name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifests == nil {
		s.manifests = map[string]map[string]any{}
	}
	code := fmt.Sprintf("code%d", s.id())
	s.manifests[code] = m
	return code, nil
}

// InstallApp is the browser step that installs an app on the repository
// only. It reports whether the app exists.
func (s *Server) InstallApp(slug string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.apps {
		if a.Slug == slug {
			a.Installation = a.ID * 1000
			return true
		}
	}
	return false
}

// AppBySlug returns the app with the slug, or nil.
func (s *Server) AppBySlug(slug string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.apps {
		if a.Slug == slug {
			return a
		}
	}
	return nil
}

// Labels returns the repository's label names, sorted.
func (s *Server) Labels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.listLabels(nil, nil, nil, nil).list {
		out = append(out, l.(map[string]any)["name"].(string))
	}
	return out
}

// UpToDate reports whether classic protection requires branches to be up
// to date.
func (s *Server) UpToDate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strict
}

var conversionPath = regexp.MustCompile(`^/app-manifests/([^/]+)/conversions$`)

var slugChars = regexp.MustCompile(`[^a-z0-9-]+`)

// convertManifest answers POST /app-manifests/{code}/conversions: it
// creates the app, not yet installed, and returns its credentials.
func (s *Server) convertManifest(code string) response {
	m, ok := s.manifests[code]
	if !ok {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	delete(s.manifests, code)
	name := m["name"].(string)
	slug := strings.Trim(slugChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	perms := map[string]string{}
	if dp, ok := m["default_permissions"].(map[string]any); ok {
		for k, v := range dp {
			perms[k], _ = v.(string)
		}
	}
	id := s.id()
	bot := &User{ID: s.id(), Login: slug + "[bot]", Type: "Bot", Token: "tok-" + slug + "[bot]"}
	s.users[bot.Token] = bot
	s.appIDs[bot.ID] = id
	if s.apps == nil {
		s.apps = map[int64]*App{}
	}
	s.apps[id] = &App{ID: id, Slug: slug, Bot: bot, Permissions: perms}
	return response{status: http.StatusCreated, body: map[string]any{"id": id, "slug": slug, "name": name,
		"pem": appKey(), "client_id": "Iv1." + slug, "client_secret": "cs-" + slug, "webhook_secret": nil}}
}

// createLabel answers POST /repos/{o}/{r}/labels.
func (s *Server) createLabel(_ *User, _ []string, _ url.Values, in map[string]any) response {
	name, _ := in["name"].(string)
	if name == "" {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed")
	}
	if _, ok := s.labels[strings.ToLower(name)]; ok {
		return errorResponse(http.StatusUnprocessableEntity, "Validation Failed: already_exists")
	}
	s.labels[strings.ToLower(name)] = name
	return response{status: http.StatusCreated, body: map[string]any{"name": name, "color": in["color"]}}
}

// setClassicChecks answers PATCH
// /repos/{o}/{r}/branches/{b}/protection/required_status_checks.
func (s *Server) setClassicChecks(u *User, m []string, _ url.Values, in map[string]any) response {
	if r, no := s.admin(u); no {
		return r
	}
	name, _ := url.PathUnescape(m[1])
	if name != s.defaultBranch || !s.classic || len(s.classicChecks) == 0 {
		return errorResponse(http.StatusNotFound, "Required status checks not enabled")
	}
	if v, ok := in["strict"].(bool); ok {
		s.strict = v
	}
	return s.getClassicChecks(u, m, nil, nil)
}
