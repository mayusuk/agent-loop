package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TECHNICAL-SPEC §11: every planned command, with its subcommands.
var plannedUsages = []string{
	"board [project]",
	"item pause|resume|abort <item>",
	"project validate|pause|resume <project>",
	"pause",
	"resume",
	"mint <project>",
	"plan abandon <plan>",
	"server init|start",
	"worker --capabilities <list>",
	"init",
	"run --dry-run <project>",
	"host check",
	"log <item>",
}

func TestHelpListsPlannedCommands(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, out, errOut := runCLI(arg)
		if code != 0 || errOut != "" {
			t.Fatalf("%s: code %d, stderr %q", arg, code, errOut)
		}
		for _, u := range plannedUsages {
			if !strings.Contains(out, "  "+u+" ") {
				t.Errorf("%s: help does not list %q:\n%s", arg, u, out)
			}
		}
		if !strings.Contains(out, "  version ") {
			t.Errorf("%s: help does not list version", arg)
		}
	}
}

func TestNoArgsPrintsHelpToStderr(t *testing.T) {
	code, out, errOut := runCLI()
	if code != 2 || out != "" || !strings.Contains(errOut, "Commands:") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, out, errOut := runCLI("frobnicate")
	if code != 2 || out != "" {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	if !strings.HasPrefix(errOut, `agent-loop: unknown command "frobnicate"`) || !strings.Contains(errOut, "Commands:") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestPlannedCommandNotImplemented(t *testing.T) {
	for _, c := range commands {
		if c.run != nil {
			continue
		}
		code, out, errOut := runCLI(c.name, "x")
		want := "agent-loop " + c.name + ": not implemented yet\n"
		if code != 1 || out != "" || errOut != want {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.name, code, out, errOut)
		}
	}
}

func TestVersion(t *testing.T) {
	restore := func(v string, f func() (*debug.BuildInfo, bool)) func() {
		oldV, oldF := version, readBuildInfo
		version, readBuildInfo = v, f
		return func() { version, readBuildInfo = oldV, oldF }
	}
	info := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: v}}, true
		}
	}
	noInfo := func() (*debug.BuildInfo, bool) { return nil, false }

	cases := []struct {
		name, linked string
		build        func() (*debug.BuildInfo, bool)
		want         string
	}{
		{"link-time version wins", "v0.1.0", info("v9.9.9"), "agent-loop v0.1.0\n"},
		{"module version of go install", "dev", info("v0.1.2"), "agent-loop v0.1.2\n"},
		{"local build", "dev", info("(devel)"), "agent-loop dev\n"},
		{"no build info", "dev", noInfo, "agent-loop dev\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer restore(tc.linked, tc.build)()
			code, out, errOut := runCLI("version")
			if code != 0 || out != tc.want || errOut != "" {
				t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}

func TestExtraArgumentsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"version", "extra"},
		{"help", "extra"},
		{"--help", "extra"},
		{"version", "a", "b"},
	} {
		code, out, errOut := runCLI(args...)
		want := "agent-loop " + args[0] + ": unexpected argument \"" + args[1] + "\"\n"
		if args[0] == "--help" {
			want = "agent-loop help: unexpected argument \"" + args[1] + "\"\n"
		}
		if code != 2 || out != "" || errOut != want {
			t.Errorf("%q: code %d, stdout %q, stderr %q, want stderr %q", args, code, out, errOut, want)
		}
	}
}
