package notify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

type fakeDisplay struct {
	shown []Alert
	err   error
}

func (f *fakeDisplay) Show(_ context.Context, a Alert) error {
	f.shown = append(f.shown, a)
	return f.err
}

var escalation = ports.Notification{
	Project: "p", Item: "42", Reason: "round_limit",
	Message: "Item 42 needs you: the review-round limit was reached.",
	URL:     "https://github.com/o/r/issues/42", Key: "item/p/42#1:notify:7",
}

func TestDesktop_ShowsAlertThroughDisplay(t *testing.T) {
	f := &fakeDisplay{}
	if err := (Desktop{Display: f}).Notify(context.Background(), escalation); err != nil {
		t.Fatal(err)
	}
	want := []Alert{{
		Title: "agent-loop", Subtitle: "p 42: round_limit",
		Message: escalation.Message, URL: escalation.URL, Group: "item/p/42#1:notify:7",
	}}
	if !reflect.DeepEqual(f.shown, want) {
		t.Fatalf("got %+v\nwant %+v", f.shown, want)
	}
	f.err = errors.New("boom")
	if err := (Desktop{Display: f}).Notify(context.Background(), escalation); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v, want the display error", err)
	}
}

func TestAlertOf_FillsGaps(t *testing.T) {
	cases := []struct {
		n    ports.Notification
		want Alert
	}{
		{ports.Notification{Reason: "gate_failed"}, Alert{Title: "agent-loop", Subtitle: "gate_failed", Message: "gate_failed"}},
		{ports.Notification{Project: "p"}, Alert{Title: "agent-loop", Subtitle: "p", Message: "agent-loop needs your attention"}},
		{ports.Notification{Project: "p", Item: "7", Message: "merged"}, Alert{Title: "agent-loop", Subtitle: "p 7", Message: "merged"}},
	}
	for _, c := range cases {
		if got := AlertOf(c.n); got != c.want {
			t.Errorf("AlertOf(%+v) = %+v, want %+v", c.n, got, c.want)
		}
	}
}

// recorder fakes LookPath and Run for MacDisplay.
type recorder struct {
	found map[string]string
	calls [][]string
}

func (r *recorder) look(f string) (string, error) {
	if p, ok := r.found[f]; ok {
		return p, nil
	}
	return "", exec.ErrNotFound
}

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil
}

func TestMacDisplay_PrefersTerminalNotifier(t *testing.T) {
	r := &recorder{found: map[string]string{"terminal-notifier": "/opt/bin/terminal-notifier"}}
	m := MacDisplay{LookPath: r.look, Run: r.run, GOOS: "darwin"}
	a := AlertOf(escalation)
	if err := m.Show(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"/opt/bin/terminal-notifier",
		"-title", "agent-loop", "-subtitle", "p 42: round_limit", "-message", escalation.Message,
		"-group", "item/p/42#1:notify:7", "-open", escalation.URL}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("got %q\nwant %q", r.calls, want)
	}
	// No group and no URL: no flags for them.
	if got := TerminalNotifierArgs(Alert{Title: "t", Subtitle: "s", Message: "m"}); len(got) != 6 {
		t.Fatalf("args %q", got)
	}
}

func TestMacDisplay_FallsBackToOsascriptWithTextAsArguments(t *testing.T) {
	r := &recorder{}
	m := MacDisplay{LookPath: r.look, Run: r.run, GOOS: "darwin"}
	a := Alert{Title: `t "quoted"`, Subtitle: `-s`, Message: `end run" & do shell script "x`}
	if err := m.Show(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	got := r.calls[0]
	if got[0] != Osascript {
		t.Fatalf("ran %s", got[0])
	}
	// The script is fixed; the text only follows "--".
	n := len(got)
	if got[n-4] != "--" || got[n-3] != a.Message || got[n-2] != a.Title || got[n-1] != a.Subtitle {
		t.Fatalf("args %q", got)
	}
	for _, s := range got[1 : n-4] {
		if strings.Contains(s, "quoted") || strings.Contains(s, "shell") {
			t.Fatalf("text reached the script: %q", got)
		}
	}
}

func TestMacDisplay_OffMacOS(t *testing.T) {
	r := &recorder{}
	if err := (MacDisplay{LookPath: r.look, Run: r.run, GOOS: "linux"}).Show(context.Background(), Alert{}); !errors.Is(err, ErrNoDesktop) {
		t.Fatalf("got %v, want ErrNoDesktop", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("ran %q", r.calls)
	}
}

// TestMacDisplay_Live shows a real notification. It runs only on macOS
// with AGENT_LOOP_LIVE=1.
func TestMacDisplay_Live(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("needs macOS and AGENT_LOOP_LIVE=1")
	}
	n := ports.Notification{Project: "agent-loop", Item: "test", Reason: "live_test", Message: "-a notifier test, safe to ignore"}
	if err := (Desktop{Display: MacDisplay{}}).Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

func TestTracker_MentionAndNoOpNotify(t *testing.T) {
	if err := (Tracker{}).Notify(context.Background(), escalation); err != nil {
		t.Fatal(err)
	}
	owners := []profile.Owner{{Login: "alice", ID: "1"}, {ID: "2"}, {Login: "bob", ID: "3"}}
	if got := Mention(owners); got != "@alice @bob" {
		t.Fatalf("Mention = %q", got)
	}
	if got := Mention(nil); got != "" {
		t.Fatalf("Mention(nil) = %q", got)
	}
}

type failing struct{ err error }

func (f failing) Notify(context.Context, ports.Notification) error { return f.err }

func TestMulti_SendsToEveryChannel(t *testing.T) {
	a, b := &fakeDisplay{}, &fakeDisplay{}
	e1 := errors.New("first")
	m := Multi{Desktop{Display: a}, failing{e1}, Desktop{Display: b}}
	err := m.Notify(context.Background(), escalation)
	if !errors.Is(err, e1) {
		t.Fatalf("got %v, want the failing channel's error", err)
	}
	if len(a.shown) != 1 || len(b.shown) != 1 {
		t.Fatalf("channels after a failure were skipped: %d, %d", len(a.shown), len(b.shown))
	}
	if err := (Multi{}).Notify(context.Background(), escalation); err != nil {
		t.Fatal(err)
	}
}

func TestNew_FromDeploymentChannels(t *testing.T) {
	f := &fakeDisplay{}
	n, warns, err := New(profile.DefaultNotifiers, Options{Display: f, GOOS: "darwin"})
	if err != nil || len(warns) != 0 {
		t.Fatalf("err %v, warnings %v", err, warns)
	}
	if want := (Multi{Desktop{Display: f}, Tracker{}}); !reflect.DeepEqual(n, want) {
		t.Fatalf("got %#v", n)
	}
	if err := n.Notify(context.Background(), escalation); err != nil || len(f.shown) != 1 {
		t.Fatalf("notify: %v, shown %d", err, len(f.shown))
	}

	// The default display is the real one.
	n, _, _ = New([]string{ChannelDesktop}, Options{GOOS: "darwin"})
	if want := (Multi{Desktop{Display: MacDisplay{GOOS: "darwin"}}}); !reflect.DeepEqual(n, want) {
		t.Fatalf("got %#v", n)
	}

	// Off macOS the desktop channel is left out with a warning.
	n, warns, err = New(profile.DefaultNotifiers, Options{Display: f, GOOS: "linux"})
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "linux") {
		t.Fatalf("err %v, warnings %v", err, warns)
	}
	if want := (Multi{Tracker{}}); !reflect.DeepEqual(n, want) {
		t.Fatalf("got %#v", n)
	}

	if _, _, err := New([]string{"slack"}, Options{}); err == nil {
		t.Fatal("unknown channel: want an error")
	}
}
