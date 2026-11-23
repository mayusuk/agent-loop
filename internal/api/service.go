package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// ErrNotRunning is a Workflows error for a workflow that isn't running.
var ErrNotRunning = errors.New("api: workflow not running")

// Workflows are the calls the API makes on the running workflows: signals
// for commands and state queries for the board (TECHNICAL-SPEC §11).
// TemporalWorkflows implements it over a Temporal client.
type Workflows interface {
	Signal(ctx context.Context, workflowID, name string, arg any) error
	QueryProject(ctx context.Context, project string) (workflows.ProjectView, error)
	QueryDeployment(ctx context.Context) (workflows.DeploymentState, error)
}

// Projection is the read model the API reads; *projection.DB implements
// it.
type Projection interface {
	Board(ctx context.Context, project string, now time.Time) ([]projection.BoardRow, error)
	FindOnBoard(ctx context.Context, project, item string) ([]projection.BoardItem, error)
	ItemLog(ctx context.Context, project, item string) ([]projection.LogEntry, error)
}

var _ Projection = (*projection.DB)(nil)

// Service is the local API. Every call acts as the owner (TECHNICAL-SPEC
// §11): it is reachable only through the owner's socket or the
// token-protected gateway.
type Service struct {
	apiv1.UnimplementedAgentLoopServer

	Workflows  Workflows
	Projection Projection
	// Projects are the registered project names.
	Projects []string
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

var _ apiv1.AgentLoopServer = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Board lists every claimed item of each project, with the project's and
// deployment's waits that have no label: a pause and the daily limit
// (FR-11 AC1).
func (s *Service) Board(ctx context.Context, req *apiv1.BoardRequest) (*apiv1.BoardReply, error) {
	projects := slices.Clone(s.Projects)
	slices.Sort(projects)
	if req.GetProject() != "" {
		if err := s.known(req.GetProject()); err != nil {
			return nil, err
		}
		projects = []string{req.GetProject()}
	}
	out := &apiv1.BoardReply{}
	if d, err := s.Workflows.QueryDeployment(ctx); err != nil {
		out.DeploymentUnknown = true
	} else {
		out.DeploymentPaused = d.Paused
	}
	now := s.now()
	for _, p := range projects {
		pb := &apiv1.ProjectBoard{Name: p}
		if v, err := s.Workflows.QueryProject(ctx, p); err != nil {
			pb.Unknown = true
			pb.Error = notRunning(err, "project "+p)
		} else {
			pb.Paused = v.State.Paused
			pb.DailyLimitReached = v.DailyLimitReached
			for item := range v.State.Items {
				pb.RunningItems = append(pb.RunningItems, item)
			}
			slices.Sort(pb.RunningItems)
			pb.DailyRuns, pb.Day = int64(v.State.Daily.Runs), v.State.Daily.Day
			if v.State.Spend.Day == v.State.Daily.Day {
				pb.DailySpend = v.State.Spend.Amount
			}
		}
		rows, err := s.Projection.Board(ctx, p, now)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "board: %v", err)
		}
		for _, r := range rows {
			pb.Items = append(pb.Items, &apiv1.BoardItem{
				Item: r.Item, State: string(r.State), Round: int64(r.Round), RoundsAllowed: int64(r.RoundsAllowed),
				Blocking: int64(r.Blocking), Host: r.Host, TimeInStateSeconds: int64(r.TimeInState / time.Second),
				NextAction: r.NextAction, WaitReason: r.WaitReason,
			})
		}
		out.Projects = append(out.Projects, pb)
	}
	return out, nil
}

func notRunning(err error, what string) string {
	if errors.Is(err, ErrNotRunning) {
		return what + " isn't running: start it with agent-loop server start"
	}
	return err.Error()
}

func (s *Service) known(project string) error {
	if !slices.Contains(s.Projects, project) {
		return status.Errorf(codes.NotFound, "project %q isn't registered in the deployment config", project)
	}
	return nil
}

var itemCommands = map[apiv1.ItemCommandKind]core.Command{
	apiv1.ItemCommandKind_ITEM_COMMAND_KIND_PAUSE:  core.CommandPause,
	apiv1.ItemCommandKind_ITEM_COMMAND_KIND_RESUME: core.CommandResume,
	apiv1.ItemCommandKind_ITEM_COMMAND_KIND_ABORT:  core.CommandAbort,
}

// ItemCommand signals an owner command to the item's workflow (FR-10 AC3,
// AC7). The item is found on the board; without a project it must be on
// one project's board only (work plan #22 D3).
func (s *Service) ItemCommand(ctx context.Context, req *apiv1.ItemCommandRequest) (*apiv1.CommandReply, error) {
	cmd, ok := itemCommands[req.GetCommand()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "item command: pause, resume or abort")
	}
	if req.GetItem() == "" {
		return nil, status.Error(codes.InvalidArgument, "item command: no item")
	}
	if req.GetProject() != "" {
		if err := s.known(req.GetProject()); err != nil {
			return nil, err
		}
	}
	found, err := s.Projection.FindOnBoard(ctx, req.GetProject(), req.GetItem())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "item command: %v", err)
	}
	switch len(found) {
	case 0:
		return nil, status.Errorf(codes.NotFound, "item %s isn't on the board", req.GetItem())
	case 1:
	default:
		var ps []string
		for _, f := range found {
			ps = append(ps, f.Project)
		}
		return nil, status.Errorf(codes.FailedPrecondition, "item %s is on the board of %s: name the project", req.GetItem(), strings.Join(ps, ", "))
	}
	sig := workflows.MustSignal(core.OwnerCommand{At: core.At{Time: s.now()}, Command: cmd})
	if err := s.signal(ctx, found[0].WorkflowID, workflows.SignalEvent, sig); err != nil {
		return nil, err
	}
	return &apiv1.CommandReply{WorkflowId: found[0].WorkflowID}, nil
}

func pauseOf(k apiv1.PauseCommandKind) (workflows.PauseSignal, error) {
	switch k {
	case apiv1.PauseCommandKind_PAUSE_COMMAND_KIND_PAUSE:
		return workflows.PauseSignal{Paused: true}, nil
	case apiv1.PauseCommandKind_PAUSE_COMMAND_KIND_RESUME:
		return workflows.PauseSignal{Paused: false}, nil
	}
	return workflows.PauseSignal{}, status.Error(codes.InvalidArgument, "pause or resume")
}

// ProjectCommand signals a pause or resume to the project's workflow
// (FR-10 AC3).
func (s *Service) ProjectCommand(ctx context.Context, req *apiv1.ProjectCommandRequest) (*apiv1.CommandReply, error) {
	sig, err := pauseOf(req.GetCommand())
	if err != nil {
		return nil, err
	}
	if err := s.known(req.GetProject()); err != nil {
		return nil, err
	}
	id := workflows.ProjectID(req.GetProject())
	if err := s.signal(ctx, id, workflows.SignalPause, sig); err != nil {
		return nil, err
	}
	return &apiv1.CommandReply{WorkflowId: id}, nil
}

// DeploymentCommand signals a pause or resume to the deployment workflow
// (FR-10 AC3).
func (s *Service) DeploymentCommand(ctx context.Context, req *apiv1.DeploymentCommandRequest) (*apiv1.CommandReply, error) {
	sig, err := pauseOf(req.GetCommand())
	if err != nil {
		return nil, err
	}
	if err := s.signal(ctx, workflows.DeploymentID, workflows.SignalPause, sig); err != nil {
		return nil, err
	}
	return &apiv1.CommandReply{WorkflowId: workflows.DeploymentID}, nil
}

func (s *Service) signal(ctx context.Context, id, name string, arg any) error {
	err := s.Workflows.Signal(ctx, id, name, arg)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotRunning):
		return status.Errorf(codes.FailedPrecondition, "workflow %s isn't running", id)
	}
	return status.Errorf(codes.Unavailable, "signal %s: %v", id, err)
}

// ItemLog returns an item's stage runs and decisions in time order (FR-11
// AC2). Without a project the item must be in one project's log only.
func (s *Service) ItemLog(ctx context.Context, req *apiv1.ItemLogRequest) (*apiv1.ItemLogReply, error) {
	if req.GetItem() == "" {
		return nil, status.Error(codes.InvalidArgument, "log: no item")
	}
	projects := []string{req.GetProject()}
	if req.GetProject() != "" {
		if err := s.known(req.GetProject()); err != nil {
			return nil, err
		}
	} else {
		projects = slices.Clone(s.Projects)
		slices.Sort(projects)
	}
	var (
		found   string
		entries []projection.LogEntry
	)
	for _, p := range projects {
		es, err := s.Projection.ItemLog(ctx, p, req.GetItem())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "log: %v", err)
		}
		if len(es) == 0 {
			continue
		}
		if found != "" {
			return nil, status.Errorf(codes.FailedPrecondition, "item %s is in the log of %s and %s: name the project", req.GetItem(), found, p)
		}
		found, entries = p, es
	}
	if found == "" {
		return nil, status.Errorf(codes.NotFound, "item %s has no log", req.GetItem())
	}
	out := &apiv1.ItemLogReply{Project: found}
	for _, e := range entries {
		out.Entries = append(out.Entries, logEntry(e))
	}
	return out, nil
}

func logEntry(e projection.LogEntry) *apiv1.LogEntry {
	out := &apiv1.LogEntry{AtUnixNano: e.At.UnixNano()}
	switch {
	case e.Decision != nil:
		r := e.Decision.Record
		out.Kind = "decision"
		out.Event, out.FromState, out.ToState, out.Rule = r.Event, string(r.From), string(r.To), r.Rule
		out.Claim = int64(e.Decision.Claim)
		out.Actions = slices.Clone(r.Actions)
	case e.Run != nil:
		r := e.Run
		out.Kind = "run_start"
		if e.RunEnd {
			out.Kind = "run_end"
			out.Result, out.LogPath = r.Result, r.LogPath
		}
		out.Stage, out.RunKind, out.RunId, out.Attempt, out.Host = string(r.Stage), r.Kind, r.RunID, int64(r.Attempt), r.Host
	}
	return out
}
