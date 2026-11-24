package profile

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	floats := map[string]floatKind{}
	var dups []leaf
	checkFloats(generic, "", r, floats)
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
		var leaves []leaf
		collectLeaves(ve, inst, &leaves)
		dups = schemaProblems(leaves, ps, r.overrides, floats)
	}
	floatFallbacks(floats, r.overrides, ps, before)
	for _, l := range dups {
		leafProblem(l.key, l.ve, ps, r.overrides)
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

// floatKind says why a float was taken out of the schema's view.
type floatKind int

const (
	floatNotAllowed floatKind = iota + 1 // a float where only whole numbers or other types belong
	floatNotFinite                       // inf or nan, which JSON can't hold
)

// checkFloats finds every float that isn't allowed where it is, and every inf
// or nan, and records its key in floats. It replaces each one with a number
// the schema sees as not whole, because a whole-number float such as 2.0
// would pass the schema's integer checks (review R1, R2). It reports nothing:
// the schema's verdict on the placeholder decides the problem (#36).
func checkFloats(v any, key string, r fileRules, floats map[string]floatKind) any {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsInf(x, 0) || math.IsNaN(x):
			floats[key] = floatNotFinite
			return 0.5
		case r.fractions[key]:
			return x
		}
		floats[key] = floatNotAllowed
		return notWhole(x)
	case map[string]any:
		for k, e := range x {
			x[k] = checkFloats(e, joinKey(key, k), r, floats)
		}
	case []any:
		for i, e := range x {
			x[i] = checkFloats(e, idx(key, i), r, floats)
		}
		distinctPlaceholders(x, key, floats)
	case []map[string]any:
		for i, e := range x {
			checkFloats(e, idx(key, i), r, floats)
		}
	}
	return v
}

// notWhole returns x if it has a fraction, and otherwise a nearby number that
// does.
func notWhole(x float64) float64 {
	if x != math.Trunc(x) {
		return x
	}
	if y := x + 0.5; y != math.Trunc(y) {
		return y
	}
	return 0.5 // too large to hold a fraction
}

// distinctPlaceholders gives each float placeholder in list a value no other
// item has, so the schema's uniqueItems check never pairs a placeholder with
// anything and still finds a real duplicate among the other items (review R1
// on #85). Each new value is a not-whole number, so it still fails the
// integer checks.
func distinctPlaceholders(list []any, key string, floats map[string]floatKind) {
	used := map[float64]bool{}
	for i, e := range list {
		if f, ok := e.(float64); ok && floats[idx(key, i)] == 0 {
			used[f] = true
		}
	}
	next := 0.5
	for i, e := range list {
		f, ok := e.(float64)
		if !ok || floats[idx(key, i)] == 0 {
			continue
		}
		for used[f] {
			f = next
			next++
		}
		used[f] = true
		list[i] = f
	}
}

// leaf is one leaf of a validation error, with its TOML key.
type leaf struct {
	key string
	ve  *jsonschema.ValidationError
}

func collectLeaves(ve *jsonschema.ValidationError, inst any, out *[]leaf) {
	if len(ve.Causes) > 0 {
		for _, c := range ve.Causes {
			collectLeaves(c, inst, out)
		}
		return
	}
	*out = append(*out, leaf{keyPath(inst, ve.InstanceLocation), ve})
}

// schemaProblems turns the leaves of a validation error into problems keyed by
// the dotted TOML path. At a float's key, a type error that wants an integer
// becomes one whole-number problem; the leaves at an inf or nan are dropped,
// and floatFallbacks reports it. It returns the duplicate-item leaves instead
// of reporting them, so the caller adds them after floatFallbacks: a
// duplicate at a list must not count as the problem for a float inside it.
func schemaProblems(leaves []leaf, ps *problems, overrides map[string]string, floats map[string]floatKind) (dups []leaf) {
	wantsInt := map[string]bool{}
	for _, l := range leaves {
		if k, ok := l.ve.ErrorKind.(*kind.Type); ok && floats[l.key] == floatNotAllowed && slices.Contains(k.Want, "integer") {
			wantsInt[l.key] = true
		}
	}
	done := map[string]bool{}
	for _, l := range leaves {
		key := l.key
		switch {
		case floats[key] == floatNotFinite:
			continue
		case wantsInt[key]:
			if !done[key] {
				done[key] = true
				ps.add(key, wholeNumber(key, overrides))
			}
			continue
		}
		if k, ok := l.ve.ErrorKind.(*kind.UniqueItems); ok {
			if floats[idx(key, k.Duplicates[0])] == 0 && floats[idx(key, k.Duplicates[1])] == 0 {
				dups = append(dups, l)
			}
			continue // a placeholder can't be compared (R7)
		}
		leafProblem(key, l.ve, ps, overrides)
	}
	return dups
}

func wholeNumber(key string, overrides map[string]string) string {
	if msg := overrides[key]; msg != "" {
		return msg
	}
	return "must be a whole number"
}

// floatFallbacks reports each float the schema said nothing about at its key
// or above it: an inf or nan, or a float at a key the schema doesn't
// constrain. A float under a key that is already a problem, such as an
// unknown key, adds nothing (R7).
func floatFallbacks(floats map[string]floatKind, overrides map[string]string, ps *problems, before int) {
	keys := make([]string, 0, len(floats))
	for k := range floats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if coveredBy(k, ps.list[before:]) {
			continue
		}
		if floats[k] == floatNotFinite {
			ps.add(k, "must be a finite number")
		} else {
			ps.add(k, wholeNumber(k, overrides))
		}
	}
}

// coveredBy says whether a problem is already reported at key or at a key
// that contains it.
func coveredBy(key string, list []Problem) bool {
	for _, p := range list {
		if p.Key == "" || p.Key == key || strings.HasPrefix(key, p.Key+".") || strings.HasPrefix(key, p.Key+"[") {
			return true
		}
	}
	return false
}

// leafProblem turns one leaf of a validation error into a problem.
func leafProblem(key string, ve *jsonschema.ValidationError, ps *problems, overrides map[string]string) {
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
