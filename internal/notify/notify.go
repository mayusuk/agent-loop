package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

// Channel names in the deployment config (profile.DefaultNotifiers).
const (
	ChannelDesktop = "desktop"
	ChannelTracker = "tracker"
)

// ErrNoDesktop is returned when desktop notifications can't be shown on
// this platform.
var ErrNoDesktop = errors.New("desktop notifications need macOS")

// Alert is one desktop notification.
type Alert struct {
	Title    string
	Subtitle string
	Message  string
	URL      string // opened when the owner clicks it, if the display can
	Group    string // a repeat with the same group replaces the earlier one, if the display can
}

// Display shows desktop notifications. MacDisplay is the real one; tests
// use a fake.
type Display interface {
	Show(ctx context.Context, a Alert) error
}

// Desktop is the desktop channel (TECHNICAL-SPEC §12).
type Desktop struct{ Display Display }

// Notify shows n on the desktop.
func (d Desktop) Notify(ctx context.Context, n ports.Notification) error {
	if err := d.Display.Show(ctx, AlertOf(n)); err != nil {
		return fmt.Errorf("notify: desktop: %w", err)
	}
	return nil
}

// AlertOf is the desktop alert for a notification (work plan D10): the
// title "agent-loop", the subtitle "<project> <item>" and the reason, the
// message (the reason when there is none, and a fixed fallback when there
// is neither), the item's URL, and the
// idempotency key as the group.
func AlertOf(n ports.Notification) Alert {
	sub := strings.TrimSpace(n.Project + " " + string(n.Item))
	if n.Reason != "" {
		if sub != "" {
			sub += ": "
		}
		sub += n.Reason
	}
	msg := n.Message
	if msg == "" {
		msg = n.Reason
	}
	if msg == "" {
		msg = "agent-loop needs your attention"
	}
	return Alert{Title: "agent-loop", Subtitle: sub, Message: msg, URL: n.URL, Group: string(n.Key)}
}

// MacDisplay shows notifications with terminal-notifier when it is on PATH
// (it can open the URL and replace by group), and otherwise with osascript
// (work plan D9). The text is passed as arguments, never built into a
// script.
type MacDisplay struct {
	// LookPath and Run default to exec.LookPath and running the command.
	LookPath func(file string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) error
	// GOOS defaults to runtime.GOOS.
	GOOS string
}

// Osascript is the AppleScript runner used when terminal-notifier is
// missing.
const Osascript = "/usr/bin/osascript"

// Show shows a.
func (m MacDisplay) Show(ctx context.Context, a Alert) error {
	if goos(m.GOOS) != "darwin" {
		return ErrNoDesktop
	}
	look, run := m.LookPath, m.Run
	if look == nil {
		look = exec.LookPath
	}
	if run == nil {
		run = runCommand
	}
	if p, err := look("terminal-notifier"); err == nil {
		return run(ctx, p, TerminalNotifierArgs(a)...)
	}
	return run(ctx, Osascript, OsascriptArgs(a)...)
}

// TerminalNotifierArgs are terminal-notifier's arguments for a.
func TerminalNotifierArgs(a Alert) []string {
	args := []string{"-title", a.Title, "-subtitle", a.Subtitle, "-message", a.Message}
	if a.Group != "" {
		args = append(args, "-group", a.Group)
	}
	if a.URL != "" {
		args = append(args, "-open", a.URL)
	}
	return args
}

// OsascriptArgs are osascript's arguments for a: a fixed script that reads
// the text from its arguments, after "--" so text starting with "-" is not
// an option.
func OsascriptArgs(a Alert) []string {
	return []string{
		"-e", "on run argv",
		"-e", "display notification (item 1 of argv) with title (item 2 of argv) subtitle (item 3 of argv)",
		"-e", "end run",
		"--", a.Message, a.Title, a.Subtitle,
	}
}

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func goos(s string) string {
	if s == "" {
		return runtime.GOOS
	}
	return s
}

// Tracker is the tracker channel. Its notification is the owner mention in
// the escalation status comment, which the escalation activity posts with
// Mention (INTEGRATION-SPEC §6.5), so Notify itself writes nothing
// (work plan D11).
type Tracker struct{}

// Notify does nothing: the escalation comment carries the mention.
func (Tracker) Notify(context.Context, ports.Notification) error { return nil }

// Mention is the owner mention an escalation comment carries: "@login" for
// each owner account with a login, in profile order, separated by spaces.
func Mention(owners []profile.Owner) string {
	var b strings.Builder
	for _, o := range owners {
		if o.Login == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("@" + o.Login)
	}
	return b.String()
}

// Multi sends each notification to every channel. One channel failing
// doesn't stop the others; the errors are joined.
type Multi []ports.Notifier

// Notify sends n to every channel.
func (m Multi) Notify(ctx context.Context, n ports.Notification) error {
	var errs []error
	for _, c := range m {
		if err := c.Notify(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Options configure New.
type Options struct {
	Display Display // defaults to MacDisplay{}
	GOOS    string  // defaults to runtime.GOOS
}

// New builds the notifier for the deployment's channels (FR-11 AC5). Off
// macOS the desktop channel is left out and named in the warnings, so a
// Linux server still notifies through the tracker (work plan D12). An
// unknown channel is an error.
func New(channels []string, o Options) (ports.Notifier, []string, error) {
	var m Multi
	var warnings []string
	for _, c := range channels {
		switch c {
		case ChannelDesktop:
			if goos(o.GOOS) != "darwin" {
				warnings = append(warnings, "desktop notifications need macOS; the desktop channel is off on "+goos(o.GOOS))
				continue
			}
			d := o.Display
			if d == nil {
				d = MacDisplay{GOOS: o.GOOS}
			}
			m = append(m, Desktop{Display: d})
		case ChannelTracker:
			m = append(m, Tracker{})
		default:
			return nil, nil, fmt.Errorf("notify: unknown channel %q", c)
		}
	}
	return m, warnings, nil
}
