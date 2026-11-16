package profile

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

//go:embed schema/*.schema.json
var schemaFS embed.FS

var (
	profileSchema    = mustCompile("profile.schema.json")
	deploymentSchema = mustCompile("deployment.schema.json")
	printer          = message.NewPrinter(language.English)
)

func mustCompile(name string) *jsonschema.Schema {
	b, err := schemaFS.ReadFile("schema/" + name)
	if err != nil {
		panic(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		panic(fmt.Sprintf("profile: schema %s: %v", name, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, doc); err != nil {
		panic(err)
	}
	return c.MustCompile(name)
}

// ProfileSchema and DeploymentSchema return the embedded JSON Schemas, for
// tools and documentation.
func ProfileSchema() []byte    { return mustRead("profile.schema.json") }
func DeploymentSchema() []byte { return mustRead("deployment.schema.json") }

func mustRead(name string) []byte {
	b, err := schemaFS.ReadFile("schema/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// decodeFile reads a TOML file, checks it against the schema, and decodes it
// into out. It returns false when the file can't be decoded into out, in
// which case the problems already say why and semantic checks are skipped.
func decodeFile(file string, sch *jsonschema.Schema, out any, ps *problems, overrides map[string]string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		ps.add("", err.Error())
		return false
	}
	return decodeBytes(data, sch, out, ps, overrides)
}

func decodeBytes(data []byte, sch *jsonschema.Schema, out any, ps *problems, overrides map[string]string) bool {
	var generic map[string]any
	if _, err := toml.Decode(string(data), &generic); err != nil {
		ps.add("", "invalid TOML: "+err.Error())
		return false
	}
	if bad := nonFinite(generic, ""); len(bad) > 0 {
		for _, k := range bad {
			ps.add(k, "must be a finite number")
		}
		return false
	}
	js, err := json.Marshal(generic)
	if err != nil {
		ps.add("", "can't convert to JSON: "+err.Error())
		return false
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		ps.add("", "can't convert to JSON: "+err.Error())
		return false
	}
	before := len(ps.list)
	if err := sch.Validate(inst); err != nil {
		ve, ok := err.(*jsonschema.ValidationError)
		if !ok {
			ps.add("", err.Error())
			return false
		}
		schemaProblems(ve, ps, overrides)
	}
	if len(ps.list) > before {
		return false
	}
	if _, err := toml.Decode(string(data), out); err != nil {
		ps.add("", err.Error())
		return false
	}
	return true
}

// schemaProblems turns the leaves of a validation error into problems keyed by
// the dotted TOML path.
func schemaProblems(ve *jsonschema.ValidationError, ps *problems, overrides map[string]string) {
	if len(ve.Causes) > 0 {
		for _, c := range ve.Causes {
			schemaProblems(c, ps, overrides)
		}
		return
	}
	key := keyPath(ve.InstanceLocation)
	switch k := ve.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		for _, p := range k.Properties {
			ps.add(joinKey(key, p), "unknown key")
		}
		return
	case *kind.Required:
		for _, p := range k.Missing {
			ps.add(joinKey(key, p), "required key is missing")
		}
		return
	}
	if msg, ok := overrides[key]; ok {
		ps.add(key, msg)
		return
	}
	switch k := ve.ErrorKind.(type) {
	case *kind.Pattern:
		if hint, ok := patternHints[k.Want]; ok {
			ps.add(key, fmt.Sprintf("%q %s", k.Got, hint))
			return
		}
	case *kind.MinProperties, *kind.MaxProperties:
		if strings.HasPrefix(key, "markers.") {
			ps.add(key, "set exactly one of label or status")
			return
		}
	}
	ps.add(key, ve.ErrorKind.LocalizedString(printer))
}

// patternHints say in words what a schema pattern means.
var patternHints = map[string]string{
	`^secret://\S+$`: `is not a secret reference such as "secret://github-app/coder"`,
	`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`:        `is not a duration such as "10m" or "2h"`,
	`^[a-z][a-z0-9_-]*$`:                               "must start with a lower-case letter and use only a-z, 0-9, _ and -",
	`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`:                `is not a repository such as "owner/name"`,
	`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`:                  "may use only A-Z, a-z, 0-9, ., _, / and -, and can't start with / or -",
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`:               `is not a contract version such as "1.0"`,
	`^[^/~]`:                                           "must be a path relative to the repository",
	`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$`: `is not an address such as "localhost:7233"`,
	`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`: `is not a lower-case host name, optionally starting with "*."`,
}

// keyPath renders a JSON pointer's tokens as a TOML key path: list indexes
// become [n].
func keyPath(tokens []string) string {
	var b strings.Builder
	for i, t := range tokens {
		if _, err := strconv.Atoi(t); err == nil && i > 0 {
			b.WriteString("[" + t + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(t)
	}
	return b.String()
}

// nonFinite returns the keys of inf and nan values, which JSON can't hold.
func nonFinite(v any, key string) []string {
	var out []string
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			out = append(out, key)
		}
	case map[string]any:
		for k, e := range x {
			out = append(out, nonFinite(e, joinKey(key, k))...)
		}
	case []any:
		for i, e := range x {
			out = append(out, nonFinite(e, idx(key, i))...)
		}
	case []map[string]any:
		for i, e := range x {
			out = append(out, nonFinite(e, idx(key, i))...)
		}
	}
	return out
}

func joinKey(base, k string) string {
	if base == "" {
		return k
	}
	return base + "." + k
}

func idx(base string, i int) string { return base + "[" + strconv.Itoa(i) + "]" }

// resolvePath expands a leading ~/ to the home directory, and makes a
// relative path relative to dir (D15).
func resolvePath(p, dir string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home + p[1:], nil
	}
	if !filepath.IsAbs(p) && dir != "" {
		return filepath.Join(dir, p), nil
	}
	return p, nil
}
