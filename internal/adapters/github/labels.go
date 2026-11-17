package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// labelMaps returns the marker-to-label map, DefaultLabels overridden by
// custom, and its inverse by lower-case label name. The tracker and the
// host share it (work plan #15 D18).
func labelMaps(custom map[ports.Marker]string) (map[ports.Marker]string, map[string]ports.Marker, error) {
	labels := map[ports.Marker]string{}
	markers := map[string]ports.Marker{}
	for m, l := range DefaultLabels {
		labels[m] = l
	}
	for m, l := range custom {
		if l != "" {
			labels[m] = l
		}
	}
	for m, l := range labels {
		if other, dup := markers[strings.ToLower(l)]; dup {
			return nil, nil, fmt.Errorf("github: label %q maps both %s and %s", l, other, m)
		}
		markers[strings.ToLower(l)] = m
	}
	return labels, markers, nil
}

// setIssueLabel adds or removes a label on an issue or pull request as the
// loop identity. Removing a label that is already gone is not an error.
func setIssueLabel(ctx context.Context, c *Client, n int, name string, on bool) error {
	if on {
		return c.Send(ctx, asLoop, http.MethodPost, c.RepoPath("/issues/%d/labels", n), map[string]any{"labels": []string{name}}, nil)
	}
	err := c.Send(ctx, asLoop, http.MethodDelete, c.RepoPath("/issues/%d/labels/%s", n, url.PathEscape(name)), nil, nil)
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	return err
}

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }
