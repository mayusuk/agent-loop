package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// systemBrowser runs the GitHub App manifest flow in the owner's browser:
// a page on a loopback port posts the manifest to GitHub, and GitHub
// redirects back to it with the one-time code.
type systemBrowser struct {
	w io.Writer
}

// browserWait bounds how long init waits for the owner in the browser.
var browserWait = 10 * time.Minute

var manifestPage = template.Must(template.New("m").Parse(`<!doctype html>
<html><body><p>Sending the GitHub App manifest to GitHub...</p>
<form method="post" action="{{.Action}}"><input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Continue to GitHub</button></noscript></form>
<script>document.forms[0].submit()</script></body></html>`))

func (b *systemBrowser) ManifestCode(ctx context.Context, createURL string, build func(string) ([]byte, error)) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	state := hex.EncodeToString(buf)
	base := "http://" + ln.Addr().String()
	manifest, err := build(base + "/callback")
	if err != nil {
		return "", err
	}
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_ = manifestPage.Execute(w, map[string]string{"Action": createURL + "?state=" + url.QueryEscape(state), "Manifest": string(manifest)})
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state || q.Get("code") == "" {
			http.Error(w, "unexpected callback", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "The app is created. You can close this tab and go back to agent-loop init.")
		select {
		case codes <- q.Get("code"):
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	fmt.Fprintf(b.w, "   opening %s/ in your browser; confirm the app on GitHub\n", base)
	openURL(base + "/")
	ctx, cancel := context.WithTimeout(ctx, browserWait)
	defer cancel()
	select {
	case c := <-codes:
		return c, nil
	case <-ctx.Done():
		return "", fmt.Errorf("no confirmation from GitHub within %s", browserWait)
	}
}

func (b *systemBrowser) Install(_ context.Context, installURL, slug string) error {
	fmt.Fprintf(b.w, "   opening %s: install %s on this repository only (\"Only select repositories\")\n", installURL, slug)
	openURL(installURL)
	return nil
}

// openURL opens u in the default browser. A failure is ignored: init has
// printed the URL.
func openURL(u string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, u).Start()
}

// deviceFlow logs the owner in with GitHub's OAuth device flow for the
// OAuth App clientID. The token is returned, never stored.
func deviceFlow(ctx context.Context, web, clientID string, w io.Writer) (string, error) {
	post := func(path string, form url.Values, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, web+path, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: status %d", path, res.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if err := post("/login/device/code", url.Values{"client_id": {clientID}, "scope": {"repo read:org"}}, &dc); err != nil {
		return "", fmt.Errorf("starting the device flow: %w", err)
	}
	if dc.DeviceCode == "" {
		return "", errors.New("starting the device flow: GitHub returned no device code")
	}
	fmt.Fprintf(w, "   open %s and enter the code %s\n", dc.VerificationURI, dc.UserCode)
	interval := time.Duration(max(dc.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(dc.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		var tr struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		if err := post("/login/oauth/access_token", url.Values{"client_id": {clientID}, "device_code": {dc.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &tr); err != nil {
			return "", fmt.Errorf("device flow: %w", err)
		}
		switch tr.Error {
		case "":
			if tr.AccessToken != "" {
				return tr.AccessToken, nil
			}
			return "", errors.New("device flow: GitHub returned no token")
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return "", fmt.Errorf("device flow: %s", tr.Error)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
	return "", errors.New("device flow: the code expired before you entered it")
}
