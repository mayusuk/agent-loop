package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "al")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TECHNICAL-SPEC §11: the socket's directory is 0700 and the socket 0600,
// also when the directory already existed with a looser mode.
func TestT13_SocketAndDirectoryModes(t *testing.T) {
	for _, existing := range []bool{false, true} {
		dir := filepath.Join(shortDir(t), "cfg")
		if existing {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		sock := filepath.Join(dir, "api.sock")
		l, err := Listen(sock)
		if err != nil {
			t.Fatal(err)
		}
		if got := mode(t, dir); got != 0o700 {
			t.Errorf("existing=%v: directory mode %o, want 700", existing, got)
		}
		if got := mode(t, sock); got != 0o600 {
			t.Errorf("existing=%v: socket mode %o, want 600", existing, got)
		}
		l.Close()
	}
}

func TestListenRefusesLiveSocketAndReplacesStaleOne(t *testing.T) {
	sock := filepath.Join(shortDir(t), "api.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(sock); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second listen: %v", err)
	}
	l.Close()
	// A socket file with no server behind it.
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ul.SetUnlinkOnClose(false)
	ul.Close()
	l, err = Listen(sock)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	l.Close()

	plain := filepath.Join(shortDir(t), "file")
	os.WriteFile(plain, nil, 0o600)
	if _, err := Listen(plain); err == nil {
		t.Fatal("a plain file was replaced")
	}
}

type stubWorkflows struct{ signals []string }

func (s *stubWorkflows) Signal(_ context.Context, id, name string, _ any) error {
	s.signals = append(s.signals, id+" "+name)
	return nil
}
func (s *stubWorkflows) QueryProject(context.Context, string) (workflows.ProjectView, error) {
	return workflows.ProjectView{}, ErrNotRunning
}
func (s *stubWorkflows) QueryDeployment(context.Context) (workflows.DeploymentState, error) {
	return workflows.DeploymentState{Paused: true}, nil
}

// TECHNICAL-SPEC §11: the gateway needs the bearer token from the
// deployment config, and serves the same calls as JSON.
func TestT13_GatewayNeedsBearerToken(t *testing.T) {
	db, err := projection.Open(context.Background(), filepath.Join(shortDir(t), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wf := &stubWorkflows{}
	srv := httptest.NewServer(Gateway(&Service{Workflows: wf, Projection: db, Projects: []string{"p"}, Now: time.Now}, "s3cret"))
	defer srv.Close()
	post := func(path, auth, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	for _, auth := range []string{"", "Bearer wrong", "s3cret", "Basic s3cret"} {
		if code, _ := post("/v1/DeploymentCommand", auth, `{"command":"PAUSE_COMMAND_KIND_PAUSE"}`); code != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, code)
		}
	}
	if len(wf.signals) != 0 {
		t.Fatalf("unauthorised calls signalled %v", wf.signals)
	}
	if code, body := post("/v1/DeploymentCommand", "Bearer s3cret", `{"command":"PAUSE_COMMAND_KIND_PAUSE"}`); code != 200 || !strings.Contains(body, "deployment") {
		t.Fatalf("authorised: %d %s", code, body)
	}
	if want := []string{"deployment pause"}; len(wf.signals) != 1 || wf.signals[0] != want[0] {
		t.Fatalf("signals %v, want %v", wf.signals, want)
	}
	if code, body := post("/v1/Board", "Bearer s3cret", `{}`); code != 200 || !strings.Contains(body, `"deploymentPaused":true`) {
		t.Fatalf("board: %d %s", code, body)
	}
	if code, _ := post("/v1/ProjectCommand", "Bearer s3cret", `{"project":"nope","command":"PAUSE_COMMAND_KIND_PAUSE"}`); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", code)
	}
	if code, _ := post("/v1/Nope", "Bearer s3cret", `{}`); code != http.StatusNotFound {
		t.Fatalf("unknown method: %d", code)
	}
	if empty := Gateway(&Service{}, ""); empty != nil {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/Board", nil)
		req.Header.Set("Authorization", "Bearer ")
		empty.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("gateway with no token: %d, want 401", rec.Code)
		}
	}
}

func TestListenGatewayOnlyLoopback(t *testing.T) {
	if _, err := ListenGateway("0.0.0.0:0"); err == nil {
		t.Fatal("listened on a non-loopback address")
	}
	l, err := ListenGateway("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

// socketPathOfLen returns a path of exactly n bytes under a short temporary
// directory, or skips when the directory is already too long.
func socketPathOfLen(t *testing.T, n int) string {
	t.Helper()
	base := shortDir(t)
	pad := n - len(filepath.Join(base, "x", "api.sock"))
	if pad < 0 {
		t.Skip("temp directory too long")
	}
	p := filepath.Join(base, "x"+strings.Repeat("a", pad), "api.sock")
	if len(p) != n {
		t.Fatalf("path is %d bytes, want %d", len(p), n)
	}
	return p
}

// wantTooLong checks err names the path, its length and the limit (#174 D1).
func wantTooLong(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error for a %d-byte socket path", len(path))
	}
	msg := err.Error()
	for _, want := range []string{path, fmt.Sprintf("is %d bytes", len(path)), fmt.Sprintf("at most %d", sunPathSize()-1), "too long for a Unix socket"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q doesn't contain %q", msg, want)
		}
	}
}

// #174 D1: Listen refuses a socket path of exactly sun_path bytes (one past
// the #139 D1 limit), naming the path, its length and the limit, and creates
// nothing.
func TestI174D1_ListenRefusesASocketPathTooLong(t *testing.T) {
	p := socketPathOfLen(t, sunPathSize())
	_, err := Listen(p)
	wantTooLong(t, err, p)
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Fatalf("socket directory created: %v", err)
	}
}

// #174 D1: Dial refuses the same path up front, instead of a bare "invalid
// argument" on the first call.
func TestI174D1_DialRefusesASocketPathTooLong(t *testing.T) {
	p := socketPathOfLen(t, sunPathSize())
	c, closer, err := Dial(p)
	wantTooLong(t, err, p)
	if c != nil || closer != nil {
		t.Fatal("client returned with an error")
	}
}

// #174 D1: the longest accepted path, one byte short of sun_path, still
// listens and dials.
func TestI174D1_LongestAcceptedSocketPathListensAndDials(t *testing.T) {
	p := socketPathOfLen(t, sunPathSize()-1)
	l, err := Listen(p)
	if err != nil {
		t.Fatalf("listen on a %d-byte path: %v", len(p), err)
	}
	defer l.Close()
	_, closer, err := Dial(p)
	if err != nil {
		t.Fatalf("dial a %d-byte path: %v", len(p), err)
	}
	closer.Close()
}
