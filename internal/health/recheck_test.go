package health

import (
	"context"
	"sync/atomic"
	"testing"
)

// Recheck runs only what it is asked to: a port coming free rechecks the
// API, not the model, whose probe is a request to a paid service.
func TestRecheckRunsOnlyTheNamedChecks(t *testing.T) {
	var model, api atomic.Int32
	apiState := Fail
	m := New(
		Func("model", "Model", func(context.Context) (State, string, string) { model.Add(1); return OK, "", "" }, nil),
		Func("api", "API", func(context.Context) (State, string, string) { api.Add(1); return apiState, "", "" }, nil),
	)
	changes := 0
	m.OnChange = func(prev, cur Report) { changes++ }

	// Before any full round there is nothing to keep: it runs everything.
	m.Recheck(context.Background(), "api")
	if model.Load() != 1 || api.Load() != 1 {
		t.Fatalf("first round: model %d api %d", model.Load(), api.Load())
	}
	apiState = OK
	rep := m.Recheck(context.Background(), "api")
	if model.Load() != 1 || api.Load() != 2 {
		t.Fatalf("recheck probed the model again: model %d api %d", model.Load(), api.Load())
	}
	if len(rep.Results) != 2 || rep.Results[0].Name != "api" || rep.Results[0].State != OK || rep.Results[1].Name != "model" {
		t.Fatalf("report %+v", rep.Results)
	}
	if changes != 2 {
		t.Fatalf("the API's change should be reported: %d changes", changes)
	}
	if m.Last().Results[0].State != OK {
		t.Fatal("the recheck is kept as the latest report")
	}
}
