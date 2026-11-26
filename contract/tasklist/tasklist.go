// Package tasklist validates a plan's task list, plans/<plan-key>/tasks.yaml
// (INTEGRATION-SPEC §7). It is a subpackage so that package contract, which
// the core and the handlers import, keeps to the standard library (work plan
// #154 D1).
package tasklist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

// MaxBody is the largest body file a task may name (INTEGRATION-SPEC §7).
const MaxBody = contract.MaxTextFile

// Rule names the INTEGRATION-SPEC §7 rule a problem breaks.
type Rule string

// The task list rules.
const (
	// RuleSchema: the file isn't one YAML document that matches
	// tasks.schema.json.
	RuleSchema Rule = "schema"
	// RulePlan: plan isn't the plan directory's name.
	RulePlan Rule = "plan"
	// RuleDuplicateKey: two tasks have the same key.
	RuleDuplicateKey Rule = "duplicate_key"
	// RuleType: a task's type isn't configured in the profile.
	RuleType Rule = "type"
	// RuleBodyMissing: a body file doesn't exist or isn't a regular file.
	RuleBodyMissing Rule = "body_missing"
	// RuleBodyTooLarge: a body file is over MaxBody bytes.
	RuleBodyTooLarge Rule = "body_too_large"
	// RuleUnknownKey: a depends_on entry names a task key the plan doesn't
	// have.
	RuleUnknownKey Rule = "unknown_key"
	// RuleMissingItem: a depends_on entry names an item that doesn't exist.
	RuleMissingItem Rule = "missing_item"
	// RuleCycle: the dependencies have a cycle.
	RuleCycle Rule = "cycle"
)

// Problem is one reason a task list is invalid.
type Problem struct {
	Rule Rule
	// Task is the key of the task the problem is about, or "".
	Task string
	// Path is the location in the file, such as "tasks[1].depends_on[0]",
	// or "" for the whole file.
	Path    string
	Message string
}

// Error returns the problem as one line.
func (p Problem) Error() string {
	if p.Path == "" {
		return p.Message
	}
	return p.Path + ": " + p.Message
}

// keyRe is the task and plan key pattern (INTEGRATION-SPEC §7).
var keyRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

var (
	schema  = mustCompile()
	printer = message.NewPrinter(language.English)
)

func mustCompile() *jsonschema.Schema {
	b, err := contract.Schema(contract.TasksSchema)
	if err != nil {
		panic(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		panic(fmt.Sprintf("tasklist: %s: %v", contract.TasksSchema, err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(contract.TasksSchema, doc); err != nil {
		panic(err)
	}
	return c.MustCompile(contract.TasksSchema)
}

// ValidateTasks checks a plan's tasks.yaml against every rule in
// INTEGRATION-SPEC §7 and returns every problem it finds (FR-17 AC6).
//
//   - plan is the plan directory's name, which the file's plan must equal.
//   - file is the tasks.yaml contents.
//   - bodies holds the plan directory: task bodies are looked up in it by
//     their path relative to the directory. It must not be nil.
//   - types are the item types the profile configures.
//   - itemExists reports whether an external item ref in depends_on (one
//     that isn't a task key, such as "#17") names an item that exists. It
//     gets the ref as written. When it is nil, external refs aren't checked
//     (work plan #154 D4, D5).
//
// Problems come in a fixed order: problems with the YAML itself, or schema
// problems sorted by location, then the plan name, then each task's
// problems in file order, then cycles. The task list is returned only when
// there are no problems.
func ValidateTasks(plan string, file []byte, bodies fs.FS, types []string, itemExists func(string) bool) (contract.TaskList, []Problem) {
	value, probs := parseYAML(file)
	if len(probs) > 0 {
		return contract.TaskList{}, probs
	}
	probs = append(probs, schemaProblems(value)...)
	probs = append(probs, ruleProblems(plan, value, bodies, types, itemExists)...)
	if len(probs) > 0 {
		return contract.TaskList{}, probs
	}
	return toTaskList(value), nil
}

// parseYAML reads file as one YAML document and converts it to the values
// encoding/json uses (work plan #154 D3).
func parseYAML(file []byte) (any, []Problem) {
	dec := yaml.NewDecoder(bytes.NewReader(file))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []Problem{{Rule: RuleSchema, Message: "the file is empty"}}
		}
		return nil, []Problem{{Rule: RuleSchema, Message: "not valid YAML: " + err.Error()}}
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, []Problem{{Rule: RuleSchema, Message: "the file holds more than one YAML document"}}
	}
	var c converter
	v := c.value(&doc, "")
	return v, c.probs
}

type converter struct{ probs []Problem }

func (c *converter) add(loc, msg string) {
	c.probs = append(c.probs, Problem{Rule: RuleSchema, Path: loc, Message: msg})
}

func (c *converter) value(n *yaml.Node, loc string) any {
	if n.Anchor != "" {
		c.add(loc, "YAML anchors and aliases are not allowed")
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return c.value(n.Content[0], loc)
	case yaml.AliasNode:
		c.add(loc, "YAML anchors and aliases are not allowed")
		return nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, e := range n.Content {
			out = append(out, c.value(e, loc+"["+strconv.Itoa(i)+"]"))
		}
		return out
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				if k.ShortTag() == "!!merge" {
					c.add(loc, "YAML merge keys (<<) are not allowed")
				} else {
					c.add(loc, fmt.Sprintf("mapping key %q is not a string", k.Value))
				}
				continue
			}
			if _, dup := out[k.Value]; dup {
				c.add(joinLoc(loc, k.Value), "key is repeated")
				continue
			}
			out[k.Value] = c.value(v, joinLoc(loc, k.Value))
		}
		return out
	case yaml.ScalarNode:
		return c.scalar(n, loc)
	}
	c.add(loc, "unsupported YAML node")
	return nil
}

func (c *converter) scalar(n *yaml.Node, loc string) any {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err == nil {
			return b
		}
	case "!!int":
		var i int64
		if err := n.Decode(&i); err == nil {
			return i
		}
		var f float64
		if err := n.Decode(&f); err == nil && !math.IsInf(f, 0) {
			return f
		}
		c.add(loc, fmt.Sprintf("number %s is out of range", n.Value))
		return nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err == nil {
			if math.IsInf(f, 0) || math.IsNaN(f) {
				c.add(loc, fmt.Sprintf("number %s is not finite", n.Value))
				return nil
			}
			return f
		}
	}
	// Strings, timestamps and other tagged scalars keep their text.
	return n.Value
}

func joinLoc(loc, key string) string {
	if loc == "" {
		return key
	}
	return loc + "." + key
}

// schemaProblems validates value against tasks.schema.json and returns one
// problem per leaf error, sorted by location.
func schemaProblems(value any) []Problem {
	js, err := json.Marshal(value)
	if err != nil {
		return []Problem{{Rule: RuleSchema, Message: "can't convert to JSON: " + err.Error()}}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return []Problem{{Rule: RuleSchema, Message: "can't convert to JSON: " + err.Error()}}
	}
	err = schema.Validate(inst)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Problem{{Rule: RuleSchema, Message: err.Error()}}
	}
	var probs []Problem
	var walk func(*jsonschema.ValidationError)
	walk = func(ve *jsonschema.ValidationError) {
		if len(ve.Causes) > 0 {
			for _, c := range ve.Causes {
				walk(c)
			}
			return
		}
		loc := location(inst, ve.InstanceLocation)
		switch k := ve.ErrorKind.(type) {
		case *kind.AdditionalProperties:
			for _, p := range k.Properties {
				probs = append(probs, Problem{Rule: RuleSchema, Path: joinLoc(loc, p), Message: "unknown key"})
			}
		case *kind.Required:
			for _, p := range k.Missing {
				probs = append(probs, Problem{Rule: RuleSchema, Path: joinLoc(loc, p), Message: "required key is missing"})
			}
		default:
			probs = append(probs, Problem{Rule: RuleSchema, Path: loc, Message: ve.ErrorKind.LocalizedString(printer)})
		}
	}
	walk(ve)
	sort.SliceStable(probs, func(i, j int) bool {
		if probs[i].Path != probs[j].Path {
			return probs[i].Path < probs[j].Path
		}
		return probs[i].Message < probs[j].Message
	})
	return slices.CompactFunc(probs, func(a, b Problem) bool { return a == b })
}

// location turns a JSON pointer's tokens into "tasks[1].key", using inst to
// tell array indexes from object keys.
func location(inst any, tokens []string) string {
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
		case map[string]any:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(t)
			cur = c[t]
		default:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(t)
			cur = nil
		}
	}
	return b.String()
}

// task is one task's well-typed fields. A field of the wrong type is left
// empty, and the schema problem reports it.
type task struct {
	index     int
	key       string
	typ       string
	hasType   bool
	body      string
	dependsOn []string // entries that are strings
	loc       string
}

func str(m map[string]any, k string) (string, bool) {
	s, ok := m[k].(string)
	return s, ok
}

func tasksOf(value any) []task {
	root, _ := value.(map[string]any)
	list, _ := root["tasks"].([]any)
	var out []task
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		t := task{index: i, loc: "tasks[" + strconv.Itoa(i) + "]"}
		t.key, _ = str(m, "key")
		t.typ, t.hasType = str(m, "type")
		t.body, _ = str(m, "body")
		if deps, ok := m["depends_on"].([]any); ok {
			for _, d := range deps {
				if s, ok := d.(string); ok && s != "" {
					t.dependsOn = append(t.dependsOn, s)
				}
			}
		}
		out = append(out, t)
	}
	return out
}

// ruleProblems makes the §7 checks that need the plan directory, the
// profile or the whole list, on every field that is well typed.
func ruleProblems(plan string, value any, bodies fs.FS, types []string, itemExists func(string) bool) []Problem {
	var probs []Problem
	root, _ := value.(map[string]any)
	if p, ok := str(root, "plan"); ok && p != plan {
		probs = append(probs, Problem{Rule: RulePlan, Path: "plan", Message: fmt.Sprintf("plan %q is not the plan directory's name %q", p, plan)})
	}
	tasks := tasksOf(value)
	first := map[string]int{} // key -> index into tasks of its first task
	for i, t := range tasks {
		if t.key == "" {
			continue
		}
		if j, dup := first[t.key]; dup {
			probs = append(probs, Problem{Rule: RuleDuplicateKey, Task: t.key, Path: t.loc + ".key",
				Message: fmt.Sprintf("key %q is also the key of %s", t.key, tasks[j].loc)})
			continue
		}
		first[t.key] = i
	}
	for _, t := range tasks {
		if t.hasType && t.typ != "" && !slices.Contains(types, t.typ) {
			probs = append(probs, Problem{Rule: RuleType, Task: t.key, Path: t.loc + ".type",
				Message: fmt.Sprintf("type %q is not configured (configured: %s)", t.typ, configured(types))})
		}
		if p, ok := bodyProblem(bodies, t); ok {
			probs = append(probs, p)
		}
		for j, d := range t.dependsOn {
			loc := fmt.Sprintf("%s.depends_on[%d]", t.loc, j)
			if _, ok := first[d]; ok {
				continue
			}
			if keyRe.MatchString(d) {
				probs = append(probs, Problem{Rule: RuleUnknownKey, Task: t.key, Path: loc,
					Message: fmt.Sprintf("no task in this plan has key %q", d)})
				continue
			}
			if itemExists != nil && !itemExists(d) {
				probs = append(probs, Problem{Rule: RuleMissingItem, Task: t.key, Path: loc,
					Message: fmt.Sprintf("item %q does not exist", d)})
			}
		}
	}
	return append(probs, cycles(tasks, first)...)
}

// bodyProblem checks a task's body file (work plan #154 D7). Bodies that the
// schema rejects are not looked up.
func bodyProblem(bodies fs.FS, t task) (Problem, bool) {
	if t.body == "" || strings.HasPrefix(t.body, "/") || slices.Contains(strings.Split(t.body, "/"), "..") {
		return Problem{}, false
	}
	loc := t.loc + ".body"
	name := path.Clean(t.body)
	missing := func(why string) (Problem, bool) {
		return Problem{Rule: RuleBodyMissing, Task: t.key, Path: loc, Message: fmt.Sprintf("body file %q %s", t.body, why)}, true
	}
	if !fs.ValidPath(name) || name == "." {
		return missing("is not a path in the plan directory")
	}
	// fs.Lstat doesn't follow a final symlink when bodies implements
	// fs.ReadLinkFS (os.DirFS does). Checking every element also catches a
	// symlinked directory, so a body can't name a file outside the plan
	// directory (work plan #154 D10).
	var fi fs.FileInfo
	elems := strings.Split(name, "/")
	for i := range elems {
		var err error
		fi, err = fs.Lstat(bodies, strings.Join(elems[:i+1], "/"))
		if err != nil {
			return missing("does not exist")
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return missing("is not a regular file: it goes through a symbolic link")
		}
	}
	if !fi.Mode().IsRegular() {
		return missing("is not a regular file")
	}
	tooLarge := Problem{Rule: RuleBodyTooLarge, Task: t.key, Path: loc,
		Message: fmt.Sprintf("body file %q is larger than 64 KiB", t.body)}
	if fi.Size() > MaxBody {
		return tooLarge, true
	}
	f, err := bodies.Open(name)
	if err != nil {
		return missing("can't be read")
	}
	defer f.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(f, MaxBody+1))
	if err != nil {
		return missing("can't be read")
	}
	if n > MaxBody {
		return tooLarge, true
	}
	return Problem{}, false
}

// cycles returns one problem per dependency cycle: each strongly connected
// component with more than one task, or a task that depends on itself
// (work plan #154 D6).
func cycles(tasks []task, first map[string]int) []Problem {
	n := len(tasks)
	edges := make([][]int, n)
	self := make([]bool, n)
	for _, t := range tasks {
		from, ok := first[t.key]
		if t.key == "" || !ok {
			continue
		}
		for _, d := range t.dependsOn {
			to, ok := first[d]
			if !ok {
				continue
			}
			if to == from {
				self[from] = true
			}
			edges[from] = append(edges[from], to)
		}
	}
	// Tarjan's algorithm.
	index := make([]int, n)
	low := make([]int, n)
	on := make([]bool, n)
	for i := range index {
		index[i] = -1
	}
	var stack []int
	var comps [][]int
	next := 0
	var visit func(v int)
	visit = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		on[v] = true
		for _, w := range edges[v] {
			if index[w] < 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []int
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			if len(comp) > 1 || self[v] {
				sort.Ints(comp)
				comps = append(comps, comp)
			}
		}
	}
	for v := range tasks {
		if index[v] < 0 {
			visit(v)
		}
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i][0] < comps[j][0] })
	var probs []Problem
	for _, comp := range comps {
		t := tasks[comp[0]]
		msg := fmt.Sprintf("task %q depends on itself", t.key)
		if len(comp) > 1 {
			keys := make([]string, len(comp))
			for i, c := range comp {
				keys[i] = strconv.Quote(tasks[c].key)
			}
			msg = fmt.Sprintf("tasks %s depend on each other in a cycle", list(keys))
		}
		probs = append(probs, Problem{Rule: RuleCycle, Task: t.key, Path: t.loc + ".depends_on", Message: msg})
	}
	return probs
}

func configured(types []string) string {
	if len(types) == 0 {
		return "none"
	}
	return strings.Join(types, ", ")
}

// list joins items as "a, b and c".
func list(items []string) string {
	switch len(items) {
	case 0:
		return "none"
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// toTaskList converts a value that passed every check. Body paths are
// cleaned, as they were looked up.
func toTaskList(value any) contract.TaskList {
	root := value.(map[string]any)
	tl := contract.TaskList{Version: 1, Plan: root["plan"].(string)}
	tl.Tasks = []contract.Task{}
	for _, e := range root["tasks"].([]any) {
		m := e.(map[string]any)
		t := contract.Task{Key: m["key"].(string), Type: m["type"].(string), Title: m["title"].(string), Body: path.Clean(m["body"].(string))}
		if deps, ok := m["depends_on"].([]any); ok {
			for _, d := range deps {
				t.DependsOn = append(t.DependsOn, d.(string))
			}
		}
		tl.Tasks = append(tl.Tasks, t)
	}
	return tl
}
