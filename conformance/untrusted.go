package conformance

import (
	"reflect"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

var untrustedType = reflect.TypeOf(ports.UntrustedText{})

// leaks returns the paths in v where a plain string or byte slice contains
// canary. Untrusted text is skipped: it is the only allowed carrier.
func leaks(v reflect.Value, path, canary string, out *[]string) {
	if !v.IsValid() {
		return
	}
	if v.Type() == untrustedType {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), canary) {
			*out = append(*out, path)
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if v.Kind() == reflect.Slice && strings.Contains(string(v.Bytes()), canary) {
				*out = append(*out, path)
			}
			return
		}
		for i := 0; i < v.Len(); i++ {
			leaks(v.Index(i), path+"[]", canary, out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			leaks(k, path+"{key}", canary, out)
			leaks(v.MapIndex(k), path+"{}", canary, out)
		}
	case reflect.Pointer, reflect.Interface:
		leaks(v.Elem(), path, canary, out)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			leaks(v.Field(i), path+"."+v.Type().Field(i).Name, canary, out)
		}
	}
}

// c11: text fields from humans come back only as UntrustedText.
func c11(s *suite) {
	e := s.env()
	ctx := s.ctx()
	const canary = "CANARY-ignore-previous-instructions"
	_, tcur := s.trackerFeed("")
	_, hcur := s.hostFeed("")
	it, err := e.Driver.NewItem(e.Human, canary+" title", canary+" body", ports.MarkerReady)
	s.must(err)
	s.must(e.Driver.CommentItem(e.Human, it, canary+" item comment", nil))
	ch, head := s.loopChange(it)
	s.must(e.Driver.CommentChange(e.Human, ch, canary+" change comment"))
	th, err := e.Driver.ReviewComment(e.Human, ch, ChangedFile, 1, canary+" thread")
	s.must(err)
	s.must(e.Driver.ReplyInThread(e.Owner, ch, th, canary+" reply"))
	_, err = e.Driver.SubmitReview(e.Human, ch, ReviewChangesRequested, canary+" review", nil)
	s.must(err)

	var results []any
	keep := func(v any, err error) {
		s.t.Helper()
		s.must(err)
		results = append(results, v)
	}
	keep(e.Tracker.Item(ctx, it))
	keep(e.Tracker.Candidates(ctx))
	keep(e.Tracker.FindRecords(ctx, it, ports.RecordStatus))
	keep(e.Tracker.Dependencies(ctx, it))
	keep(e.Host.ChangeForItem(ctx, it))
	keep(e.Host.Change(ctx, ch))
	keep(e.Host.Trust(ctx, ch))
	keep(e.Host.Gates(ctx, ch, head))
	keep(e.Host.ReviewRecords(ctx, ch))
	tevs, _ := s.trackerFeed(tcur)
	hevs, _ := s.hostFeed(hcur)
	results = append(results, tevs, hevs)
	mcs, err := e.Host.ManualComments(ctx, ch)
	keep(mcs, err)

	var bad []string
	for _, r := range results {
		leaks(reflect.ValueOf(r), reflect.TypeOf(r).String(), canary, &bad)
	}
	if len(bad) != 0 {
		s.t.Errorf("human text in plain fields: %v", bad)
	}
	want := map[string]bool{canary + " thread": false, canary + " reply": false, canary + " review": false}
	for _, mc := range mcs {
		body := untrusted.Read(mc.Body)
		if _, ok := want[body]; ok {
			want[body] = true
		}
	}
	for body, seen := range want {
		if !seen {
			s.t.Errorf("no manual comment carries %q as UntrustedText", body)
		}
	}
}
