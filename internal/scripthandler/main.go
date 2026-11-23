// Command scripthandler is the handler the live PRODUCT-SPEC §11.1 scenario
// runs on a real repository (work plan #25 D9). It meets the stage
// contracts, but the scenario test decides every result: the handler sends
// input.json to the test over a Unix socket, makes and pushes a commit when
// the test asks, and writes the out/ files and result.json the test sends
// back. So the live run takes the same scripted steps as the fake run.
//
// Usage, as a profile stage command: scripthandler <socket>.
//
// The exchange, one JSON value per line each way:
//
//	-> {"input": <input.json>}
//	<- {"commit": bool}            or {"error": "..."}
//	-> {"head": "<sha>", "gh_token": "<GH_TOKEN>"}
//	<- {"files": {"name": "text"}, "result": <result.json>} or {"error": "..."}
//
// "{{GH_TOKEN}}" in a file's text is replaced with the run's GH_TOKEN, as a
// careless handler would echo it, so the scenario can check that the
// worker masks it (PRODUCT-SPEC §8 item 7).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// TokenPlaceholder is replaced with the run's GH_TOKEN in the files.
const TokenPlaceholder = "{{GH_TOKEN}}"

// Hello is the handler's first message.
type Hello struct {
	Input json.RawMessage `json:"input"`
}

// Plan is the test's first answer.
type Plan struct {
	Commit bool   `json:"commit"`
	Error  string `json:"error,omitempty"`
}

// Pushed is the handler's second message.
type Pushed struct {
	Head    string `json:"head"`
	GHToken string `json:"gh_token,omitempty"`
}

// Finish is the test's second answer.
type Finish struct {
	Files  map[string]string `json:"files,omitempty"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  string            `json:"error,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "scripthandler: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string) error {
	if len(args) != 1 {
		return errors.New("usage: scripthandler <socket>")
	}
	inPath, outDir, resPath := getenv("AGENT_LOOP_INPUT"), getenv("AGENT_LOOP_OUT"), getenv("AGENT_LOOP_RESULT")
	if inPath == "" || outDir == "" || resPath == "" {
		return errors.New("AGENT_LOOP_INPUT, AGENT_LOOP_OUT and AGENT_LOOP_RESULT must be set")
	}
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	in, err := contract.ParseInput(raw)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", args[0], 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(bufio.NewReader(conn))

	if err := enc.Encode(Hello{Input: raw}); err != nil {
		return err
	}
	var plan Plan
	if err := dec.Decode(&plan); err != nil {
		return fmt.Errorf("the scenario's plan: %w", err)
	}
	if plan.Error != "" {
		return errors.New(plan.Error)
	}
	head := in.Workspace.Head
	if plan.Commit {
		if head, err = commitAndPush(in); err != nil {
			return err
		}
	}
	if err := enc.Encode(Pushed{Head: head, GHToken: getenv("GH_TOKEN")}); err != nil {
		return err
	}
	var fin Finish
	if err := dec.Decode(&fin); err != nil {
		return fmt.Errorf("the scenario's result: %w", err)
	}
	if fin.Error != "" {
		return errors.New(fin.Error)
	}
	for name, text := range fin.Files {
		if filepath.Base(name) != name {
			return fmt.Errorf("out file %q is not a plain name", name)
		}
		text = strings.ReplaceAll(text, TokenPlaceholder, getenv("GH_TOKEN"))
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(text), 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(resPath, fin.Result, 0o600)
}

// changedFile is the file a commit of the item's runs changes, so items
// don't conflict with each other.
func changedFile(item string) string { return "scenario/item-" + item + ".txt" }

// commitAndPush adds a line to the item's file, commits it and pushes the
// branch with the run's credential helper; it returns the new head.
func commitAndPush(in contract.Input) (string, error) {
	dir := in.Workspace.Path
	if in.Workspace.Branch == "" {
		return "", errors.New("no branch to push: a review run can't commit")
	}
	file := filepath.Join(dir, changedFile(in.Item.Ref))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	_, err = fmt.Fprintf(f, "%s run %s\n", in.Stage, in.RunID)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	git := func(args ...string) (string, error) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("add", changedFile(in.Item.Ref)); err != nil {
		return "", err
	}
	if _, err := git("-c", "user.name=agent-loop scenario", "-c", "user.email=scenario@example.invalid",
		"commit", "-q", "-m", fmt.Sprintf("Scenario %s for item %s", in.Stage, in.Item.Ref)); err != nil {
		return "", err
	}
	if _, err := git("push", "-q", "origin", "HEAD:refs/heads/"+in.Workspace.Branch); err != nil {
		return "", err
	}
	return git("rev-parse", "HEAD")
}
