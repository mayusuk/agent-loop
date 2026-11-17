package projection

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// I11, INTEGRATION-SPEC §9.8 step 6: each coder run's start, end and head
// are recorded, and the runs live in a poll window are found.
func TestI11_CoderRunsRecordedAndFoundByWindow(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	runs := CoderRuns{DB: db, Project: "p"}
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(runs.Started(ctx, "1", "r1", t0))
	must(runs.Started(ctx, "1", "r1", t0.Add(time.Hour))) // a retry changes nothing
	must(runs.Ended(ctx, "1", "r1", t0.Add(10*time.Minute), "abc"))
	must(runs.Started(ctx, "2", "r2", t0.Add(20*time.Minute))) // still running
	must(CoderRuns{DB: db, Project: "other"}.Started(ctx, "1", "r3", t0))

	cases := []struct {
		name     string
		from, to time.Time
		want     []string
	}{
		{"before any run", t0.Add(-time.Hour), t0.Add(-time.Minute), nil},
		{"during r1", t0.Add(5 * time.Minute), t0.Add(6 * time.Minute), []string{"r1"}},
		{"between the runs", t0.Add(11 * time.Minute), t0.Add(19 * time.Minute), nil},
		{"r2 has no end", t0.Add(5 * time.Hour), t0.Add(6 * time.Hour), []string{"r2"}},
		{"both", t0, t0.Add(time.Hour), []string{"r1", "r2"}},
	}
	for _, c := range cases {
		got, err := runs.CoderRuns(ctx, c.from, c.to)
		must(err)
		var ids []string
		for _, r := range got {
			ids = append(ids, "r"+string(r.Item))
		}
		if len(ids) != len(c.want) || (len(ids) > 0 && ids[0] != c.want[0]) || (len(ids) > 1 && ids[1] != c.want[1]) {
			t.Errorf("%s: runs %v, want %v", c.name, ids, c.want)
		}
	}
	got, _ := runs.CoderRuns(ctx, t0, t0)
	if len(got) != 1 || got[0].Head != "abc" || !got[0].Start.Equal(t0) || !got[0].End.Equal(t0.Add(10*time.Minute)) || got[0].Item != "1" {
		t.Errorf("r1 = %+v", got)
	}
	// The GitHub host asks for every run up to now with a zero from.
	all, _ := runs.CoderRuns(ctx, time.Time{}, t0.Add(time.Hour))
	if len(all) != 2 || !all[1].End.IsZero() {
		t.Errorf("all runs = %+v", all)
	}
}

func TestI11_CoderRunEndWithoutStartIsNotFound(t *testing.T) {
	db := openTest(t)
	err := CoderRuns{DB: db, Project: "p"}.Ended(context.Background(), "1", "nope", time.Now(), "")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := (CoderRuns{DB: db}).Started(context.Background(), "1", "r", time.Now()); err == nil {
		t.Fatal("a run with no project was recorded")
	}
}
