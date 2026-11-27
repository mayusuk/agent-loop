package githubtest

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// listDir answers the contents API for a directory: its files and
// subdirectories, one level deep, by name.
func (s *Server) listDir(c *commit, dir string) response {
	dir = strings.Trim(dir, "/")
	entries := map[string]map[string]any{}
	for p, content := range c.files {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		name, _, sub := strings.Cut(rest, "/")
		if _, seen := entries[name]; seen {
			continue
		}
		e := map[string]any{"name": name, "path": dir + "/" + name, "type": "file", "size": len(content), "sha": blobSHA(content)}
		if sub {
			e["type"], e["size"] = "dir", 0
		}
		entries[name] = e
	}
	if len(entries) == 0 {
		return errorResponse(http.StatusNotFound, "Not Found")
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, entries[n])
	}
	return response{body: out}
}

// subtree returns a commit's files at or under path.
func subtree(c *commit, path string) map[string]string {
	out := map[string]string{}
	for p, v := range c.files {
		if p == path || strings.HasPrefix(p, path+"/") {
			out[p] = v
		}
	}
	return out
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// touches reports whether c changed path, with the default history
// simplification of a path-filtered log: a merge counts only if path
// differs from every parent.
func (s *Server) touches(c *commit, path string) bool {
	mine := subtree(c, path)
	if len(c.parents) == 0 {
		return len(mine) > 0
	}
	for _, p := range c.parents {
		if sameFiles(mine, subtree(s.commits[p], path)) {
			return false
		}
	}
	return true
}

// listCommits answers GET /commits with sha and path, newest first.
func (s *Server) listCommits(_ *User, _ []string, q url.Values, _ map[string]any) response {
	ref := q.Get("sha")
	if ref == "" {
		ref = s.defaultBranch
	}
	tip := s.resolve(ref)
	if tip == nil {
		return errorResponse(http.StatusNotFound, "No commit found for SHA: "+ref)
	}
	path := strings.Trim(q.Get("path"), "/")
	var cs []*commit
	for x := range s.ancestors(tip.sha) {
		if c := s.commits[x]; path == "" || s.touches(c, path) {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].seq > cs[j].seq })
	list := []any{}
	for _, c := range cs {
		list = append(list, map[string]any{"sha": c.sha, "commit": map[string]any{
			"committer": map[string]any{"date": c.at.Format(time.RFC3339)}}})
	}
	return response{list: list}
}

// commitPulls answers GET /commits/{sha}/pulls: the pull requests whose
// merge commit is sha or whose commits include it. As on GitHub, the list
// doesn't carry merged_by.
func (s *Server) commitPulls(_ *User, m []string, _ url.Values, _ map[string]any) response {
	sha := m[1]
	var nums []int
	for n, it := range s.issues {
		if !it.pull || it.pr == nil {
			continue
		}
		hit := it.pr.mergedSHA == sha
		for _, tl := range it.pr.timeline {
			hit = hit || (tl.kind == "PullRequestCommit" && tl.commit == sha)
		}
		if hit {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	list := []any{}
	for _, n := range nums {
		pj := s.pullJSON(s.issues[n])
		pj["merged_by"] = nil
		list = append(list, pj)
	}
	return response{list: list}
}
