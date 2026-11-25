package githubtest

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var opName = regexp.MustCompile(`^\s*(query|mutation)\s+(\w+)`)

func gqlError(typ, msg string) response {
	return response{body: map[string]any{"data": nil, "errors": []any{map[string]any{"type": typ, "message": msg}}}}
}

func gqlData(data map[string]any) response { return response{body: map[string]any{"data": data}} }

// graphqlHost answers the code host's named GraphQL operations. ok is false
// for other queries.
func (s *Server) graphqlHost(u *User, query string, vars map[string]any) (response, bool) {
	m := opName.FindStringSubmatch(query)
	if m == nil {
		return response{}, false
	}
	switch m[2] {
	case "PullDetail":
		return s.gqlPullDetail(vars), true
	case "DriverThreads":
		n, _ := vars["number"].(float64)
		it := s.issues[int(n)]
		if it == nil || !it.pull || it.pr == nil {
			return gqlError("NOT_FOUND", "Could not resolve to a PullRequest"), true
		}
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": s.pullNode(it, 0)}}), true
	case "ResolveThread", "UnresolveThread":
		id, _ := vars["id"].(string)
		t := s.threadByID(id)
		if t == nil {
			return gqlError("NOT_FOUND", "Could not resolve to a node with the global id of '"+id+"'"), true
		}
		// GitHub checks the app installation's permissions here, not the
		// token's: an app installed without contents: write is refused
		// (measured, #100).
		if p, ok := s.installPerms(u); ok && p["contents"] != "write" {
			return gqlError("FORBIDDEN", "Resource not accessible by integration"), true
		}
		on := m[2] == "ResolveThread"
		if t.resolved != on {
			t.resolved = on
			t.resolvedBy = nil
			if on {
				t.resolvedBy = u
			}
		}
		field := "resolveReviewThread"
		if !on {
			field = "unresolveReviewThread"
		}
		return gqlData(map[string]any{field: map[string]any{"thread": map[string]any{"id": t.id, "isResolved": t.resolved}}}), true
	case "PullClosing":
		n, _ := vars["number"].(float64)
		it := s.issues[int(n)]
		if it == nil || !it.pull {
			return gqlError("NOT_FOUND", "Could not resolve to a PullRequest"), true
		}
		nodes := []any{}
		for _, c := range it.closes {
			nodes = append(nodes, map[string]any{"number": c})
		}
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"closingIssuesReferences": map[string]any{"nodes": nodes}}}}), true
	case "ClosingRefs":
		n, _ := vars["number"].(float64)
		it := s.issues[int(n)]
		if it == nil || it.pull {
			return gqlError("NOT_FOUND", "Could not resolve to an Issue"), true
		}
		var nums []int
		for num, p := range s.issues {
			if !p.pull || p.state != "open" {
				continue
			}
			for _, c := range p.closes {
				if c == it.number {
					nums = append(nums, num)
				}
			}
		}
		sort.Ints(nums)
		nodes := []any{}
		for _, num := range nums {
			nodes = append(nodes, map[string]any{"number": num, "state": "OPEN"})
		}
		return gqlData(map[string]any{"repository": map[string]any{"issue": map[string]any{
			"closedByPullRequestsReferences": map[string]any{"nodes": nodes}}}}), true
	}
	return gqlError("UNSUPPORTED", "githubtest: unsupported operation "+m[2]), true
}

func (s *Server) threadByID(id string) *thread {
	for _, it := range s.issues {
		if it.pr == nil {
			continue
		}
		for _, t := range it.pr.threads {
			if t.id == id {
				return t
			}
		}
	}
	return nil
}

// gqlPullDetail answers PullDetail: pull requests $n0, $n1, ... as p0,
// p1, ..., each with its timeline from $s0, $s1, ...
func (s *Server) gqlPullDetail(vars map[string]any) response {
	repo := map[string]any{}
	for i := 0; ; i++ {
		k := strconv.Itoa(i)
		n, ok := vars["n"+k].(float64)
		if !ok {
			break
		}
		skip, _ := vars["s"+k].(float64)
		it := s.issues[int(n)]
		if it == nil || !it.pull || it.pr == nil {
			return gqlError("NOT_FOUND", "Could not resolve to a PullRequest with the number of "+strconv.Itoa(int(n)))
		}
		repo["p"+k] = s.pullNode(it, int(skip))
	}
	return gqlData(map[string]any{"repository": repo})
}

// gqlActor is an Actor as GraphQL returns it: a bot's login has no "[bot]".
func gqlActor(u *User) any {
	if u == nil {
		return nil
	}
	return map[string]any{"__typename": u.Type, "login": strings.TrimSuffix(u.Login, "[bot]"), "databaseId": u.ID}
}

const timelinePage = 100

func (s *Server) pullNode(it *issue, skip int) map[string]any {
	state := "OPEN"
	switch {
	case it.mergedAt != nil:
		state = "MERGED"
	case it.state == "closed":
		state = "CLOSED"
	}
	tl := it.pr.timeline
	lo := min(max(skip, 0), len(tl))
	hi := min(lo+timelinePage, len(tl))
	nodes := []any{}
	for _, x := range tl[lo:hi] {
		n := map[string]any{"__typename": x.kind, "createdAt": x.at.Format(time.RFC3339)}
		switch x.kind {
		case "PullRequestCommit":
			n = map[string]any{"__typename": x.kind, "commit": map[string]any{"oid": x.commit, "committedDate": x.at.Format(time.RFC3339)}}
		case "PullRequestReview":
			n["databaseId"], n["state"], n["author"], n["submittedAt"] = x.review.id, x.review.state, gqlActor(x.actor), x.at.Format(time.RFC3339)
			if x.review.state == "DISMISSED" {
				n["state"] = "DISMISSED"
			}
		case "ReviewDismissedEvent":
			n["actor"], n["previousReviewState"], n["review"] = gqlActor(x.actor), x.prevState, map[string]any{"databaseId": x.review.id}
		case "MergedEvent":
			n["actor"], n["commit"] = gqlActor(x.actor), map[string]any{"oid": x.commit}
		default:
			n["actor"] = gqlActor(x.actor)
		}
		nodes = append(nodes, n)
	}
	threads := []any{}
	for _, t := range it.pr.threads {
		cs := []any{}
		for _, c := range t.comments {
			cs = append(cs, map[string]any{"databaseId": c.id, "body": c.body, "createdAt": c.at.Format(time.RFC3339),
				"url":    "https://github.com/" + s.Owner + "/" + s.Name + "/pull/" + strconv.Itoa(it.number) + "#discussion_r" + strconv.FormatInt(c.id, 10),
				"author": gqlActor(c.user), "pullRequestReview": map[string]any{"databaseId": c.review}})
		}
		var line any
		subject := "FILE"
		if t.line > 0 {
			line, subject = t.line, "LINE"
		}
		var by any
		if t.resolvedBy != nil {
			// resolvedBy is a User in GitHub's schema.
			by = map[string]any{"__typename": "User", "login": t.resolvedBy.Login, "databaseId": t.resolvedBy.ID}
		}
		threads = append(threads, map[string]any{"id": t.id, "isResolved": t.resolved, "resolvedBy": by, "path": t.path,
			"line": line, "subjectType": subject, "comments": map[string]any{"nodes": cs}})
	}
	var rollup any
	if ctxs := s.checkContexts(it.pr.headSHA); len(ctxs) > 0 {
		rollup = map[string]any{"contexts": map[string]any{"nodes": ctxs}}
	}
	return map[string]any{
		"number": it.number, "createdAt": it.createdAt.Format(time.RFC3339), "updatedAt": it.updatedAt.Format(time.RFC3339),
		"state": state, "merged": it.mergedAt != nil, "headRefOid": it.pr.headSHA, "author": gqlActor(it.user),
		"timelineItems": map[string]any{"totalCount": len(tl), "nodes": nodes},
		"reviewThreads": map[string]any{"nodes": threads},
		"commits":       map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": it.pr.headSHA, "statusCheckRollup": rollup}}}},
	}
}

// checkContexts is a commit's statusCheckRollup contexts: the latest check
// run of each name and the latest status of each context.
func (s *Server) checkContexts(sha string) []any {
	out := []any{}
	seen := map[string]bool{}
	runs := s.checkRuns[sha]
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if seen[r.name] {
			continue
		}
		seen[r.name] = true
		var concl any
		if r.conclusion != "" {
			concl = strings.ToUpper(r.conclusion)
		}
		out = append(out, map[string]any{"__typename": "CheckRun", "name": r.name, "status": strings.ToUpper(r.status), "conclusion": concl})
	}
	for _, st := range s.latestStatuses(sha) {
		out = append(out, map[string]any{"__typename": "StatusContext", "context": st.context, "state": strings.ToUpper(st.state)})
	}
	return out
}
