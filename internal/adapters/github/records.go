package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// The record marker of INTEGRATION-SPEC §6.1, as contract/ writes it.
const (
	markerPrefix = "<!-- agent-loop:v1 "
	markerSuffix = " -->"
)

// recordAuthor is the identity allowed to write each record type (§6.1,
// work plan D9).
var recordAuthor = map[ports.RecordType]ports.Identity{
	ports.RecordChange:          ports.IdentityCoder,
	ports.RecordReview:          ports.IdentityReviewer,
	ports.RecordFinding:         ports.IdentityReviewer,
	ports.RecordReply:           ports.IdentityCoder,
	ports.RecordManualReply:     ports.IdentityCoder,
	ports.RecordStatus:          ports.IdentityLoop,
	ports.RecordApprovedCommits: ports.IdentityLoop,
	ports.RecordFollowUp:        ports.IdentityLoop,
	ports.RecordTask:            ports.IdentityLoop,
	ports.RecordPlan:            ports.IdentityLoop,
	ports.RecordProposal:        ports.IdentityCoder,
}

// Identities are the loop's three identities as GitHub reports them: the
// numeric ID of each app's bot user (work plan D8).
type Identities struct {
	Coder, Reviewer, Loop ports.Actor
}

// Actor returns the actor of an identity.
func (ids Identities) Actor(id ports.Identity) ports.Actor {
	switch id {
	case ports.IdentityCoder:
		return ids.Coder
	case ports.IdentityReviewer:
		return ids.Reviewer
	case ports.IdentityLoop:
		return ids.Loop
	}
	return ports.Actor{}
}

// Counts reports whether a record of type t by author counts (§6.1, C3).
func (ids Identities) Counts(t ports.RecordType, author ports.Actor) bool {
	id, ok := recordAuthor[t]
	if !ok {
		return false
	}
	want := ids.Actor(id)
	return want.ID != "" && author.ID == want.ID
}

// encodeMarker returns the marker line for rec with key. The record's Data
// is its JSON object; its "type" and "key" are set from rec.Type and key.
func encodeMarker(rec ports.Record, key ports.IdemKey) (string, error) {
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(rec.Data)) > 0 {
		if err := json.Unmarshal(rec.Data, &fields); err != nil {
			return "", fmt.Errorf("github: record data is not a JSON object: %v: %w", err, ports.ErrConflict)
		}
	}
	if rec.Type == "" {
		return "", fmt.Errorf("github: record has no type: %w", ports.ErrConflict)
	}
	if key == "" {
		return "", fmt.Errorf("github: record has no key: %w", ports.ErrConflict)
	}
	t, _ := json.Marshal(rec.Type)
	k, _ := json.Marshal(key)
	fields["type"], fields["key"] = t, k
	if _, ok := fields["v"]; !ok {
		fields["v"] = json.RawMessage("1")
	}
	// json.Marshal compacts the values and escapes <, > and &, so the JSON
	// can't close the HTML comment early.
	b, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("github: record data: %v: %w", err, ports.ErrConflict)
	}
	return markerPrefix + string(b) + markerSuffix, nil
}

// withMarker returns body with the marker as its last line.
func withMarker(body, marker string) string {
	body = strings.TrimRight(body, " \t\r\n")
	if body == "" {
		return marker
	}
	return body + "\n\n" + marker
}

// findMarker reads the record on a body's last non-blank line. Only that
// line counts (contract D14).
func findMarker(body string) (ports.Record, bool) {
	body = strings.TrimRight(body, " \t\r\n")
	last := body
	if i := strings.LastIndexAny(body, "\r\n"); i >= 0 {
		last = body[i+1:]
	}
	last = strings.TrimSpace(last)
	if !strings.HasPrefix(last, markerPrefix) || !strings.HasSuffix(last, markerSuffix) {
		return ports.Record{}, false
	}
	js := strings.TrimSpace(last[len(markerPrefix) : len(last)-len(markerSuffix)])
	var head struct {
		Type ports.RecordType `json:"type"`
		Key  ports.IdemKey    `json:"key"`
	}
	if !strings.HasPrefix(js, "{") || json.Unmarshal([]byte(js), &head) != nil || head.Type == "" || head.Key == "" {
		return ports.Record{}, false
	}
	if _, ok := recordAuthor[head.Type]; !ok {
		return ports.Record{}, false
	}
	return ports.Record{Type: head.Type, Key: head.Key, Data: json.RawMessage(js)}, true
}

// user is a GitHub account as the REST API returns it.
type user struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // "User", "Bot", "Organization"
}

// actor converts a GitHub account; ID is the numeric account ID (I13).
func (u user) actor() ports.Actor {
	k := ports.ActorHuman
	if u.Type == "Bot" {
		k = ports.ActorBot
	}
	return ports.Actor{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Kind: k}
}

// recordIn returns the record in a body written by author, if it is of type
// t (any type when t is empty) and author may write it.
func (ids Identities) recordIn(body string, author user, at time.Time, ref ports.CommentRef, t ports.RecordType) (ports.Record, bool) {
	r, ok := findMarker(body)
	if !ok || (t != "" && r.Type != t) {
		return ports.Record{}, false
	}
	a := author.actor()
	if !ids.Counts(r.Type, a) {
		return ports.Record{}, false
	}
	r.Author, r.Ref, r.At = a, ref, at
	return r, true
}
