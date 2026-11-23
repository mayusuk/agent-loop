package secrets

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// EnvPrefix starts the name of every environment variable the env/file
// backend reads.
const EnvPrefix = "AGENT_LOOP_SECRET_"

// EnvVar returns the environment variable that holds the secret name: the
// prefix, then the name in upper case with every character other than a
// letter or digit turned into "_". For example "github-app/coder/app_id" is
// AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID.
func EnvVar(name string) string {
	var b strings.Builder
	b.WriteString(EnvPrefix)
	for _, c := range strings.ToUpper(name) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// EnvFile is the env/file backend. A secret is read from the environment
// variable EnvVar(name), or else from the same key in the file at Path.
//
// The file has one KEY=value per line. Blank lines and lines starting with
// "#" are skipped, and a line may start with "export ". A value in double
// quotes may use the escapes \n, \" and \\, so a PEM key fits on one line.
// The file must not be readable or writable by group or others. A missing
// file is not an error: the environment alone is used.
type EnvFile struct {
	Path string
	// LookupEnv reads the environment. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Get implements Backend.
func (e *EnvFile) Get(_ context.Context, name string) (string, error) {
	key := EnvVar(name)
	lookup := e.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if v, ok := lookup(key); ok {
		return v, nil
	}
	where := "the environment (" + key + ")"
	if e.Path == "" {
		return "", notFound(name, where)
	}
	vals, err := readEnvFile(e.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", notFound(name, where+" or "+e.Path+" (missing)")
	}
	if err != nil {
		return "", err
	}
	if v, ok := vals[key]; ok {
		return v, nil
	}
	return "", notFound(name, where+" or "+e.Path)
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// readEnvFile parses an env file. Errors name the file and line, never a
// value.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %04o, which lets other users read it; run chmod 600 %s: %w", path, st.Mode().Perm(), path, ports.ErrPermission)
	}
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envKey.MatchString(k) {
			return nil, fmt.Errorf("%s:%d: not a KEY=value line", path, n)
		}
		if _, dup := vals[k]; dup {
			return nil, fmt.Errorf("%s:%d: %s is set twice", path, n, k)
		}
		v, err := envValue(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %s: %v", path, n, k, err)
		}
		vals[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return vals, nil
}

func envValue(v string) (string, error) {
	if !strings.HasPrefix(v, `"`) {
		return v, nil
	}
	if len(v) < 2 || !strings.HasSuffix(v, `"`) {
		return "", errors.New("unterminated quoted value")
	}
	in := v[1 : len(v)-1]
	var b bytes.Buffer
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '\\' && i+1 < len(in):
			i++
			switch in[i] {
			case 'n':
				b.WriteByte('\n')
			case '"', '\\':
				b.WriteByte(in[i])
			default:
				return "", fmt.Errorf(`unknown escape \%c in quoted value`, in[i])
			}
		case c == '\\':
			return "", errors.New(`quoted value ends with \`)
		case c == '"':
			return "", errors.New(`unescaped " in quoted value`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// Set stores the secret in the file at Path, replacing its line or adding
// one, and leaves every other line as it was. The file is written with
// mode 0600, and its directory is created with mode 0700 if missing. The
// environment is not changed, so a variable that is set still wins.
func (e *EnvFile) Set(_ context.Context, name, value string) error {
	if _, err := ParseRef(RefScheme + name); err != nil {
		return fmt.Errorf("envfile: %q is not a secret name", name)
	}
	if e.Path == "" {
		return errors.New("envfile: no file to write: set secrets.path in the deployment config")
	}
	var lines []string
	if _, err := readEnvFile(e.Path); err == nil {
		data, err := os.ReadFile(e.Path)
		if err != nil {
			return err
		}
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	key := EnvVar(name)
	line := key + "=" + quoteEnv(value)
	found := false
	for i, l := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(l), "export "), "=")
		if ok && strings.TrimSpace(k) == key {
			lines[i], found = line, true
		}
	}
	if !found {
		lines = append(lines, line)
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.Path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), e.Path)
}

// quoteEnv quotes a value so readEnvFile reads it back unchanged.
func quoteEnv(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}
