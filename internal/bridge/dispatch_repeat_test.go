package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
)

// A repeated dispatch (issue #4) is answered with the existing task and
// `duplicate: true`. The bridge adds nothing to that answer and must not drop it:
// the extension hands the bridge's result to the model verbatim, so this field is
// the only way the model learns that no new task was started. The answer is built
// from the coordinator's own result type, so renaming its wire field fails here.
func TestBridgeDispatchCarriesTheDuplicateFlagAndProtocolStaysOne(t *testing.T) {
	if Protocol != 1 {
		t.Fatalf("Protocol = %d: the duplicate flag is an additive result field and the version stays 1", Protocol)
	}
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.confirm(primaryPane, primarySession)

	answers := []coord.DispatchResult{
		{TaskID: "orch_aaaaaaaaaaaa", Title: "Plan: parser", Phase: "prompt_submitted", Stage: "running"},
		{TaskID: "orch_aaaaaaaaaaaa", Title: "Plan: parser", Phase: "prompt_submitted", Stage: "running", Duplicate: true},
	}
	calls := 0
	ops := Ops{Dispatch: func(context.Context, Principal, json.RawMessage) (any, error) {
		answer := answers[calls]
		calls++
		return answer, nil
	}}
	params := `{"title":"Plan: parser","brief":"b","checks":["go test ./..."],"baseRevision":"0123456"}`

	f.env["HERDR_PANE_ID"] = primaryPane
	var first, repeat map[string]any
	for i, into := range []*map[string]any{&first, &repeat} {
		resp := f.callWith("dispatch", primarySession, params, ops)
		if !resp.OK {
			t.Fatalf("dispatch %d failed: %+v", i+1, resp.Error)
		}
		resp.decodeResult(t, into)
	}

	if _, present := first["duplicate"]; present {
		t.Errorf("a new task's answer carries a duplicate field: %v", first)
	}
	if repeat["duplicate"] != true {
		t.Errorf("a repeat's answer = %v, want duplicate:true", repeat)
	}
	if repeat["taskId"] != first["taskId"] {
		t.Errorf("the repeat names %v, want the first task %v", repeat["taskId"], first["taskId"])
	}
}
