package workflows

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// flakyManual fails ManualComments with err for the first fails calls
// (every call when fails < 0).
type flakyManual struct {
	ports.CodeHost
	err   error
	fails int
	calls int
}

func (f *flakyManual) ManualComments(ctx context.Context, ref ports.ChangeRef) ([]ports.ManualComment, error) {
	f.calls++
	if f.fails < 0 || f.calls <= f.fails {
		return nil, f.err
	}
	return f.CodeHost.ManualComments(ctx, ref)
}

// TestFR7AC9_ReviewManualReadIsRetried: a transient failure of the manual
// comment read after a posted review is retried like a record write, and
// the result carries the comments (work plan #25 D3, review R3).
func TestFR7AC9_ReviewManualReadIsRetried(t *testing.T) {
	h := newHarness(t)
	item := h.item()
	ch := h.implemented(item)
	if _, err := h.f.ReviewComment(fake.Owner, ch.Ref, "a.go", 1, "rename this"); err != nil {
		t.Fatal(err)
	}
	fl := &flakyManual{CodeHost: h.a.Host, err: fmt.Errorf("502: %w", ports.ErrTransient), fails: 1}
	h.a.Host = fl
	rep := h.review(item, ch, nil, reviewer(nil, nil))
	if rep.Result == nil || len(rep.Result.Manual) != 1 {
		t.Fatalf("result after one transient read failure: %+v", rep.Result)
	}
	if fl.calls < 2 {
		t.Fatalf("the read was tried %d times, want a retry", fl.calls)
	}
}

// TestFR7AC9_ReviewManualReadFailureIsNotACrash: a read that keeps failing
// after the review was posted returns the review's report without the
// manual comments, not an error the workflow would count as a crash; the
// next poll and merge check read the comments (review R3).
func TestFR7AC9_ReviewManualReadFailureIsNotACrash(t *testing.T) {
	for name, cause := range map[string]error{
		"transient": fmt.Errorf("502: %w", ports.ErrTransient),
		"permanent": errors.New("decode: bad response"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			item := h.item()
			ch := h.implemented(item)
			if _, err := h.f.ReviewComment(fake.Owner, ch.Ref, "a.go", 1, "rename this"); err != nil {
				t.Fatal(err)
			}
			h.a.Host = &flakyManual{CodeHost: h.a.Host, err: cause, fails: -1}
			rep := h.review(item, ch, nil, reviewer(nil, []newF{{4, "", 0}})) // fails the test on an error
			if rep.Result == nil || rep.Result.Review == nil || len(rep.Result.Review.Findings) != 1 {
				t.Fatalf("result = %+v, want the posted review", rep.Result)
			}
			if len(rep.Result.Manual) != 0 {
				t.Fatalf("manual = %+v, want none when the read failed", rep.Result.Manual)
			}
			if n := len(h.reviews(ch.Ref)); n != 1 {
				t.Fatalf("review records = %d, want 1", n)
			}
		})
	}
}
