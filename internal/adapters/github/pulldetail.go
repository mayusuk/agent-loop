package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// gqlUser is an Actor as GraphQL returns it. A bot's login has no "[bot]"
// there (work plan D8).
type gqlUser struct {
	Typename   string `json:"__typename"`
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
}

// rest converts it to the REST form, so record author checks and actors
// match the REST ones.
func (a *gqlUser) rest() user {
	if a == nil {
		return user{}
	}
	u := user{ID: a.DatabaseID, Login: a.Login, Type: a.Typename}
	if a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
		u.Login += "[bot]"
	}
	return u
}

func (a *gqlUser) actor() ports.Actor {
	if a == nil {
		return ports.Actor{}
	}
	return a.rest().actor()
}

// tlNode is one pull-request timeline item.
type tlNode struct {
	Typename  string    `json:"__typename"`
	CreatedAt time.Time `json:"createdAt"`
	Actor     *gqlUser  `json:"actor"`
	Author    *gqlUser  `json:"author"`
	Commit    *struct {
		OID           string    `json:"oid"`
		CommittedDate time.Time `json:"committedDate"`
	} `json:"commit"`
	DatabaseID          int64  `json:"databaseId"`
	State               string `json:"state"`
	PreviousReviewState string `json:"previousReviewState"`
	Review              *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"review"`
}

// at is when the item happened: a commit's date for a commit.
func (n tlNode) at() time.Time {
	if n.Typename == "PullRequestCommit" && n.Commit != nil {
		return n.Commit.CommittedDate
	}
	return n.CreatedAt
}

type threadComment struct {
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	URL        string    `json:"url"`
	Author     *gqlUser  `json:"author"`
	Review     *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"pullRequestReview"`
}

type prThread struct {
	ID          string   `json:"id"`
	IsResolved  bool     `json:"isResolved"`
	ResolvedBy  *gqlUser `json:"resolvedBy"`
	Path        string   `json:"path"`
	Line        *int     `json:"line"`
	SubjectType string   `json:"subjectType"`
	Comments    struct {
		Nodes []threadComment `json:"nodes"`
	} `json:"comments"`
}

// where is the thread's location for the status table.
func (t prThread) where() string {
	if t.Line != nil && *t.Line > 0 {
		return t.Path + ":" + strconv.Itoa(*t.Line)
	}
	return t.Path + " (file)"
}

type checkContext struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

// prDetail is what PullDetail reads of one pull request.
type prDetail struct {
	Number     int       `json:"number"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	State      string    `json:"state"`
	Merged     bool      `json:"merged"`
	HeadRefOID string    `json:"headRefOid"`
	Author     *gqlUser  `json:"author"`
	Timeline   struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []tlNode `json:"nodes"`
	} `json:"timelineItems"`
	Threads struct {
		Nodes []prThread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				OID    string `json:"oid"`
				Rollup *struct {
					Contexts struct {
						Nodes []checkContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// contexts are the head commit's check runs and statuses.
func (d *prDetail) contexts() []checkContext {
	if len(d.Commits.Nodes) == 0 || d.Commits.Nodes[0].Commit.Rollup == nil {
		return nil
	}
	return d.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes
}

const actorFields = `__typename login ... on User { databaseId } ... on Bot { databaseId }`

// pullFields is the selection PullDetail reads of each pull request; %s is
// the timeline's skip variable.
const pullFields = `number createdAt updatedAt state merged headRefOid author { ` + actorFields + ` }
      timelineItems(itemTypes: [PULL_REQUEST_COMMIT, PULL_REQUEST_REVIEW, REVIEW_DISMISSED_EVENT, MERGED_EVENT, CLOSED_EVENT, REOPENED_EVENT], first: 100, skip: $%s) {
        totalCount
        nodes {
          __typename
          ... on PullRequestCommit { commit { oid committedDate } }
          ... on PullRequestReview { databaseId createdAt state author { ` + actorFields + ` } }
          ... on ReviewDismissedEvent { createdAt previousReviewState actor { ` + actorFields + ` } review { databaseId } }
          ... on MergedEvent { createdAt actor { ` + actorFields + ` } commit { oid } }
          ... on ClosedEvent { createdAt actor { ` + actorFields + ` } }
          ... on ReopenedEvent { createdAt actor { ` + actorFields + ` } }
        }
      }
      reviewThreads(first: 100) {
        nodes {
          id isResolved resolvedBy { __typename login databaseId } path line subjectType
          comments(first: 100) { nodes { databaseId body createdAt url author { ` + actorFields + ` } pullRequestReview { databaseId } } }
        }
      }
      commits(last: 1) {
        nodes { commit { oid statusCheckRollup { contexts(first: 100) { nodes {
          __typename ... on CheckRun { name status conclusion } ... on StatusContext { context state }
        } } } } }
      }`

// pullDetailQuery reads k pull requests, $n0... with timelines from $s0...
func pullDetailQuery(k int) string {
	var vars, body strings.Builder
	vars.WriteString("$owner: String!, $name: String!")
	for i := 0; i < k; i++ {
		fmt.Fprintf(&vars, ", $n%d: Int!, $s%d: Int!", i, i)
		fmt.Fprintf(&body, "    p%d: pullRequest(number: $n%d) {\n      %s\n    }\n", i, i, fmt.Sprintf(pullFields, "s"+strconv.Itoa(i)))
	}
	return "query PullDetail(" + vars.String() + ") {\n  repository(owner: $owner, name: $name) {\n" + body.String() + "  }\n}"
}

// detailBatch is how many pull requests one PullDetail query reads.
const detailBatch = 10

// details reads the pull requests' detail, each timeline from its skip to
// the end, in batched GraphQL queries (§9.4).
func (h *Host) details(ctx context.Context, nums []int, skips map[int]int) (map[int]*prDetail, error) {
	out := map[int]*prDetail{}
	for lo := 0; lo < len(nums); lo += detailBatch {
		batch := nums[lo:min(lo+detailBatch, len(nums))]
		vars := map[string]any{"owner": h.c.Owner(), "name": h.c.Name()}
		for i, n := range batch {
			vars["n"+strconv.Itoa(i)], vars["s"+strconv.Itoa(i)] = n, skips[n]
		}
		var res struct {
			Repository map[string]*prDetail `json:"repository"`
		}
		if err := h.c.GraphQL(ctx, asLoop, pullDetailQuery(len(batch)), vars, &res); err != nil {
			return nil, err
		}
		for i, n := range batch {
			d := res.Repository["p"+strconv.Itoa(i)]
			if d == nil {
				return nil, fmt.Errorf("github: pull request %d: %w", n, ports.ErrNotFound)
			}
			out[n] = d
		}
	}
	// Read the rest of any timeline longer than one page.
	for _, n := range nums {
		d := out[n]
		for got := skips[n] + len(d.Timeline.Nodes); got < d.Timeline.TotalCount; {
			vars := map[string]any{"owner": h.c.Owner(), "name": h.c.Name(), "n0": n, "s0": got}
			var res struct {
				Repository map[string]*prDetail `json:"repository"`
			}
			if err := h.c.GraphQL(ctx, asLoop, pullDetailQuery(1), vars, &res); err != nil {
				return nil, err
			}
			more := res.Repository["p0"]
			if more == nil || len(more.Timeline.Nodes) == 0 {
				break
			}
			d.Timeline.Nodes = append(d.Timeline.Nodes, more.Timeline.Nodes...)
			got += len(more.Timeline.Nodes)
		}
	}
	return out, nil
}

func (h *Host) detail(ctx context.Context, n int) (*prDetail, error) {
	ds, err := h.details(ctx, []int{n}, nil)
	if err != nil {
		return nil, err
	}
	return ds[n], nil
}

// findingOf returns the finding ID of a reviewer finding comment.
func (h *Host) findingOf(c threadComment) (ports.FindingID, bool) {
	r, ok := h.ids.recordIn(c.Body, c.Author.rest(), c.CreatedAt, "", ports.RecordFinding)
	if !ok {
		return "", false
	}
	var d struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(r.Data, &d) != nil || d.ID == "" {
		return "", false
	}
	return ports.FindingID(d.ID), true
}

// findingThreads maps each finding the reviewer posted with a thread to its
// thread and the review it was posted with (work plan D5).
func (h *Host) findingThreads(d *prDetail) map[ports.FindingID]findingThread {
	out := map[ports.FindingID]findingThread{}
	for _, t := range d.Threads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		first := t.Comments.Nodes[0]
		if first.Author == nil || strconv.FormatInt(first.Author.DatabaseID, 10) != h.ids.Reviewer.ID {
			continue
		}
		id, ok := h.findingOf(first)
		if !ok {
			continue
		}
		if _, dup := out[id]; dup {
			continue // the first one posted is the finding's thread
		}
		ft := findingThread{thread: t, url: first.URL}
		if first.Review != nil {
			ft.review = first.Review.DatabaseID
		}
		out[id] = ft
	}
	return out
}

type findingThread struct {
	thread prThread
	review int64
	url    string
}
