package workflows

import (
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

// withUsage adds a usage object to a handler's result.
func withUsage(hd handler, usage string) handler {
	return func(h *harness, in contract.Input, out string) string {
		res := hd(h, in, out)
		return strings.TrimSuffix(res, "}") + `,"usage":` + usage + `}`
	}
}

// FR-16 AC3, INTEGRATION-SPEC §5.1: a valid result's usage.cost reaches the
// workflow; a result with no cost, or an invalid one, adds nothing.
func TestFR16AC3_StageReportCarriesUsageCost(t *testing.T) {
	h := newHarness(t)
	item := h.item()
	rep := h.run(h.req(item, core.RunStage{Stage: contract.StageImplement}),
		withUsage(implementer("agent/"+string(item)), `{"cost":1.5,"currency":"USD"}`))
	if rep.Cost == nil || *rep.Cost != 1.5 {
		t.Fatalf("cost = %v, want 1.5", rep.Cost)
	}

	h2 := newHarness(t)
	item2 := h2.item()
	rep = h2.run(h2.req(item2, core.RunStage{Stage: contract.StageImplement}),
		withUsage(implementer("agent/"+string(item2)), `{"input_tokens":10}`))
	if rep.Cost != nil {
		t.Fatalf("cost = %v with no usage.cost", *rep.Cost)
	}

	h3 := newHarness(t)
	item3 := h3.item()
	rep = h3.run(h3.req(item3, core.RunStage{Stage: contract.StageImplement}),
		withUsage(func(h *harness, in contract.Input, out string) string {
			return resultJSON(in, "done", map[string]any{"head": "nope"})
		}, `{"cost":9}`))
	if rep.Result == nil || rep.Result.Outcome != contract.OutcomeFailed || rep.Cost != nil {
		t.Fatalf("invalid result: %+v cost %v", rep.Result, rep.Cost)
	}
}
