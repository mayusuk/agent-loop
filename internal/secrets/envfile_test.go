package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

func noEnv(string) (string, bool) { return "", false }

func writeFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseRef(t *testing.T) {
	for ref, want := range map[string]string{
		"secret://github-app/coder": "github-app/coder",
		"secret://gateway":          "gateway",
		"secret://a.b/c_d/e-f":      "a.b/c_d/e-f",
	} {
		got, err := ParseRef(ref)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	for _, ref := range []string{"", "github-app/coder", "secret://", "secret:///x", "secret://x/", "secret://a//b", "secret://a b", "secrets://x"} {
		if _, err := ParseRef(ref); err == nil {
			t.Errorf("ParseRef(%q) succeeded", ref)
		}
	}
}

func TestEnvVar(t *testing.T) {
	for name, want := range map[string]string{
		"github-app/coder/app_id":     "AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID",
		"gateway":                     "AGENT_LOOP_SECRET_GATEWAY",
		"github-app/loop/private_key": "AGENT_LOOP_SECRET_GITHUB_APP_LOOP_PRIVATE_KEY",
		"a.b/c9":                      "AGENT_LOOP_SECRET_A_B_C9",
	} {
		if got := EnvVar(name); got != want {
			t.Errorf("EnvVar(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEnvFileEnvironmentWinsOverFile(t *testing.T) {
	p := writeFile(t, "AGENT_LOOP_SECRET_A=from-file\nAGENT_LOOP_SECRET_B=file-only\n", 0o600)
	e := &EnvFile{Path: p, LookupEnv: func(k string) (string, bool) {
		if k == "AGENT_LOOP_SECRET_A" {
			return "from-env", true
		}
		return "", false
	}}
	ctx := context.Background()
	if v, err := e.Get(ctx, "a"); err != nil || v != "from-env" {
		t.Errorf("a = %q, %v; want from-env", v, err)
	}
	if v, err := e.Get(ctx, "b"); err != nil || v != "file-only" {
		t.Errorf("b = %q, %v; want file-only", v, err)
	}
}

func TestEnvFileUsesProcessEnvironmentByDefault(t *testing.T) {
	t.Setenv("AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID", "123")
	v, err := (&EnvFile{}).Get(context.Background(), "github-app/coder/app_id")
	if err != nil || v != "123" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestEnvFileFormat(t *testing.T) {
	p := writeFile(t, strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"AGENT_LOOP_SECRET_PLAIN = plain value ",
		"export AGENT_LOOP_SECRET_EXPORTED=x",
		`AGENT_LOOP_SECRET_PEM="-----BEGIN KEY-----\nAAAA\n-----END KEY-----\n"`,
		`AGENT_LOOP_SECRET_ESC="a\"b\\c"`,
		"AGENT_LOOP_SECRET_EMPTY=",
		"AGENT_LOOP_SECRET_EQ=a=b",
	}, "\n"), 0o600)
	e := &EnvFile{Path: p, LookupEnv: noEnv}
	for name, want := range map[string]string{
		"plain":    "plain value",
		"exported": "x",
		"pem":      "-----BEGIN KEY-----\nAAAA\n-----END KEY-----\n",
		"esc":      `a"b\c`,
		"empty":    "",
		"eq":       "a=b",
	} {
		v, err := e.Get(context.Background(), name)
		if err != nil || v != want {
			t.Errorf("%s = %q, %v; want %q", name, v, err, want)
		}
	}
}

func TestEnvFileNotFound(t *testing.T) {
	p := writeFile(t, "AGENT_LOOP_SECRET_OTHER=x\n", 0o600)
	for _, e := range []*EnvFile{
		{LookupEnv: noEnv},
		{Path: p, LookupEnv: noEnv},
		{Path: filepath.Join(t.TempDir(), "missing.env"), LookupEnv: noEnv},
	} {
		_, err := e.Get(context.Background(), "github-app/coder/app_id")
		if !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("path %q: err = %v, want ErrNotFound", e.Path, err)
			continue
		}
		if !strings.Contains(err.Error(), "AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID") {
			t.Errorf("error %q doesn't name the variable", err)
		}
	}
}

func TestEnvFileRefusesFileOthersCanRead(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o666} {
		p := writeFile(t, "AGENT_LOOP_SECRET_A=x\n", mode)
		_, err := (&EnvFile{Path: p, LookupEnv: noEnv}).Get(context.Background(), "a")
		if !errors.Is(err, ports.ErrPermission) {
			t.Errorf("mode %04o: err = %v, want ErrPermission", mode, err)
		}
	}
	p := writeFile(t, "AGENT_LOOP_SECRET_A=x\n", 0o400)
	if _, err := (&EnvFile{Path: p, LookupEnv: noEnv}).Get(context.Background(), "a"); err != nil {
		t.Errorf("mode 0400: %v", err)
	}
}

func TestEnvFileMalformedLinesNameTheLineNotTheValue(t *testing.T) {
	for content, want := range map[string]string{
		"AGENT_LOOP_SECRET_A=ok\nnot a line s3cr3t\n": ":2: not a KEY=value line",
		"1BAD=s3cr3t\n": ":1: not a KEY=value line",
		"AGENT_LOOP_SECRET_A=s3cr3t\nAGENT_LOOP_SECRET_A=s3cr3t\n": ":2: AGENT_LOOP_SECRET_A is set twice",
		`AGENT_LOOP_SECRET_A="s3cr3t` + "\n":                       ":1: AGENT_LOOP_SECRET_A: unterminated",
		"AGENT_LOOP_SECRET_A=\"s3\tcr3t\"\n":                       "", // a tab is fine
		`AGENT_LOOP_SECRET_A="s3\qcr3t"` + "\n":                    `unknown escape \q`,
		`AGENT_LOOP_SECRET_A="s3"cr3t"` + "\n":                     `unescaped "`,
		`AGENT_LOOP_SECRET_A="s3cr3t\"` + "\n":                     `ends with \`,
	} {
		p := writeFile(t, content, 0o600)
		_, err := (&EnvFile{Path: p, LookupEnv: noEnv}).Get(context.Background(), "a")
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", content, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to contain %q", content, err, want)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "cr3t") {
			t.Errorf("%q: error %q contains the value", content, err)
		}
	}
}

// TestFR1AC5_EnvFileSetRoundTrips: init stores a PEM key in the env file
// and the backend reads it back; other lines stay, and the file is 0600.
func TestFR1AC5_EnvFileSetRoundTrips(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "dir", "secrets.env")
	e := &EnvFile{Path: p, LookupEnv: noEnv}
	pemKey := "-----BEGIN KEY-----\nab\"c\\d\n-----END KEY-----\n"
	for name, v := range map[string]string{"github-app/w/coder/private_key": pemKey, "github-app/w/coder/app_id": "12"} {
		if err := e.Set(ctx, name, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Set(ctx, "github-app/w/coder/app_id", "13"); err != nil {
		t.Fatal(err)
	}
	if v, err := e.Get(ctx, "github-app/w/coder/private_key"); err != nil || v != pemKey {
		t.Errorf("private_key = %q, %v", v, err)
	}
	if v, err := e.Get(ctx, "github-app/w/coder/app_id"); err != nil || v != "13" {
		t.Errorf("app_id = %q, %v", v, err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st.Mode().Perm(), err)
	}
	data, _ := os.ReadFile(p)
	if strings.Count(string(data), "APP_ID=") != 1 {
		t.Errorf("app_id written twice:\n%s", data)
	}
	if err := (&EnvFile{LookupEnv: noEnv}).Set(ctx, "a", "b"); err == nil {
		t.Error("Set without a path succeeded")
	}
	if err := e.Set(ctx, "bad name", "b"); err == nil {
		t.Error("Set with a bad name succeeded")
	}
	loose := writeFile(t, "AGENT_LOOP_SECRET_X=1\n", 0o644)
	if err := (&EnvFile{Path: loose, LookupEnv: noEnv}).Set(ctx, "y", "2"); !errors.Is(err, ports.ErrPermission) {
		t.Errorf("Set on a 0644 file: %v", err)
	}
}
