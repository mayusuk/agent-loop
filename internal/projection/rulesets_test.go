package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
)

// TestFR13AC5_RulesetRecordsRoundTrip: the owner's read of the rulesets is
// kept per project, and each save replaces the project's records (work
// plan #101 D4).
func TestFR13AC5_RulesetRecordsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	p, q := Rulesets{DB: db, Project: "p"}, Rulesets{DB: db, Project: "q"}
	if recs, err := p.RulesetRecords(ctx); err != nil || len(recs) != 0 {
		t.Fatalf("empty: %v, %v", recs, err)
	}
	app, role := int64(501), int64(5)
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	want := []github.RulesetRecord{
		{ID: 10, UpdatedAt: at, Bypass: []github.BypassActor{{ActorType: "Integration", ActorID: &app}, {ActorType: "RepositoryRole", ActorID: &role}}},
		{ID: 11, UpdatedAt: at.Add(time.Second), Bypass: []github.BypassActor{}},
	}
	if err := p.SaveRulesetRecords(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveRulesetRecords(ctx, []github.RulesetRecord{{ID: 99, UpdatedAt: at}}); err != nil {
		t.Fatal(err)
	}
	got, err := p.RulesetRecords(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if err := p.SaveRulesetRecords(ctx, want[1:]); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.RulesetRecords(ctx); len(got) != 1 || got[0].ID != 11 {
		t.Errorf("after a second save: %+v", got)
	}
	if got, _ := q.RulesetRecords(ctx); len(got) != 1 || got[0].ID != 99 || len(got[0].Bypass) != 0 {
		t.Errorf("the other project's records changed: %+v", got)
	}
	if err := (Rulesets{DB: db}).SaveRulesetRecords(ctx, nil); err == nil {
		t.Error("a save with no project gave no error")
	}
}
