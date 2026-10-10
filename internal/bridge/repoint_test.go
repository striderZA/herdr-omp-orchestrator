package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
)

// repointMethods are the two operations that re-point queued dependents of a
// superseded task to its successor (issue #61). Both are primary-only.
var repointMethods = []string{MethodRepointPreview, MethodRepoint}

func TestBridgeRepointMethodsArePrimaryOnly(t *testing.T) {
	ctx := context.Background()
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)

	calls := 0
	var seen Principal
	count := func(_ context.Context, p Principal, _ json.RawMessage) (any, error) {
		calls++
		seen = p
		return map[string]any{"accepted": true}, nil
	}
	stub := Ops{RepointPreview: count, Repoint: count}
	bodies := map[string]string{
		MethodRepointPreview: `{"supersededTaskId":"orch_s","dependentIds":["orch_d"]}`,
		MethodRepoint:        `{"supersededTaskId":"orch_s","successorTaskId":"orch_n","dependents":[{"taskId":"orch_d","expectedRevision":3}]}`,
	}

	// A confirmed, bound worker may not re-point anything.
	f.env["HERDR_PANE_ID"] = workerPane
	for _, method := range repointMethods {
		wantError(t, f.callWith(method, workerSession, bodies[method], stub), CodeForbidden)
	}

	// A primary that is announced but not yet confirmed is refused too.
	if err := f.store.RecordReady(ctx, primaryPane, announcedID(primarySession), 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	f.env["HERDR_PANE_ID"] = primaryPane
	for _, method := range repointMethods {
		wantError(t, f.callWith(method, primarySession, bodies[method], stub), CodeNotReady)
	}
	if calls != 0 {
		t.Fatalf("a refused caller reached the coordinator %d times", calls)
	}

	// The confirmed primary reaches both operations, with the verified principal.
	f.confirm(primaryPane, primarySession)
	for _, method := range repointMethods {
		if resp := f.callWith(method, primarySession, bodies[method], stub); !resp.OK {
			t.Fatalf("%s failed for the confirmed primary: %+v", method, resp.Error)
		}
	}
	if calls != len(repointMethods) {
		t.Fatalf("the operations ran %d times, want %d", calls, len(repointMethods))
	}
	if seen.Role != "primary" || seen.ProjectKey != f.repoRoot || seen.LaunchPaneID != primaryPane {
		t.Fatalf("the coordinator must see the verified principal: %+v", seen)
	}
}

func TestBridgeRepointMethodsWithoutWiringAreAuthorizedThenUnavailable(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.asPrimary()
	for _, method := range repointMethods {
		// Authorized, but no operation is wired: not_implemented, never a bypass.
		wantError(t, f.callWith(method, primarySession, `{}`, Ops{}), CodeNotImplemented)
		// A build whose coordinator did not open answers with the cause.
		wantError(t, f.callWith(method, primarySession, `{}`, UnavailableOps(errors.New("no coordinator"))), CodeHerdrUnavailable)
	}
}

// The successor and re-point refusals reach the extension as the stable codes it
// reads, however the coordinator wrapped the sentinel (issue #61).
func TestBridgeMapsSuccessorAndRepointRefusalsToStableCodes(t *testing.T) {
	cases := map[error]string{
		coord.ErrSuccessorRequired:   "successor_required",
		coord.ErrSuccessorNotAllowed: "successor_not_allowed",
		coord.ErrSuccessorUnknown:    "successor_unknown",
		coord.ErrSuccessorSelf:       "successor_self",
		coord.ErrRepointRefused:      "repoint_refused",
		coord.ErrRepointChanged:      "repoint_changed",
		coord.ErrRepointCycle:        "repoint_cycle",
	}
	for sentinel, want := range cases {
		if got := coordErrorCode(fmt.Errorf("record the decision: %w", sentinel)); got != want {
			t.Errorf("coordErrorCode(%v) = %q, want %q", sentinel, got, want)
		}
	}
}

// The extension sends these JSON names and the bridge decodes them into the
// coordinator's params, so the names are part of the protocol (issue #61).
func TestSuccessorAndRepointParamsDecodeFromTheWireNames(t *testing.T) {
	var review coord.ReviewParams
	if err := json.Unmarshal([]byte(`{"taskId":"orch_s","expectedRevision":2,"outcome":"superseded","disposition":"replaced","successor":"orch_n"}`), &review); err != nil {
		t.Fatalf("decode review params: %v", err)
	}
	if review.Successor != "orch_n" || review.Outcome != "superseded" {
		t.Fatalf("review params = %+v, want the successor and the superseded outcome", review)
	}

	var ticket coord.TicketReviewParams
	if err := json.Unmarshal([]byte(`{"ticketId":"tkt_s","digest":"d","outcome":"superseded","disposition":"replaced","successor":"tkt_n"}`), &ticket); err != nil {
		t.Fatalf("decode ticket review params: %v", err)
	}
	if ticket.Successor != "tkt_n" {
		t.Fatalf("ticket review params = %+v, want the successor ticket", ticket)
	}

	var repoint coord.RepointParams
	if err := json.Unmarshal([]byte(`{"supersededTaskId":"orch_s","successorTaskId":"orch_n","dependents":[{"taskId":"orch_d","expectedRevision":3}]}`), &repoint); err != nil {
		t.Fatalf("decode repoint params: %v", err)
	}
	if repoint.SuccessorTaskID != "orch_n" || len(repoint.Dependents) != 1 || repoint.Dependents[0].ExpectedRevision != 3 {
		t.Fatalf("repoint params = %+v, want the successor and the confirmed revision", repoint)
	}
}
