package githubtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// ErrNotYet is returned by the Driver's code-host actions, which come with
// the CodeHost (plan T10).
var ErrNotYet = errors.New("githubtest: code-host driver actions come with the CodeHost (T10)")

// Driver acts in a GitHub repository (the stub, or a live sandbox) as a
// given account, with that account's token. It implements the tracker part
// of conformance.Driver; its code-host actions return ErrNotYet.
type Driver struct {
	BaseURL string // the stub's URL, or https://api.github.com
	Repo    string // owner/name
	// Tokens maps an account's ID to its token.
	Tokens map[string]string
	// Labels maps markers to labels; DefaultLabels fills the rest.
	Labels map[ports.Marker]string
	HTTP   *http.Client
}

func (d *Driver) label(m ports.Marker) string {
	if l, ok := d.Labels[m]; ok {
		return l
	}
	return github.DefaultLabels[m]
}

func (d *Driver) call(as ports.Actor, method, path string, in, out any) error {
	tok, ok := d.Tokens[as.ID]
	if !ok {
		return fmt.Errorf("githubtest: no token for %s (%s)", as.Login, as.ID)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, strings.TrimRight(d.BaseURL, "/")+"/repos/"+d.Repo+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hc := d.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	switch {
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("githubtest: %s %s as %s: %d %s: %w", method, path, as.Login, res.StatusCode, data, ports.ErrPermission)
	case res.StatusCode >= 300:
		return fmt.Errorf("githubtest: %s %s as %s: %d %s", method, path, as.Login, res.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Marker returns the record marker line for rec, as the adapter writes it.
func Marker(rec ports.Record) string {
	var buf bytes.Buffer
	if json.Compact(&buf, rec.Data) != nil {
		buf.Reset()
		buf.WriteString(`{"type":"` + string(rec.Type) + `","key":"` + string(rec.Key) + `"}`)
	}
	return "<!-- agent-loop:v1 " + buf.String() + " -->"
}

func (d *Driver) NewItem(as ports.Actor, title, body string, markers ...ports.Marker) (ports.ItemRef, error) {
	labels := []string{}
	for _, m := range markers {
		labels = append(labels, d.label(m))
	}
	var it struct {
		Number int `json:"number"`
	}
	if err := d.call(as, "POST", "/issues", map[string]any{"title": title, "body": body, "labels": labels}, &it); err != nil {
		return "", err
	}
	return ports.ItemRef(strconv.Itoa(it.Number)), nil
}

func (d *Driver) SetMarker(as ports.Actor, item ports.ItemRef, m ports.Marker, on bool) error {
	if on {
		return d.call(as, "POST", "/issues/"+string(item)+"/labels", map[string]any{"labels": []string{d.label(m)}}, nil)
	}
	return d.call(as, "DELETE", "/issues/"+string(item)+"/labels/"+url.PathEscape(d.label(m)), nil, nil)
}

func (d *Driver) Assign(as ports.Actor, item ports.ItemRef, assignee *ports.Actor) error {
	if assignee != nil {
		return d.call(as, "POST", "/issues/"+string(item)+"/assignees", map[string]any{"assignees": []string{assignee.Login}}, nil)
	}
	var it struct {
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
	}
	if err := d.call(as, "GET", "/issues/"+string(item), nil, &it); err != nil {
		return err
	}
	if len(it.Assignees) == 0 {
		return nil
	}
	var logins []string
	for _, a := range it.Assignees {
		logins = append(logins, a.Login)
	}
	return d.call(as, "DELETE", "/issues/"+string(item)+"/assignees", map[string]any{"assignees": logins}, nil)
}

func (d *Driver) CommentItem(as ports.Actor, item ports.ItemRef, body string, rec *ports.Record) error {
	if rec != nil {
		body = strings.TrimRight(body, "\n") + "\n\n" + Marker(*rec)
	}
	return d.call(as, "POST", "/issues/"+string(item)+"/comments", map[string]any{"body": body}, nil)
}

func (d *Driver) CloseItem(as ports.Actor, item ports.ItemRef, reason ports.CloseReason) error {
	sr := "completed"
	if reason == ports.CloseNotDone {
		sr = "not_planned"
	}
	return d.call(as, "PATCH", "/issues/"+string(item), map[string]any{"state": "closed", "state_reason": sr}, nil)
}

func (d *Driver) ReopenItem(as ports.Actor, item ports.ItemRef) error {
	return d.call(as, "PATCH", "/issues/"+string(item), map[string]any{"state": "open"}, nil)
}

// The code-host actions come with the CodeHost (T10).

func (d *Driver) Push(ports.Actor, string) (ports.Commit, error)          { return "", ErrNotYet }
func (d *Driver) MergeBaseInto(ports.Actor, string) (ports.Commit, error) { return "", ErrNotYet }
func (d *Driver) OpenChange(ports.Actor, ports.ItemRef, string) (ports.ChangeRef, error) {
	return "", ErrNotYet
}
func (d *Driver) CommentChange(ports.Actor, ports.ChangeRef, string) error { return ErrNotYet }
func (d *Driver) ReviewComment(ports.Actor, ports.ChangeRef, string, int, string) (ports.ThreadRef, error) {
	return "", ErrNotYet
}
func (d *Driver) ReplyInThread(ports.Actor, ports.ChangeRef, ports.ThreadRef, string) error {
	return ErrNotYet
}
func (d *Driver) ResolveThread(ports.Actor, ports.ChangeRef, ports.ThreadRef) error { return ErrNotYet }
func (d *Driver) UnresolveThread(ports.Actor, ports.ChangeRef, ports.ThreadRef) error {
	return ErrNotYet
}
func (d *Driver) SubmitReview(ports.Actor, ports.ChangeRef, string, string, *ports.Record) (ports.ReviewRef, error) {
	return "", ErrNotYet
}
func (d *Driver) DismissReview(ports.Actor, ports.ChangeRef, ports.ReviewRef) error { return ErrNotYet }
func (d *Driver) MergeChange(ports.Actor, ports.ChangeRef) error                    { return ErrNotYet }
func (d *Driver) CloseChange(ports.Actor, ports.ChangeRef) error                    { return ErrNotYet }
func (d *Driver) ReopenChange(ports.Actor, ports.ChangeRef) error                   { return ErrNotYet }
func (d *Driver) CreateTag(ports.Actor, string) error                               { return ErrNotYet }
func (d *Driver) Replies(ports.ChangeRef, ports.ThreadRef) ([]ports.Record, error) {
	return nil, ErrNotYet
}
func (d *Driver) SetGate(ports.Commit, string, ports.GateState) error { return ErrNotYet }
func (d *Driver) RequireApprovals(int) error                          { return ErrNotYet }
func (d *Driver) SetRuleset(string, string) error                     { return ErrNotYet }
func (d *Driver) AddAdmin(ports.Actor) error                          { return ErrNotYet }
func (d *Driver) RemoveAdmin(ports.Actor) error                       { return ErrNotYet }

// StartCoderRun records nothing yet: pusher attribution is the CodeHost's
// (T10, T11).
func (d *Driver) StartCoderRun(ports.ItemRef) (func(), error) { return func() {}, nil }
