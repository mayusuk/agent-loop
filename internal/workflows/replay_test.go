package workflows

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// The recorded histories (TECHNICAL-SPEC §14): an item from claim to the
// loop's merge, and the project run that claimed it, up to its first
// continue-as-new.
var histories = []string{"item_claim_to_merge.json", "project_first_run.json"}

// TestT16_ReplayRecordedHistories replays each recorded history against
// the current workflow code. A workflow change that would break a running
// workflow fails here; such a change needs a new worker version.
func TestT16_ReplayRecordedHistories(t *testing.T) {
	for _, name := range histories {
		t.Run(name, func(t *testing.T) {
			r := worker.NewWorkflowReplayer()
			RegisterWorkflows(r)
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "histories", name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// historyIdentity is the client and worker identity in the recorded histories, in
// place of the default <pid>@<hostname>@ (review R2).
const historyIdentity = "recorder"

// runsRoot stands in for the recording run's temp RunsDir in the
// histories' payloads.
const runsRoot = "/runs"

// scrub removes the recording machine's details from a history: every
// identity becomes historyIdentity, the hostname in any other string (the
// sticky task queue's name) becomes it too, and runsDir in any payload
// becomes runsRoot. Replay reads none of them: identities and queue names
// are metadata, and the paths are only passed on to activities.
func scrub(m protoreflect.Message, runsDir, host string) {
	if p, ok := m.Interface().(*commonpb.Payload); ok {
		p.Data = bytes.ReplaceAll(p.Data, []byte(runsDir), []byte(runsRoot))
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap() && fd.Name() == "identity":
			m.Set(fd, protoreflect.ValueOfString(historyIdentity))
		case fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap():
			if host != "" && strings.Contains(v.String(), host) {
				m.Set(fd, protoreflect.ValueOfString(strings.ReplaceAll(v.String(), host, historyIdentity)))
			}
		case fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind:
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				scrub(l.Get(i).Message(), runsDir, host)
			}
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					scrub(mv.Message(), runsDir, host)
					return true
				})
			}
		default:
			scrub(v.Message(), runsDir, host)
		}
		return true
	})
}

// TestT16_HistoriesCarryNoMachineDetails: the committed histories name no
// worker but historyIdentity and no local path (review R2).
func TestT16_HistoriesCarryNoMachineDetails(t *testing.T) {
	host, _ := os.Hostname()
	for _, name := range histories {
		b, err := os.ReadFile(filepath.Join("testdata", "histories", name))
		if err != nil {
			t.Fatal(err)
		}
		var h history.History
		if err := protojson.Unmarshal(b, &h); err != nil {
			t.Fatal(err)
		}
		var texts []string
		var walk func(m protoreflect.Message)
		walk = func(m protoreflect.Message) {
			if p, ok := m.Interface().(*commonpb.Payload); ok {
				texts = append(texts, string(p.Data))
				return
			}
			m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
				switch {
				case fd.Name() == "identity" && fd.Kind() == protoreflect.StringKind:
					if id := v.String(); id != historyIdentity && id != "history-service" {
						t.Errorf("%s: identity %q", name, id)
					}
				case fd.Kind() != protoreflect.MessageKind:
				case fd.IsList():
					for i := 0; i < v.List().Len(); i++ {
						walk(v.List().Get(i).Message())
					}
				case fd.IsMap():
					v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
						walk(mv.Message())
						return true
					})
				default:
					walk(v.Message())
				}
				return true
			})
		}
		walk(h.ProtoReflect())
		texts = append(texts, string(b))
		for _, s := range texts {
			for _, bad := range []string{"/var/folders/", "/private/", "/Users/", "/home/", "/tmp/", os.TempDir()} {
				if strings.Contains(s, bad) {
					t.Errorf("%s: carries the local path %q", name, bad)
				}
			}
			if host != "" && strings.Contains(s, host) {
				t.Errorf("%s: carries the hostname %q", name, host)
			}
		}
	}
}

// TestT16_ScrubReplacesIdentityAndRunsDir checks scrub on a small history.
func TestT16_ScrubReplacesIdentityAndRunsDir(t *testing.T) {
	h := &history.History{Events: []*history.HistoryEvent{{
		Attributes: &history.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &history.ActivityTaskCompletedEventAttributes{
			Identity:      "123@laptop.local@",
			WorkerVersion: &commonpb.WorkerVersionStamp{BuildId: "laptop.local:abc"},
			Result:        &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: []byte(`{"log":"/var/folders/x/T/Rec1/001/item/run.log"}`)}}},
		}},
	}}}
	scrub(h.ProtoReflect(), "/var/folders/x/T/Rec1/001", "laptop.local")
	a := h.Events[0].GetActivityTaskCompletedEventAttributes()
	if a.Identity != historyIdentity {
		t.Errorf("identity %q", a.Identity)
	}
	if got := a.WorkerVersion.BuildId; got != historyIdentity+":abc" {
		t.Errorf("hostname left in %q", got)
	}
	if got := string(a.Result.Payloads[0].Data); got != `{"log":"/runs/item/run.log"}` {
		t.Errorf("payload %s", got)
	}
}

// TestT16_RecordHistories writes the histories above from a run on a local
// Temporal dev server. It runs only with AGENT_LOOP_LIVE=1 and
// AGENT_LOOP_RECORD=1, after a change that needs new histories.
func TestT16_RecordHistories(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" || os.Getenv("AGENT_LOOP_RECORD") != "1" {
		t.Skip("set AGENT_LOOP_LIVE=1 AGENT_LOOP_RECORD=1 to record histories on a dev server")
	}
	ctx := context.Background()
	srv, err := testsuite.StartDevServer(ctx, testsuite.DevServerOptions{ClientOptions: &client.Options{Identity: historyIdentity}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	c := srv.Client()

	w := newWF(t)
	w.a.Now = time.Now
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())
	for _, q := range []string{CoreQueue, CapabilityQueue(nil)} {
		wk := worker.New(c, q, worker.Options{Identity: historyIdentity})
		if q == CoreQueue {
			RegisterWorkflows(wk)
		}
		RegisterActivities(wk, w.a)
		if err := wk.Start(); err != nil {
			t.Fatal(err)
		}
		defer wk.Stop()
	}
	cfg := w.cfg
	cfg.PollInterval = time.Second
	cfg.Item.PollInterval = time.Second
	cfg.SupervisorInterval = 20 * time.Second
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: ProjectID("p"), TaskQueue: CoreQueue}, ProjectWorkflow, ProjectInput{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	itemID := core.ClaimFor("p", string(item), 0).WorkflowID
	deadline := time.Now().Add(time.Minute)
	for {
		it, err := w.f.Tracker().Item(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if it.CloseReason == ports.CloseCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the item did not merge within a minute")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := c.GetWorkflow(ctx, itemID, "").Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// The project's first run ends with continue-as-new at its first
	// reconcile.
	time.Sleep(25 * time.Second)
	save := func(id, runID, name string) {
		var events []*history.HistoryEvent
		it := c.GetWorkflowHistory(ctx, id, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for it.HasNext() {
			e, err := it.Next()
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		}
		h := &history.History{Events: events}
		host, _ := os.Hostname()
		scrub(h.ProtoReflect(), w.a.RunsDir, host)
		b, err := protojson.MarshalOptions{Indent: "  "}.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join("testdata", "histories")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	save(itemID, "", histories[0])
	save(ProjectID("p"), run.GetRunID(), histories[1])
	c.TerminateWorkflow(ctx, ProjectID("p"), "", "recorded")
}
