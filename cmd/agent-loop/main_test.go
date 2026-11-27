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
	"plan validate <dir>|abandon <plan>",
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

// TestHelpNamesTheReleaseOfUnbuiltCommands: a command that isn't built
// says in the help which release brings it (work plan #105 D4).
func TestHelpNamesTheReleaseOfUnbuiltCommands(t *testing.T) {
	_, out, _ := runCLI("help")
	for _, c := range commands {
		if c.run != nil || c.hidden {
			continue
		}
		if !strings.Contains(c.summary, "(v0.") {
			t.Errorf("%s isn't built, but its summary %q names no release", c.name, c.summary)
		}
		if !strings.Contains(out, c.summary) {
			t.Errorf("help doesn't show %q:\n%s", c.summary, out)
		}
	}
	if !strings.Contains(out, "(v0.3, FR-14 AC4)") {
		t.Errorf("host check isn't marked v0.3:\n%s", out)
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
		// GoReleaser links {{ .Version }}, the tag without v (#124 D3, #141 D5).
		{"tag build (GoReleaser)", "0.1.0", info("(devel)"), "agent-loop 0.1.0\n"},
		{"tag build over a pseudo-version", "0.1.0", info("v0.0.0-20261126034210-5aef6f120701"), "agent-loop 0.1.0\n"},
		{"module version of go install", "dev", info("v0.1.2"), "agent-loop v0.1.2\n"},
		// Go 1.24+ stamps a go build in a git checkout with a pseudo-version
		// (work plan #141, D5).
		{"go build in a checkout", "dev", info("v0.0.0-20261126034210-5aef6f120701+dirty"), "agent-loop v0.0.0-20261126034210-5aef6f120701+dirty\n"},
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

// TestCredentialHelperIsHidden: git runs `agent-loop credential-helper` in
// a local run (TECHNICAL-SPEC §7.3); it isn't an owner command, so the help
// doesn't list it, but it runs.
func TestCredentialHelperIsHidden(t *testing.T) {
	_, out, _ := runCLI("help")
	if strings.Contains(out, "supervise") {
		t.Errorf("help lists the supervisor:\n%s", out)
	}
	if code, _, errOut := runCLI("supervise"); code != 2 || !strings.Contains(errOut, "usage:") {
		t.Errorf("supervise without arguments: code %d, stderr %q", code, errOut)
	}
	if strings.Contains(out, "credential-helper") {
		t.Errorf("help lists the credential helper:\n%s", out)
	}
	if code, _, errOut := runCLI("credential-helper"); code != 2 || !strings.Contains(errOut, "usage:") {
		t.Errorf("credential-helper without arguments: code %d, stderr %q", code, errOut)
	}
}
