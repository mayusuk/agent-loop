package githubtest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// ErrNotYet is returned by the Driver's trust actions (fork branches, tags,
// rulesets, admins), which come with Trust (plan T11).
var ErrNotYet = errors.New("githubtest: trust driver actions come with Trust (T11)")

// Driver acts in a GitHub repository (the stub, or a live sandbox) as a
// given account, with that account's token. It implements
// conformance.Driver; its trust actions return ErrNotYet (work plan #15
// D19).
type Driver struct {
	BaseURL string // the stub's URL, or https://api.github.com
	Repo    string // owner/name
	// Tokens maps an account's ID to its token.
	Tokens map[string]string
	// Labels maps markers to labels; DefaultLabels fills the rest.
	Labels map[ports.Marker]string
	HTTP   *http.Client
	// Base is the base branch ("main" when empty).
	Base string
	// Owner is the account that changes repository settings.
	Owner ports.Actor

	pushes int
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
	u := strings.TrimRight(d.BaseURL, "/") + "/repos/" + d.Repo + path
	if path == "/graphql" {
		u = strings.TrimRight(d.BaseURL, "/") + "/graphql"
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, body)
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

func (d *Driver) base() string {
	if d.Base == "" {
		return "main"
	}
	return d.Base
}

// BaseFile is the file pushes to the base branch change, so merging the
// base into a loop branch never conflicts (work plan #15 D19).
const BaseFile = "conformance-base.txt"

// ChangedFile is conformance.ChangedFile (the conformance package imports
// this one's users, not the other way round).
const ChangedFile = "conformance.txt"

func (d *Driver) tip(as ports.Actor, branch string) (string, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := d.call(as, "GET", "/branches/"+url.PathEscape(branch), nil, &b)
	return b.Commit.SHA, err
}

// Push commits one change to ChangedFile on branch (BaseFile on the base),
// creating the branch from the base tip if it doesn't exist.
func (d *Driver) Push(as ports.Actor, branch string) (ports.Commit, error) {
	if strings.Contains(branch, ":") {
		return "", ErrNotYet
	}
	if _, err := d.tip(as, branch); err != nil {
		base, err := d.tip(as, d.base())
		if err != nil {
			return "", err
		}
		if err := d.call(as, "POST", "/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": base}, nil); err != nil {
			return "", err
		}
	}
	file := ChangedFile
	if branch == d.base() {
		file = BaseFile
	}
	var cur struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	content := ""
	in := map[string]any{"branch": branch}
	if err := d.call(as, "GET", "/contents/"+file+"?ref="+url.QueryEscape(branch), nil, &cur); err == nil {
		raw, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(cur.Content, "\n", ""))
		content = string(raw)
		in["sha"] = cur.SHA
	}
	d.pushes++
	content += fmt.Sprintf("push %d by %s\n", d.pushes, as.Login)
	in["message"] = "conformance push"
	in["content"] = base64.StdEncoding.EncodeToString([]byte(content))
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := d.call(as, "PUT", "/contents/"+file, in, &out); err != nil {
		return "", err
	}
	return ports.Commit(out.Commit.SHA), nil
}

// MergeBaseInto merges the base tip into branch.
func (d *Driver) MergeBaseInto(as ports.Actor, branch string) (ports.Commit, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	if err := d.call(as, "POST", "/merges", map[string]any{"base": branch, "head": d.base(), "commit_message": "Merge base"}, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		sha, err := d.tip(as, branch)
		return ports.Commit(sha), err
	}
	return ports.Commit(out.SHA), nil
}

// OpenChange opens a pull request from branch. A non-empty item becomes a
// closing reference ("Closes #n").
func (d *Driver) OpenChange(as ports.Actor, item ports.ItemRef, branch string) (ports.ChangeRef, error) {
	if strings.Contains(branch, ":") {
		return "", ErrNotYet
	}
	body := "Opened by the conformance driver."
	if item != "" {
		body += "\n\nCloses #" + string(item)
	}
	var pr struct {
		Number int `json:"number"`
	}
	if err := d.call(as, "POST", "/pulls", map[string]any{"title": "conformance " + branch, "head": branch, "base": d.base(), "body": body}, &pr); err != nil {
		return "", err
	}
	return ports.ChangeRef(strconv.Itoa(pr.Number)), nil
}

func (d *Driver) CommentChange(as ports.Actor, change ports.ChangeRef, body string) error {
	return d.call(as, "POST", "/issues/"+string(change)+"/comments", map[string]any{"body": body}, nil)
}

// driverThread is a review thread as the driver reads it.
type driverThread struct {
	ID       string `json:"id"`
	Comments struct {
		Nodes []struct {
			DatabaseID int64  `json:"databaseId"`
			Body       string `json:"body"`
			Author     struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
				ID       int64  `json:"databaseId"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

const driverThreadsQuery = `query DriverThreads($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes { id comments(first: 100) { nodes { databaseId body author { __typename login ... on User { databaseId } ... on Bot { databaseId } } } } }
      }
    }
  }
}`

func (d *Driver) graphql(as ports.Actor, query string, vars map[string]any, out any) error {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := d.call(as, "POST", "/graphql", map[string]any{"query": query, "variables": vars}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		if res.Errors[0].Type == "FORBIDDEN" {
			return fmt.Errorf("githubtest: graphql as %s: %s: %w", as.Login, res.Errors[0].Message, ports.ErrPermission)
		}
		return fmt.Errorf("githubtest: graphql as %s: %s", as.Login, res.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Data, out)
}

func (d *Driver) owner() (string, string) {
	o, n, _ := strings.Cut(d.Repo, "/")
	return o, n
}

func (d *Driver) threads(as ports.Actor, change ports.ChangeRef) ([]driverThread, error) {
	n, err := strconv.Atoi(string(change))
	if err != nil {
		return nil, fmt.Errorf("githubtest: change %q: %w", change, ports.ErrNotFound)
	}
	o, name := d.owner()
	var res struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []driverThread `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := d.graphql(as, driverThreadsQuery, map[string]any{"owner": o, "name": name, "number": n}, &res); err != nil {
		return nil, err
	}
	return res.Repository.PullRequest.ReviewThreads.Nodes, nil
}

func (d *Driver) thread(as ports.Actor, change ports.ChangeRef, ref ports.ThreadRef) (driverThread, error) {
	ts, err := d.threads(as, change)
	if err != nil {
		return driverThread{}, err
	}
	for _, t := range ts {
		if t.ID == string(ref) {
			return t, nil
		}
	}
	return driverThread{}, fmt.Errorf("githubtest: thread %s on change %s: %w", ref, change, ports.ErrNotFound)
}

func (d *Driver) head(as ports.Actor, change ports.ChangeRef) (string, error) {
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	err := d.call(as, "GET", "/pulls/"+string(change), nil, &pr)
	return pr.Head.SHA, err
}

// ReviewComment starts a review thread on path and line (0: the file).
func (d *Driver) ReviewComment(as ports.Actor, change ports.ChangeRef, path string, line int, body string) (ports.ThreadRef, error) {
	head, err := d.head(as, change)
	if err != nil {
		return "", err
	}
	in := map[string]any{"body": body, "commit_id": head, "path": path}
	if line > 0 {
		in["line"], in["side"] = line, "RIGHT"
	} else {
		in["subject_type"] = "file"
	}
	var c struct {
		ID int64 `json:"id"`
	}
	if err := d.call(as, "POST", "/pulls/"+string(change)+"/comments", in, &c); err != nil {
		return "", err
	}
	ts, err := d.threads(as, change)
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		if len(t.Comments.Nodes) > 0 && t.Comments.Nodes[0].DatabaseID == c.ID {
			return ports.ThreadRef(t.ID), nil
		}
	}
	return "", fmt.Errorf("githubtest: no thread for comment %d", c.ID)
}

func (d *Driver) ReplyInThread(as ports.Actor, change ports.ChangeRef, ref ports.ThreadRef, body string) error {
	t, err := d.thread(as, change, ref)
	if err != nil {
		return err
	}
	if len(t.Comments.Nodes) == 0 {
		return fmt.Errorf("githubtest: thread %s has no comments", ref)
	}
	return d.call(as, "POST", fmt.Sprintf("/pulls/%s/comments/%d/replies", change, t.Comments.Nodes[0].DatabaseID), map[string]any{"body": body}, nil)
}

const (
	resolveMutation   = `mutation ResolveThread($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } } }`
	unresolveMutation = `mutation UnresolveThread($id: ID!) { unresolveReviewThread(input: {threadId: $id}) { thread { id isResolved } } }`
)

func (d *Driver) ResolveThread(as ports.Actor, change ports.ChangeRef, ref ports.ThreadRef) error {
	if _, err := d.thread(as, change, ref); err != nil {
		return err
	}
	return d.graphql(as, resolveMutation, map[string]any{"id": string(ref)}, nil)
}

func (d *Driver) UnresolveThread(as ports.Actor, change ports.ChangeRef, ref ports.ThreadRef) error {
	if _, err := d.thread(as, change, ref); err != nil {
		return err
	}
	return d.graphql(as, unresolveMutation, map[string]any{"id": string(ref)}, nil)
}

// SubmitReview submits a review: state is APPROVE, COMMENT or
// CHANGES_REQUESTED. rec, if set, is a record marker in its body.
func (d *Driver) SubmitReview(as ports.Actor, change ports.ChangeRef, state, body string, rec *ports.Record) (ports.ReviewRef, error) {
	event := map[string]string{"APPROVE": "APPROVE", "COMMENT": "COMMENT", "CHANGES_REQUESTED": "REQUEST_CHANGES"}[state]
	if event == "" {
		return "", fmt.Errorf("githubtest: review state %q: %w", state, ports.ErrConflict)
	}
	if rec != nil {
		body = strings.TrimRight(body, "\n") + "\n\n" + Marker(*rec)
	}
	var r struct {
		ID int64 `json:"id"`
	}
	in := map[string]any{"event": event, "body": body}
	if event != "APPROVE" && strings.TrimSpace(body) == "" {
		// GitHub requires a body for these events, so an empty review is
		// one with an inline comment, as a human submits it (work plan D14).
		c, err := d.inlineComment(as, change)
		if err != nil {
			return "", err
		}
		in["comments"] = []any{c}
	}
	if err := d.call(as, "POST", "/pulls/"+string(change)+"/reviews", in, &r); err != nil {
		return "", err
	}
	return ports.ReviewRef(strconv.FormatInt(r.ID, 10)), nil
}

var newHunkStart = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// inlineComment returns a review comment on the first line the pull
// request's diff adds or keeps.
func (d *Driver) inlineComment(as ports.Actor, change ports.ChangeRef) (map[string]any, error) {
	var fs []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	}
	if err := d.call(as, "GET", "/pulls/"+string(change)+"/files", nil, &fs); err != nil {
		return nil, err
	}
	for _, f := range fs {
		m := newHunkStart.FindStringSubmatch(f.Patch)
		if m == nil || m[2] == "0" {
			continue
		}
		line, _ := strconv.Atoi(m[1])
		return map[string]any{"path": f.Filename, "line": line, "side": "RIGHT", "body": "see here"}, nil
	}
	return nil, fmt.Errorf("githubtest: change %s has no line to comment on: %w", change, ports.ErrConflict)
}

func (d *Driver) DismissReview(as ports.Actor, change ports.ChangeRef, review ports.ReviewRef) error {
	return d.call(as, "PUT", "/pulls/"+string(change)+"/reviews/"+string(review)+"/dismissals",
		map[string]any{"message": "dismissed by the conformance driver", "event": "DISMISS"}, nil)
}

func (d *Driver) MergeChange(as ports.Actor, change ports.ChangeRef) error {
	return d.call(as, "PUT", "/pulls/"+string(change)+"/merge", map[string]any{"merge_method": "squash"}, nil)
}

func (d *Driver) CloseChange(as ports.Actor, change ports.ChangeRef) error {
	return d.call(as, "PATCH", "/pulls/"+string(change), map[string]any{"state": "closed"}, nil)
}

func (d *Driver) ReopenChange(as ports.Actor, change ports.ChangeRef) error {
	return d.call(as, "PATCH", "/pulls/"+string(change), map[string]any{"state": "open"}, nil)
}

// Replies returns the records in a thread, or in the change's conversation
// comments when thread is the change's own ref, each with its author.
func (d *Driver) Replies(change ports.ChangeRef, thread ports.ThreadRef) ([]ports.Record, error) {
	var out []ports.Record
	if thread == ports.ThreadRef(change) {
		var cs []struct {
			Body string `json:"body"`
			User struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"user"`
		}
		if err := d.call(d.Owner, "GET", "/issues/"+string(change)+"/comments?per_page=100", nil, &cs); err != nil {
			return nil, err
		}
		for _, c := range cs {
			if r, ok := parseMarker(c.Body); ok {
				r.Author = actor(c.User.ID, c.User.Login, c.User.Type)
				out = append(out, r)
			}
		}
		return out, nil
	}
	t, err := d.thread(d.Owner, change, thread)
	if err != nil {
		return nil, err
	}
	for _, c := range t.Comments.Nodes {
		if r, ok := parseMarker(c.Body); ok {
			login := c.Author.Login
			if c.Author.Typename == "Bot" {
				login += "[bot]"
			}
			r.Author = actor(c.Author.ID, login, c.Author.Typename)
			out = append(out, r)
		}
	}
	return out, nil
}

func actor(id int64, login, typ string) ports.Actor {
	k := ports.ActorHuman
	if typ == "Bot" {
		k = ports.ActorBot
	}
	return ports.Actor{ID: strconv.FormatInt(id, 10), Login: login, Kind: k}
}

// parseMarker reads the record marker on a body's last non-blank line.
func parseMarker(body string) (ports.Record, bool) {
	body = strings.TrimRight(body, " \t\r\n")
	last := body
	if i := strings.LastIndexAny(body, "\r\n"); i >= 0 {
		last = body[i+1:]
	}
	js, ok := strings.CutPrefix(strings.TrimSpace(last), "<!-- agent-loop:v1 ")
	if !ok {
		return ports.Record{}, false
	}
	js, ok = strings.CutSuffix(js, " -->")
	var head struct {
		Type ports.RecordType `json:"type"`
		Key  ports.IdemKey    `json:"key"`
	}
	if !ok || json.Unmarshal([]byte(js), &head) != nil || head.Type == "" {
		return ports.Record{}, false
	}
	return ports.Record{Type: head.Type, Key: head.Key, Data: json.RawMessage(js)}, true
}

// SetGate reports a gate result on a commit as a commit status. Missing
// can't be set: it is the absence of a result.
func (d *Driver) SetGate(c ports.Commit, name string, s ports.GateState) error {
	state := map[ports.GateState]string{ports.GatePass: "success", ports.GateFail: "failure", ports.GatePending: "pending"}[s]
	if state == "" {
		return fmt.Errorf("githubtest: gate state %q can't be set: %w", s, ports.ErrConflict)
	}
	return d.call(d.Owner, "POST", "/statuses/"+string(c), map[string]any{"state": state, "context": name}, nil)
}

// RequireApprovals sets the base branch's required approving reviews in
// classic protection.
func (d *Driver) RequireApprovals(n int) error {
	return d.call(d.Owner, "PATCH", "/branches/"+url.PathEscape(d.base())+"/protection/required_pull_request_reviews",
		map[string]any{"required_approving_review_count": n}, nil)
}

// The trust actions come with Trust (T11).

func (d *Driver) CreateTag(ports.Actor, string) error { return ErrNotYet }
func (d *Driver) SetRuleset(string, string) error     { return ErrNotYet }
func (d *Driver) AddAdmin(ports.Actor) error          { return ErrNotYet }
func (d *Driver) RemoveAdmin(ports.Actor) error       { return ErrNotYet }

// StartCoderRun records nothing yet: pusher attribution is T11.
func (d *Driver) StartCoderRun(ports.ItemRef) (func(), error) { return func() {}, nil }
