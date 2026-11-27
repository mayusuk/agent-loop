package githubtest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// Driver acts in a GitHub repository (the stub, or a live sandbox) as a
// given account, with that account's token. It implements
// conformance.Driver.
type Driver struct {
	BaseURL string // the stub's URL, or https://api.github.com
	Repo    string // owner/name
	// Tokens maps an account's ID to its token. Set it before the
	// Driver is shared between goroutines; after that, add tokens with
	// SetToken.
	Tokens map[string]string
	// Labels maps markers to labels; DefaultLabels fills the rest.
	Labels map[ports.Marker]string
	HTTP   *http.Client
	// Base is the base branch ("main" when empty).
	Base string
	// Owner is the account that changes repository settings.
	Owner ports.Actor
	// Runs records the coder runs StartCoderRun starts. Nil records none.
	Runs *RunLog
	// RulesetNames maps the conformance suite's ruleset names
	// (RulesetLoopBranches and so on) to the repository's, for a sandbox
	// whose rulesets `agent-loop init` created (github.RulesetNameLoop and
	// so on). A name it leaves out is used as it is.
	RulesetNames map[string]string

	pushes  int
	removed map[string]map[string]any // rulesets removed by SetRuleset, to restore

	tokMu sync.RWMutex // guards Tokens once the Driver is shared
}

// SetToken adds or replaces the token for account ID id. It is safe to
// call while other goroutines make calls with the Driver.
func (d *Driver) SetToken(id, token string) {
	d.tokMu.Lock()
	defer d.tokMu.Unlock()
	if d.Tokens == nil {
		d.Tokens = map[string]string{}
	}
	d.Tokens[id] = token
}

func (d *Driver) token(id string) (string, bool) {
	d.tokMu.RLock()
	defer d.tokMu.RUnlock()
	tok, ok := d.Tokens[id]
	return tok, ok
}

func (d *Driver) label(m ports.Marker) string {
	if l, ok := d.Labels[m]; ok {
		return l
	}
	return github.DefaultLabels[m]
}

func (d *Driver) call(as ports.Actor, method, path string, in, out any) error {
	return d.callRepo(as, d.Repo, method, path, in, out)
}

// Call makes a REST call on the repository as the account, for settings
// the Driver has no action for. path is relative to /repos/{owner}/{name}.
func (d *Driver) Call(as ports.Actor, method, path string, in, out any) error {
	return d.call(as, method, path, in, out)
}

// callRepo calls path on repository repo (another account's fork, say).
func (d *Driver) callRepo(as ports.Actor, repo, method, path string, in, out any) error {
	tok, ok := d.token(as.ID)
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
	u := strings.TrimRight(d.BaseURL, "/") + "/repos/" + repo + path
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
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnauthorized,
		// A ruleset refusal, whatever status the endpoint gives it (work
		// plan #16 D11: 403 in the stub; GitHub may answer 405, 409 or 422).
		res.StatusCode >= 400 && bytes.Contains(data, []byte("Repository rule violations")):
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
	return d.tipIn(as, d.Repo, branch)
}

func (d *Driver) tipIn(as ports.Actor, repo, branch string) (string, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := d.callRepo(as, repo, "GET", "/branches/"+url.PathEscape(branch), nil, &b)
	return b.Commit.SHA, err
}

// Push commits one change to ChangedFile on branch (BaseFile on the base),
// creating the branch from the base tip if it doesn't exist. A branch
// "<login>:<name>" is in that account's fork, which is created first.
func (d *Driver) Push(as ports.Actor, branch string) (ports.Commit, error) {
	repo := d.Repo
	if owner, name, fork := strings.Cut(branch, ":"); fork {
		if owner != as.Login {
			return "", fmt.Errorf("githubtest: %s can't push to %s's fork: %w", as.Login, owner, ports.ErrPermission)
		}
		if err := d.call(as, "POST", "/forks", map[string]any{}, nil); err != nil {
			return "", err
		}
		_, n, _ := strings.Cut(d.Repo, "/")
		repo, branch = owner+"/"+n, name
	}
	if _, err := d.tipIn(as, repo, branch); err != nil {
		base, err := d.tipIn(as, repo, d.base())
		if err != nil {
			return "", err
		}
		if err := d.callRepo(as, repo, "POST", "/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": base}, nil); err != nil {
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
	if err := d.callRepo(as, repo, "GET", "/contents/"+file+"?ref="+url.QueryEscape(branch), nil, &cur); err == nil {
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
	if err := d.callRepo(as, repo, "PUT", "/contents/"+file, in, &out); err != nil {
		return "", err
	}
	return ports.Commit(out.Commit.SHA), nil
}

// PushFiles commits each of files to branch through the contents API, one
// commit per file in path order, creating the branch from the base tip if
// it doesn't exist. It returns the last commit. Forks aren't supported.
func (d *Driver) PushFiles(as ports.Actor, branch string, files map[string]string) (ports.Commit, error) {
	if _, err := d.tip(as, branch); err != nil {
		base, err := d.tip(as, d.base())
		if err != nil {
			return "", err
		}
		if err := d.call(as, "POST", "/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": base}, nil); err != nil {
			return "", err
		}
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var last ports.Commit
	for _, p := range paths {
		in := map[string]any{"branch": branch, "message": "conformance push " + p,
			"content": base64.StdEncoding.EncodeToString([]byte(files[p]))}
		var cur struct {
			SHA string `json:"sha"`
		}
		if err := d.call(as, "GET", "/contents/"+p+"?ref="+url.QueryEscape(branch), nil, &cur); err == nil && cur.SHA != "" {
			in["sha"] = cur.SHA
		}
		var out struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := d.call(as, "PUT", "/contents/"+p, in, &out); err != nil {
			return "", err
		}
		last = ports.Commit(out.Commit.SHA)
	}
	return last, nil
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

// CreateTag creates a tag at the base tip.
func (d *Driver) CreateTag(as ports.Actor, name string) error {
	sha, err := d.tip(as, d.base())
	if err != nil {
		return err
	}
	return d.call(as, "POST", "/git/refs", map[string]any{"ref": "refs/tags/" + name, "sha": sha}, nil)
}

// ruleset finds the repository ruleset named r, with its full body.
func (d *Driver) ruleset(r string) (map[string]any, error) {
	if n, ok := d.RulesetNames[r]; ok {
		r = n
	}
	var list []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := d.call(d.Owner, "GET", "/rulesets", nil, &list); err != nil {
		return nil, err
	}
	for _, x := range list {
		if x.Name == r {
			var full map[string]any
			err := d.call(d.Owner, "GET", "/rulesets/"+strconv.FormatInt(x.ID, 10), nil, &full)
			return full, err
		}
	}
	return nil, fmt.Errorf("githubtest: no ruleset %q: %w", r, ports.ErrNotFound)
}

// rulesetBody is the part of a ruleset that creating or updating it takes.
func rulesetBody(full map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"name", "target", "enforcement", "conditions", "rules", "bypass_actors"} {
		if v, ok := full[k]; ok {
			out[k] = v
		}
	}
	return out
}

// SetRuleset changes the ruleset named r, as the owner: "change" adds a
// rule, "disable" and "enable" set its enforcement, "add_bypass" adds an
// app to its bypass list, "remove" deletes it and "restore" creates it
// again as it was.
func (d *Driver) SetRuleset(r, a string) error {
	if a == "restore" {
		body, ok := d.removed[r]
		if !ok {
			return fmt.Errorf("githubtest: ruleset %q wasn't removed: %w", r, ports.ErrConflict)
		}
		delete(d.removed, r)
		return d.call(d.Owner, "POST", "/rulesets", body, nil)
	}
	full, err := d.ruleset(r)
	if err != nil {
		return err
	}
	id, _ := full["id"].(float64)
	path := "/rulesets/" + strconv.FormatInt(int64(id), 10)
	body := rulesetBody(full)
	switch a {
	case "change":
		rules, _ := body["rules"].([]any)
		body["rules"] = append(rules, map[string]any{"type": "required_linear_history"})
	case "disable":
		body["enforcement"] = "disabled"
	case "enable":
		body["enforcement"] = "active"
	case "add_bypass":
		// An app no rule of §9.8 step 1 allows (#101).
		by, _ := body["bypass_actors"].([]any)
		body["bypass_actors"] = append(by, map[string]any{"actor_type": "Integration", "actor_id": 777777, "bypass_mode": "always"})
	case "remove":
		if d.removed == nil {
			d.removed = map[string]map[string]any{}
		}
		d.removed[r] = body
		return d.call(d.Owner, "DELETE", path, nil, nil)
	default:
		return fmt.Errorf("githubtest: ruleset action %q: %w", a, ports.ErrConflict)
	}
	return d.call(d.Owner, "PUT", path, body, nil)
}

// AddAdmin gives the account admin permission on the repository.
func (d *Driver) AddAdmin(a ports.Actor) error {
	return d.call(d.Owner, "PUT", "/collaborators/"+url.PathEscape(a.Login), map[string]any{"permission": "admin"}, nil)
}

// RemoveAdmin leaves the account with write permission.
func (d *Driver) RemoveAdmin(a ports.Actor) error {
	return d.call(d.Owner, "PUT", "/collaborators/"+url.PathEscape(a.Login), map[string]any{"permission": "push"}, nil)
}

// StartCoderRun records a coder run for the item in Runs until end is
// called.
func (d *Driver) StartCoderRun(item ports.ItemRef) (func(), error) {
	if d.Runs == nil {
		return func() {}, nil
	}
	return d.Runs.Start(item), nil
}
