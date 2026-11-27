package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/contract/tasklist"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

// tasksFile is a plan's task list, in the plan directory (INTEGRATION-SPEC
// §7).
const tasksFile = "tasks.yaml"

// defaultProfile is the profile plan validate reads without --profile or
// --project: the one in the current directory (work plan #166 D1).
const defaultProfile = ".agent-loop.toml"

const planUsage = "plan validate [--profile <path> | --project <name> [--config <path>]] <dir> | plan abandon <plan>"

// runPlan is `agent-loop plan`. Only validate is built; abandon comes with
// the planning lifecycle (FR-17 AC9).
func runPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, planUsage)
	}
	switch args[0] {
	case "validate":
		return runPlanValidate(args[1:], stdout, stderr)
	case "abandon":
		fmt.Fprintln(stderr, "agent-loop plan abandon: not implemented yet (v0.2, FR-17 AC9)")
		return 1
	}
	return usageErr(stderr, planUsage)
}

// runPlanValidate is `agent-loop plan validate <dir>` (plan D11, FR-17
// AC6). It checks the plan directory's task list against every
// INTEGRATION-SPEC §7 rule with the profile's types, and exits 1 listing
// every problem (work plan #166 D1–D5).
func runPlanValidate(args []string, stdout, stderr io.Writer) int {
	const name = "plan validate"
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	profPath := fl.String("profile", "", "the project profile whose types the tasks may use (default "+defaultProfile+" in the current directory)")
	project := fl.String("project", "", "a registered project: use its profile, and check external depends_on refs on its tracker")
	cfg := fl.String("config", "", "with --project: the deployment config (default ~/.config/agent-loop/deployment.toml)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	switch {
	case fl.NArg() != 1:
		fmt.Fprintf(stderr, "agent-loop %s: name one plan directory\n", name)
		return 2
	case *profPath != "" && *project != "":
		fmt.Fprintf(stderr, "agent-loop %s: use --profile or --project, not both\n", name)
		return 2
	case *cfg != "" && *project == "":
		fmt.Fprintf(stderr, "agent-loop %s: --config needs --project\n", name)
		return 2
	}
	dir := filepath.Clean(fl.Arg(0))
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agent-loop %s: "+format+"\n", append([]any{name}, a...)...)
		return 1
	}

	file, err := readTasks(dir)
	if err != nil {
		return fail("%v", err)
	}

	var prof *profile.Profile
	var itemExists func(string) bool
	var lookupErrs []string
	if *project != "" {
		lp, err := loadProject(*cfg, *project)
		if err != nil {
			return fail("%v", err)
		}
		if lp.profErr != nil {
			return fail("%v", lp.profErr)
		}
		prof = lp.prof
		ctx := context.Background()
		ad, probs, err := openAdapters(ctx, lp)
		if err != nil || len(probs) > 0 {
			msgs := make([]string, 0, len(probs)+1)
			for _, p := range probs {
				msgs = append(msgs, p.Message)
			}
			if err != nil {
				msgs = append(msgs, err.Error())
			}
			return fail("can't open project %s's tracker to check external refs: %s", *project, strings.Join(msgs, "; "))
		}
		defer ad.close()
		itemExists = func(ref string) bool {
			_, err := ad.tracker.Item(ctx, itemRef(ref))
			if err == nil {
				return true
			}
			if errors.Is(err, ports.ErrNotFound) {
				return false
			}
			// Not a problem with the plan: reported apart, and the run
			// fails (work plan #166 D2).
			lookupErrs = append(lookupErrs, fmt.Sprintf("can't check item %q: %v", ref, err))
			return true
		}
	} else {
		path := *profPath
		if path == "" {
			path = defaultProfile
		}
		if prof, err = profile.LoadProfile(path, nil); err != nil {
			return fail("%v", err)
		}
	}

	key := filepath.Base(dir)
	list, probs := tasklist.ValidateTasks(key, file, os.DirFS(dir), prof.Types, itemExists)
	for _, p := range probs {
		fmt.Fprintf(stdout, "error   %s: %s\n", p.Rule, p.Error())
	}
	if len(probs) > 0 {
		fmt.Fprintf(stdout, "%s: %d problem(s)\n", dir, len(probs))
	}
	for _, e := range lookupErrs {
		fmt.Fprintf(stderr, "agent-loop %s: %s\n", name, e)
	}
	if len(probs) > 0 || len(lookupErrs) > 0 {
		return 1
	}
	if n := externalRefs(list.Tasks); n > 0 && itemExists == nil {
		fmt.Fprintf(stdout, "note: %d external depends_on ref(s) not checked; pass --project <name> to check them on the tracker\n", n)
	}
	fmt.Fprintf(stdout, "%s: valid, %d task(s)\n", dir, len(list.Tasks))
	return 0
}

// readTasks reads the plan directory's tasks.yaml, which must be a regular
// file there, not a symbolic link (work plan #166 D5).
func readTasks(dir string) ([]byte, error) {
	fsys := os.DirFS(dir)
	fi, err := fs.Lstat(fsys, tasksFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%s has no %s", dir, tasksFile)
	case err != nil:
		return nil, err
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("%s is a symbolic link; it must be a regular file in the plan directory", filepath.Join(dir, tasksFile))
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", filepath.Join(dir, tasksFile))
	}
	return fs.ReadFile(fsys, tasksFile)
}

// itemRef turns an external depends_on ref into the tracker's item key:
// "#17" is item 17 (work plan #166 D3).
func itemRef(ref string) ports.ItemRef {
	return ports.ItemRef(strings.TrimPrefix(ref, "#"))
}

// externalRefs counts the depends_on entries that aren't task keys.
func externalRefs(tasks []contract.Task) int {
	keys := make([]string, len(tasks))
	for i, t := range tasks {
		keys[i] = t.Key
	}
	n := 0
	for _, t := range tasks {
		for _, d := range t.DependsOn {
			if !slices.Contains(keys, d) {
				n++
			}
		}
	}
	return n
}
