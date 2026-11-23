package githubtest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// App is a GitHub App installed on the stub's repository. Its bot user acts
// with the installation tokens the stub mints for it.
type App struct {
	ID           int64
	Slug         string
	Bot          *User
	Installation int64
	// Permissions are the installation's permissions.
	Permissions map[string]string
}

// TokenRequest is one installation token request the stub answered.
type TokenRequest struct {
	AppID        int64
	Installation int64
	Repositories []string
	Permissions  map[string]string
}

type appState struct {
	apps     map[int64]*App // by app ID
	tokenReq []TokenRequest
	minted   int
}

// AddApp installs an app on the repository: its bot user <slug>[bot] gets
// botID, and its installation has perms. The stub answers GET /app,
// GET /repos/{o}/{r}/installation and POST
// /app/installations/{id}/access_tokens for a JWT whose iss is the app ID
// (the signature isn't checked), GET /users/<login> without credentials,
// and DELETE /installation/token.
func (s *Server) AddApp(id int64, slug string, botID int64, perms map[string]string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	bot := &User{ID: botID, Login: slug + "[bot]", Type: "Bot", Token: "tok-" + slug + "[bot]"}
	s.users[bot.Token] = bot
	s.appIDs[botID] = id
	a := &App{ID: id, Slug: slug, Bot: bot, Installation: id * 1000, Permissions: maps.Clone(perms)}
	if s.apps == nil {
		s.apps = map[int64]*App{}
	}
	s.apps[id] = a
	return a
}

// TokenRequests returns the installation token requests answered so far.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TokenRequest(nil), s.tokenReq...)
}

// SetDefaultBranch sets the repository's default branch, creating it at
// the current default branch's tip if it doesn't exist.
func (s *Server) SetDefaultBranch(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.branches[name]; !ok {
		s.branches[name] = s.branches[s.defaultBranch]
	}
	s.defaultBranch = name
}

var accessTokensPath = regexp.MustCompile(`^/app/installations/(\d+)/access_tokens$`)

// jwtApp returns the app a JWT bearer names, if the header holds a JWT.
func (s *Server) jwtApp(r *http.Request) (*App, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, true
	}
	var c struct {
		Iss json.RawMessage `json:"iss"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return nil, true
	}
	id, err := strconv.ParseInt(strings.Trim(string(c.Iss), `"`), 10, 64)
	if err != nil {
		return nil, true
	}
	return s.apps[id], true
}

// handleApps answers the app and token endpoints. ok is false for any
// other call.
func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) (status int, login string, ok bool) {
	path := r.URL.Path
	if login, isUsers := strings.CutPrefix(path, "/users/"); isUsers && r.Method == http.MethodGet {
		if u := s.userByLogin(login); u != nil {
			return s.write(w, r, "", response{body: u.json()}), "", true
		}
		return s.write(w, r, "", errorResponse(http.StatusNotFound, "Not Found")), "", true
	}
	if path == "/installation/token" && r.Method == http.MethodDelete {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		u := s.users[tok]
		if u == nil {
			return s.write(w, r, "", errorResponse(http.StatusUnauthorized, "Bad credentials")), "", true
		}
		delete(s.users, tok)
		return s.write(w, r, "", response{}), u.Login, true
	}
	if m := conversionPath.FindStringSubmatch(path); m != nil && r.Method == http.MethodPost {
		return s.write(w, r, "", s.convertManifest(m[1])), "", true
	}
	app, isJWT := s.jwtApp(r)
	if !isJWT {
		return 0, "", false
	}
	if app == nil {
		return s.write(w, r, "", errorResponse(http.StatusUnauthorized, "A JSON web token could not be decoded")), "", true
	}
	switch {
	case r.Method == http.MethodGet && path == "/app":
		return s.write(w, r, "", response{body: map[string]any{"id": app.ID, "slug": app.Slug}}), app.Slug, true
	case r.Method == http.MethodGet && path == s.repo()+"/installation":
		if app.Installation == 0 {
			return s.write(w, r, "", errorResponse(http.StatusNotFound, "Not Found")), app.Slug, true
		}
		return s.write(w, r, "", response{body: map[string]any{"id": app.Installation, "permissions": app.Permissions, "repository_selection": "selected"}}), app.Slug, true
	case r.Method == http.MethodPost && accessTokensPath.MatchString(path):
		inst, _ := strconv.ParseInt(accessTokensPath.FindStringSubmatch(path)[1], 10, 64)
		if inst == 0 || inst != app.Installation {
			return s.write(w, r, "", errorResponse(http.StatusNotFound, "Not Found")), app.Slug, true
		}
		var in struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.tokenReq = append(s.tokenReq, TokenRequest{AppID: app.ID, Installation: inst, Repositories: in.Repositories, Permissions: in.Permissions})
		granted := map[string]string{"metadata": "read"}
		for k, v := range in.Permissions {
			if app.Permissions[k] == "" {
				return s.write(w, r, "", errorResponse(http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")), app.Slug, true
			}
			granted[k] = v
		}
		s.minted++
		tok := fmt.Sprintf("ghs_stub_%s_%d", app.Slug, s.minted)
		s.users[tok] = app.Bot
		// The expiry is on the wall clock, which token holders compare it
		// with, not on the stub's clock.
		return s.write(w, r, "", response{status: http.StatusCreated, body: map[string]any{
			"token": tok, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "permissions": granted}}), app.Slug, true
	}
	return s.write(w, r, "", errorResponse(http.StatusNotFound, "Not Found")), app.Slug, true
}
