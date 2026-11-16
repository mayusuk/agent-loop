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

// fileRules says how to check one kind of file.
type fileRules struct {
	schema *jsonschema.Schema
	// overrides replace the schema's message for a key.
	overrides map[string]string
	// fractions lists the keys whose numbers may have a fraction. Every
	// other float is rejected at its key, because a whole-number float such
	// as 2.0 would pass the schema's integer checks (review R1, R2).
	fractions map[string]bool
}

// decodeFile reads a TOML file, checks it against the rules, and decodes it
// into out. It returns false when the file can't be decoded into out, in
// which case the problems already say why and semantic checks are skipped.
func decodeFile(file string, r fileRules, out any, ps *problems) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		ps.add("", err.Error())
		return false
	}
	return decodeBytes(data, r, out, ps)
}

func decodeBytes(data []byte, r fileRules, out any, ps *problems) bool {
	var generic map[string]any
	if _, err := toml.Decode(string(data), &generic); err != nil {
		ps.add("", "invalid TOML: "+err.Error())
		return false
	}
	before := len(ps.list)
	skip := map[string]bool{}
	checkFloats(generic, "", r, ps, skip)
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
	if err := r.schema.Validate(inst); err != nil {
		ve, ok := err.(*jsonschema.ValidationError)
		if !ok {
			ps.add("", err.Error())
			return false
		}
		schemaProblems(ve, inst, ps, r.overrides, skip)
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

// checkFloats reports every float that isn't allowed where it is, and every
// inf or nan, which JSON can't hold. It replaces each one with null, so the
// schema check still runs, and adds its key to skip so the schema's own
// problem for that value isn't reported twice.
func checkFloats(v any, key string, r fileRules, ps *problems, skip map[string]bool) any {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsInf(x, 0) || math.IsNaN(x):
			ps.add(key, "must be a finite number")
		case r.fractions[key]:
			return x
		case r.overrides[key] != "":
			ps.add(key, r.overrides[key])
		default:
			ps.add(key, "must be a whole number")
		}
		skip[key] = true
		return nil
	case map[string]any:
		for k, e := range x {
			x[k] = checkFloats(e, joinKey(key, k), r, ps, skip)
		}
	case []any:
		for i, e := range x {
			x[i] = checkFloats(e, idx(key, i), r, ps, skip)
		}
	case []map[string]any:
		for i, e := range x {
			checkFloats(e, idx(key, i), r, ps, skip)
		}
	}
	return v
}

// schemaProblems turns the leaves of a validation error into problems keyed by
// the dotted TOML path.
func schemaProblems(ve *jsonschema.ValidationError, inst any, ps *problems, overrides map[string]string, skip map[string]bool) {
	if len(ve.Causes) > 0 {
		for _, c := range ve.Causes {
			schemaProblems(c, inst, ps, overrides, skip)
		}
		return
	}
	key := keyPath(inst, ve.InstanceLocation)
	if skip[key] {
		return
	}
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

// keyPath renders a JSON pointer's tokens as a TOML key path. A token is a
// list index, shown as [n], only when the value it indexes is a list, so a
// table key made of digits stays a key (review R5).
func keyPath(inst any, tokens []string) string {
	var b strings.Builder
	cur := inst
	for _, t := range tokens {
		switch c := cur.(type) {
		case []any:
			b.WriteString("[" + t + "]")
			if i, err := strconv.Atoi(t); err == nil && i >= 0 && i < len(c) {
				cur = c[i]
			} else {
				cur = nil
			}
			continue
		case map[string]any:
			cur = c[t]
		default:
			cur = nil
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(t)
	}
	return b.String()
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
