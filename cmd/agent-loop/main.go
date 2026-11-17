// Command agent-loop is the CLI, server and worker entry point of agent-loop
// (TECHNICAL-SPEC §3, §11).
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mayusuk/agent-loop/internal/runners"
)

// command is one top-level subcommand. usage is the command with its
// arguments as shown in the help, summary is one line on what it does, and
// run is nil for a command that is planned but not built yet. A hidden
// command is for agent-loop's own use and isn't listed in the help.
type command struct {
	name    string
	usage   string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
	hidden  bool
}

// commands lists every subcommand, in help order. The planned ones are the
// TECHNICAL-SPEC §11 table; later tasks fill in their run functions.
var commands = []command{
	{name: "init", usage: "init", summary: "Set up a project on its tracker and register it (FR-1 AC5)"},
	{name: "project", usage: "project validate|pause|resume <project>", summary: "Validate a project's setup, or pause and resume it", run: runProject},
	{name: "run", usage: "run --dry-run <project>", summary: "Show what one supervisor pass would do, writing nothing (FR-1 AC6)", run: runDryRun},
	{name: "server", usage: "server init|start", summary: "Write the deployment config and start Temporal, or start the server", run: runServer},
	{name: "worker", usage: "worker --capabilities <list>", summary: "Start a worker with the given capabilities", run: runWorker},
	{name: "board", usage: "board [project]", summary: "Show every claimed item, its state and what it waits on (FR-11 AC1)", run: runBoard},
	{name: "item", usage: "item pause|resume|abort <item>", summary: "Pause, resume or abort one item", run: runItem},
	{name: "pause", usage: "pause", summary: "Pause the whole deployment", run: runDeploymentCommand("pause")},
	{name: "resume", usage: "resume", summary: "Resume the whole deployment", run: runDeploymentCommand("resume")},
	{name: "log", usage: "log <item>", summary: "Show an item's stage runs, logs and decisions (FR-11 AC2)", run: runLog},
	{name: "host", usage: "host check", summary: "Check that this host can run stages (FR-14 AC4)"},
	{name: "mint", usage: "mint <project>", summary: "Propose new work from the source of truth (v0.2, FR-9)"},
	{name: "plan", usage: "plan abandon <plan>", summary: "Abandon a materialising plan (v0.2, FR-17 AC9)"},
	{name: "version", usage: "version", summary: "Print the version", run: runVersion},
	{name: "help", usage: "help", summary: "Show this help"},
	// git runs this in a local run's tree to get the run's push token
	// (TECHNICAL-SPEC §7.3).
	// The local runner starts every handler under this, so the processes a
	// handler leaves behind are stopped with it (NFR-7 AC1).
	{name: runners.SuperviseCommand, usage: runners.SuperviseCommand + " <grace-ms> <path> -- <argv...>", summary: "Run a handler and stop everything it started (used by the local runner)", run: runSupervise, hidden: true},
	{name: runners.CredentialHelperCommand, usage: runners.CredentialHelperCommand + " <socket> get|store|erase", summary: "Serve a run's git credentials (used by git)", run: runCredentialHelper, hidden: true},
}

// The help command lists commands, so it is attached here to avoid an
// initialization cycle.
func init() {
	for i := range commands {
		if commands[i].name == "help" {
			commands[i].run = runHelp
		}
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches args (without the program name) and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return 2
	}
	name := args[0]
	if name == "-h" || name == "--help" {
		name = "help"
	}
	for _, c := range commands {
		if c.name != name {
			continue
		}
		if c.run == nil {
			fmt.Fprintf(stderr, "agent-loop %s: not implemented yet\n", name)
			return 1
		}
		return c.run(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "agent-loop: unknown command %q\n\n", name)
	printHelp(stderr)
	return 2
}

// noArgs reports a usage error, and returns false, when a command that takes
// no arguments got some.
func noArgs(name string, args []string, stderr io.Writer) bool {
	if len(args) == 0 {
		return true
	}
	fmt.Fprintf(stderr, "agent-loop %s: unexpected argument %q\n", name, args[0])
	return false
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if !noArgs("version", args, stderr) {
		return 2
	}
	fmt.Fprintf(stdout, "agent-loop %s\n", resolvedVersion())
	return 0
}

func runCredentialHelper(args []string, stdout, stderr io.Writer) int {
	return runners.CredentialHelperMain(args, os.Stdin, stdout, stderr)
}

func runSupervise(args []string, _, stderr io.Writer) int {
	return runners.SuperviseMain(args, stderr)
}

func runHelp(args []string, stdout, stderr io.Writer) int {
	if !noArgs("help", args, stderr) {
		return 2
	}
	printHelp(stdout)
	return 0
}

func printHelp(w io.Writer) {
	width := 0
	for _, c := range commands {
		if !c.hidden {
			width = max(width, len(c.usage))
		}
	}
	var b strings.Builder
	b.WriteString("agent-loop runs a project's development loop with coding agents.\n\n")
	b.WriteString("Usage:\n  agent-loop <command> [arguments]\n\nCommands:\n")
	for _, c := range commands {
		if !c.hidden {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, c.usage, c.summary)
		}
	}
	io.WriteString(w, b.String())
}
