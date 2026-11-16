package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// AddDependency records that item is blocked by blockedBy with GitHub's
// native issue dependency (§9.5, I3). An existing dependency is left alone.
func (t *Tracker) AddDependency(ctx context.Context, ref, blockedBy ports.ItemRef, _ ports.IdemKey) error {
	it, err := t.issue(ctx, ref)
	if err != nil {
		return err
	}
	blocker, err := t.issue(ctx, blockedBy)
	if err != nil {
		return err
	}
	if it.Number == blocker.Number {
		return fmt.Errorf("github: item %s can't block itself: %w", ref, ports.ErrConflict)
	}
	deps, err := t.blockedBy(ctx, it.Number)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.Number == blocker.Number {
			return nil
		}
	}
	err = t.c.Send(ctx, asLoop, http.MethodPost, t.c.RepoPath("/issues/%d/dependencies/blocked_by", it.Number), map[string]any{"issue_id": blocker.ID}, nil)
	if isStatus(err, http.StatusUnprocessableEntity) {
		// A concurrent or earlier add: check again before failing.
		if deps, rerr := t.blockedBy(ctx, it.Number); rerr == nil {
			for _, d := range deps {
				if d.Number == blocker.Number {
					return nil
				}
			}
		}
	}
	return err
}

func (t *Tracker) blockedBy(ctx context.Context, n int) ([]issue, error) {
	return List[issue](ctx, t.c, asLoop, t.c.RepoPath("/issues/%d/dependencies/blocked_by", n), nil)
}

// Dependencies returns the issues that block ref, each with its close
// reason, closer and whether its linked change merged (§9.5, work plan D6,
// D7). The core decides which are done (FR-2 AC6).
func (t *Tracker) Dependencies(ctx context.Context, ref ports.ItemRef) ([]ports.Dependency, error) {
	it, err := t.issue(ctx, ref)
	if err != nil {
		return nil, err
	}
	deps, err := t.blockedBy(ctx, it.Number)
	if err != nil {
		return nil, err
	}
	var out []ports.Dependency
	for _, d := range deps {
		dref := ports.ItemRef(strconv.Itoa(d.Number))
		full, err := t.issue(ctx, dref) // the list may leave out closed_by
		if err != nil {
			return nil, err
		}
		v := t.view(full)
		merged, err := t.changeMerged(ctx, full.Number)
		if err != nil {
			return nil, err
		}
		out = append(out, ports.Dependency{Ref: dref, CloseReason: v.CloseReason, ClosedBy: v.ClosedBy, ChangeMerged: merged})
	}
	return out, nil
}

// pull is a pull request as the REST list returns it.
type pull struct {
	Number   int        `json:"number"`
	Body     string     `json:"body"`
	User     user       `json:"user"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// maxBranches bounds the <prefix><item>-<n> names changeMerged tries.
const maxBranches = 50

const closersQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      closedByPullRequestsReferences(first: 25, includeClosedPrs: true) { nodes { number merged } }
      timelineItems(itemTypes: [CLOSED_EVENT], last: 1) {
        nodes { ... on ClosedEvent { closer { ... on PullRequest { number merged } } } }
      }
    }
  }
}`

// changeMerged reports whether a merged pull request is linked to issue n
// (§6.2, §9.5, work plan D7).
func (t *Tracker) changeMerged(ctx context.Context, n int) (bool, error) {
	var res struct {
		Repository struct {
			Issue struct {
				Closed struct {
					Nodes []struct {
						Merged bool `json:"merged"`
					} `json:"nodes"`
				} `json:"closedByPullRequestsReferences"`
				Timeline struct {
					Nodes []struct {
						Closer *struct {
							Merged bool `json:"merged"`
						} `json:"closer"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": t.c.Owner(), "name": t.c.Name(), "number": n}
	if err := t.c.GraphQL(ctx, asLoop, closersQuery, vars, &res); err != nil {
		return false, err
	}
	for _, p := range res.Repository.Issue.Closed.Nodes {
		if p.Merged {
			return true, nil
		}
	}
	for _, e := range res.Repository.Issue.Timeline.Nodes {
		if e.Closer != nil && e.Closer.Merged {
			return true, nil
		}
	}
	ref := strconv.Itoa(n)
	for i := 1; i <= maxBranches; i++ {
		branch := t.prefix + ref
		if i > 1 {
			branch += "-" + strconv.Itoa(i)
		}
		q := url.Values{"state": {"all"}, "head": {t.c.Owner() + ":" + branch}}
		pulls, err := List[pull](ctx, t.c, asLoop, t.c.RepoPath("/pulls"), q)
		if err != nil {
			return false, err
		}
		if len(pulls) == 0 {
			return false, nil
		}
		for _, p := range pulls {
			if p.MergedAt == nil || p.Head.Ref != branch {
				continue
			}
			r, ok := t.ids.recordIn(p.Body, p.User, time.Time{}, "", ports.RecordChange)
			if !ok {
				continue
			}
			var d struct {
				Item string `json:"item"`
			}
			if json.Unmarshal(r.Data, &d) == nil && d.Item == ref {
				return true, nil
			}
		}
	}
	return false, nil
}
