package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc/status"

	"github.com/mayusuk/agent-loop/internal/api"
	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
	"github.com/mayusuk/agent-loop/internal/profile"
)

// callTimeout bounds one API call from the CLI.
const callTimeout = 30 * time.Second

// newFlags returns a command's flag set with the --config flag every
// server command takes.
func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("agent-loop "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", "", "the deployment config (default ~/.config/agent-loop/deployment.toml)")
	return fs, cfg
}

// loadDeployment loads the deployment config at path, or at the default
// path when path is empty. A missing default config is the defaults, so
// the CLI finds the default socket (work plan #22 D5).
func loadDeployment(path string) (*profile.Deployment, error) {
	explicit := path != ""
	if !explicit {
		p, err := profile.DefaultDeploymentPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if _, err := os.Stat(path); !explicit && errors.Is(err, fs.ErrNotExist) {
		return profile.DefaultDeployment(), nil
	}
	return profile.LoadDeployment(path)
}

// withClient dials the server named by the deployment config and calls fn.
func withClient(cfgPath string, stderr io.Writer, name string, fn func(ctx context.Context, c apiv1.AgentLoopClient) error) int {
	dep, err := loadDeployment(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop %s: %v\n", name, err)
		return 1
	}
	c, closer, err := api.Dial(dep.API.Socket)
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop %s: %v\n", name, err)
		return 1
	}
	defer closer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := fn(ctx, c); err != nil {
		msg := err.Error()
		if st, ok := status.FromError(err); ok {
			msg = st.Message()
		}
		fmt.Fprintf(stderr, "agent-loop %s: %s\n", name, msg)
		return 1
	}
	return 0
}

func usageErr(stderr io.Writer, usage string) int {
	fmt.Fprintf(stderr, "usage: agent-loop %s\n", usage)
	return 2
}

func runBoard(args []string, stdout, stderr io.Writer) int {
	fs, cfg := newFlags("board", stderr)
	if fs.Parse(args) != nil || fs.NArg() > 1 {
		return usageErr(stderr, "board [--config <path>] [project]")
	}
	return withClient(*cfg, stderr, "board", func(ctx context.Context, c apiv1.AgentLoopClient) error {
		b, err := c.Board(ctx, &apiv1.BoardRequest{Project: fs.Arg(0)})
		if err != nil {
			return err
		}
		printBoard(stdout, b)
		return nil
	})
}

// printBoard writes the board (FR-11 AC1): the waits that have no label
// first, then one table per project.
func printBoard(w io.Writer, b *apiv1.BoardReply) {
	switch {
	case b.GetDeploymentUnknown():
		fmt.Fprintln(w, "Deployment: state unknown (the deployment workflow isn't answering)")
	case b.GetDeploymentPaused():
		fmt.Fprintln(w, "Deployment: paused (agent-loop resume to resume)")
	}
	for i, p := range b.GetProjects() {
		if i > 0 || b.GetDeploymentPaused() || b.GetDeploymentUnknown() {
			fmt.Fprintln(w)
		}
		var waits []string
		if p.GetUnknown() {
			waits = append(waits, "state unknown: "+p.GetError())
		}
		if p.GetPaused() {
			waits = append(waits, "paused")
		}
		if p.GetDailyLimitReached() {
			waits = append(waits, "daily limit reached")
		}
		head := "Project " + p.GetName()
		if len(waits) > 0 {
			head += ": " + strings.Join(waits, "; ")
		}
		fmt.Fprintln(w, head)
		if len(p.GetItems()) == 0 {
			fmt.Fprintln(w, "  no claimed items")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ITEM\tSTATE\tROUND\tBLOCKING\tHOST\tIN STATE\tNEXT ACTION\tWAITING ON")
		for _, it := range p.GetItems() {
			fmt.Fprintf(tw, "  %s\t%s\t%d/%d\t%d\t%s\t%s\t%s\t%s\n", it.GetItem(), it.GetState(), it.GetRound(), it.GetRoundsAllowed(),
				it.GetBlocking(), dash(it.GetHost()), (time.Duration(it.GetTimeInStateSeconds()) * time.Second).String(),
				dash(it.GetNextAction()), dash(it.GetWaitReason()))
		}
		tw.Flush()
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var itemKinds = map[string]apiv1.ItemCommandKind{
	"pause":  apiv1.ItemCommandKind_ITEM_COMMAND_KIND_PAUSE,
	"resume": apiv1.ItemCommandKind_ITEM_COMMAND_KIND_RESUME,
	"abort":  apiv1.ItemCommandKind_ITEM_COMMAND_KIND_ABORT,
}

func runItem(args []string, stdout, stderr io.Writer) int {
	const usage = "item pause|resume|abort [--config <path>] [--project <project>] <item>"
	if len(args) == 0 {
		return usageErr(stderr, usage)
	}
	kind, ok := itemKinds[args[0]]
	fs, cfg := newFlags("item "+args[0], stderr)
	project := fs.String("project", "", "the item's project, when its key is on more than one board")
	if !ok || fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
		return usageErr(stderr, usage)
	}
	return withClient(*cfg, stderr, "item "+args[0], func(ctx context.Context, c apiv1.AgentLoopClient) error {
		r, err := c.ItemCommand(ctx, &apiv1.ItemCommandRequest{Project: *project, Item: fs.Arg(0), Command: kind})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Sent %s to %s\n", args[0], r.GetWorkflowId())
		return nil
	})
}

var pauseKinds = map[string]apiv1.PauseCommandKind{
	"pause":  apiv1.PauseCommandKind_PAUSE_COMMAND_KIND_PAUSE,
	"resume": apiv1.PauseCommandKind_PAUSE_COMMAND_KIND_RESUME,
}

func runProject(args []string, stdout, stderr io.Writer) int {
	const usage = "project validate|pause|resume [--config <path>] <project>"
	if len(args) == 0 {
		return usageErr(stderr, usage)
	}
	if args[0] == "validate" {
		return runValidate(args[1:], stdout, stderr)
	}
	kind, ok := pauseKinds[args[0]]
	fs, cfg := newFlags("project "+args[0], stderr)
	if !ok || fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
		return usageErr(stderr, usage)
	}
	return withClient(*cfg, stderr, "project "+args[0], func(ctx context.Context, c apiv1.AgentLoopClient) error {
		r, err := c.ProjectCommand(ctx, &apiv1.ProjectCommandRequest{Project: fs.Arg(0), Command: kind})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Sent %s to %s\n", args[0], r.GetWorkflowId())
		return nil
	})
}

func runDeploymentCommand(name string) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		fs, cfg := newFlags(name, stderr)
		if fs.Parse(args) != nil || fs.NArg() != 0 {
			return usageErr(stderr, name+" [--config <path>]")
		}
		return withClient(*cfg, stderr, name, func(ctx context.Context, c apiv1.AgentLoopClient) error {
			r, err := c.DeploymentCommand(ctx, &apiv1.DeploymentCommandRequest{Command: pauseKinds[name]})
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Sent %s to %s\n", name, r.GetWorkflowId())
			return nil
		})
	}
}

func runLog(args []string, stdout, stderr io.Writer) int {
	fs, cfg := newFlags("log", stderr)
	project := fs.String("project", "", "the item's project, when its key is in more than one project")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		return usageErr(stderr, "log [--config <path>] [--project <project>] <item>")
	}
	return withClient(*cfg, stderr, "log", func(ctx context.Context, c apiv1.AgentLoopClient) error {
		r, err := c.ItemLog(ctx, &apiv1.ItemLogRequest{Project: *project, Item: fs.Arg(0)})
		if err != nil {
			return err
		}
		printLog(stdout, r)
		return nil
	})
}

// printLog writes an item's log, one line per entry (FR-11 AC2).
func printLog(w io.Writer, r *apiv1.ItemLogReply) {
	for _, e := range r.GetEntries() {
		at := time.Unix(0, e.GetAtUnixNano()).Local().Format(time.RFC3339)
		switch e.GetKind() {
		case "decision":
			line := fmt.Sprintf("%s  decision  %s: %s -> %s", at, e.GetEvent(), e.GetFromState(), e.GetToState())
			if e.GetRule() != "" {
				line += " (" + e.GetRule() + ")"
			}
			if len(e.GetActions()) > 0 {
				line += "; " + strings.Join(e.GetActions(), ", ")
			}
			fmt.Fprintln(w, line)
		case "run_start":
			fmt.Fprintf(w, "%s  start     %s%s attempt %d on %s (run %s)\n", at, e.GetStage(), kindSuffix(e.GetRunKind()), e.GetAttempt(), dash(e.GetHost()), e.GetRunId())
		case "run_end":
			fmt.Fprintf(w, "%s  end       %s%s attempt %d: %s, log %s\n", at, e.GetStage(), kindSuffix(e.GetRunKind()), e.GetAttempt(), dash(e.GetResult()), dash(e.GetLogPath()))
		}
	}
}

func kindSuffix(k string) string {
	if k == "" {
		return ""
	}
	return " (" + k + ")"
}
