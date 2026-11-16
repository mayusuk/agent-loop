package profile

import (
	"sort"
	"strings"
)

// Problem is one thing wrong with a config file. Key is the dotted TOML path
// of the value, with list items as [n] (for example identities.owners[0].id).
// Key is empty for a problem with the whole file, such as a TOML syntax error.
type Problem struct {
	File    string
	Key     string
	Message string
}

func (p Problem) String() string {
	if p.Key == "" {
		return p.File + ": " + p.Message
	}
	return p.File + ": " + p.Key + ": " + p.Message
}

// Error lists every problem found in one or more config files (D17).
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// problems collects the problems of one file.
type problems struct {
	file string
	list []Problem
}

func (ps *problems) add(key, msg string) {
	ps.list = append(ps.list, Problem{File: ps.file, Key: key, Message: msg})
}

// err returns nil when there are no problems, or an *Error with the problems
// sorted by key so the output is stable.
func (ps *problems) err() error {
	if len(ps.list) == 0 {
		return nil
	}
	sort.SliceStable(ps.list, func(i, j int) bool { return ps.list[i].Key < ps.list[j].Key })
	return &Error{Problems: ps.list}
}
