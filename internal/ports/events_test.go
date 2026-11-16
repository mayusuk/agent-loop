package ports

import "testing"

// TestEventVariants lists every event variant of INTEGRATION-SPEC §8.2 and
// §8.3, so a missing or extra one shows up here.
func TestEventVariants(t *testing.T) {
	items := []ItemEventDetail{
		MarkerChanged{}, ItemClosed{}, ItemReopened{}, AssigneeChanged{}, Commented{}, DependencyChanged{},
	}
	changes := []ChangeEventDetail{
		HeadMoved{}, Merged{}, ChangeClosed{}, ChangeReopened{}, Opened{}, ReviewSubmitted{},
		ThreadResolved{}, ThreadUnresolved{}, ReviewDismissed{}, GateChanged{},
	}
	if len(items) != 6 || len(changes) != 10 {
		t.Fatalf("got %d item and %d change variants, want 6 and 10", len(items), len(changes))
	}
	e := ItemEvent{Item: "42", Version: "v2", Detail: MarkerChanged{Actor: Actor{ID: "1", Kind: ActorHuman}, Marker: MarkerReady, On: true}}
	if mc, ok := e.Detail.(MarkerChanged); !ok || mc.Marker != MarkerReady || !mc.On {
		t.Fatalf("type switch on detail lost data: %+v", e.Detail)
	}
}
