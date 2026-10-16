package bridge

import (
	"context"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/task"
)

var inboxSettleMethods = []string{MethodInboxPreview, MethodInboxSettle}

// A worker's confirmed session may not preview or settle an inbox note, on its own
// pane or any other: only the verified primary reads a note it may decide on.
func TestBridgeInboxSettleMethodsArePrimaryOnly(t *testing.T) {
	f, ops := primaryTicketBridge(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)

	f.env["HERDR_PANE_ID"] = workerPane
	for _, method := range inboxSettleMethods {
		wantError(t, f.callWith(method, workerSession, `{}`, ops), CodeForbidden)
	}
}

// The primary previews one uncertain note of its own project, settles it on the
// digest it was shown, and the same decision is refused once nothing is left to
// decide. Notes of another project and notes that are not uncertain are refused
// with their own codes.
func TestBridgeInboxSettleShowsAndSettlesOneUncertainNote(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	const detail = "the notification prompt to w1:p1 may already have been delivered; inspect `agent get`/`agent read` before retrying: timeout"
	uncertain := f.seedInboxNote(f.repoRoot, "t2", "result", "finished", task.DeliveryUncertain, detail)
	pending := f.seedInboxNote(f.repoRoot, "t1", "progress", "halfway", task.DeliveryPending, "")
	foreign := f.seedInboxNote(foreignTicketProject, "t9", "result", "elsewhere", task.DeliveryUncertain, "foreign detail")

	var preview coord.InboxSettlePreview
	f.ticketCall(ops, MethodInboxPreview, ticketBody(t, map[string]any{"noteId": uncertain.ID})).decodeResult(t, &preview)
	if preview.NoteID != uncertain.ID || preview.DeliveryDetail != detail || preview.Digest == "" {
		t.Fatalf("preview = %+v", preview)
	}
	wantError(t, f.ticketCall(ops, MethodInboxPreview, ticketBody(t, map[string]any{"noteId": pending.ID})), "note_not_uncertain")
	wantError(t, f.ticketCall(ops, MethodInboxPreview, ticketBody(t, map[string]any{"noteId": foreign.ID})), "bad_request")

	// A digest the user did not inspect refuses, and an unknown decision is a bad request.
	wantError(t, f.ticketCall(ops, MethodInboxSettle, ticketBody(t, map[string]any{
		"noteId": uncertain.ID, "observedDigest": "stale", "decision": "delivered",
	})), "evidence_changed")
	wantError(t, f.ticketCall(ops, MethodInboxSettle, ticketBody(t, map[string]any{
		"noteId": uncertain.ID, "observedDigest": preview.Digest, "decision": "maybe",
	})), "bad_request")
	if stored, err := f.store.Inbox(ctx, uncertain.ID); err != nil || stored.State() != task.DeliveryUncertain {
		t.Fatalf("refused settlements changed the note: %+v, %v", stored, err)
	}

	var settled coord.InboxSettleResult
	f.ticketCall(ops, MethodInboxSettle, ticketBody(t, map[string]any{
		"noteId": uncertain.ID, "observedDigest": preview.Digest, "decision": "delivered",
	})).decodeResult(t, &settled)
	if settled.NoteID != uncertain.ID || settled.DeliveryState != task.DeliveryDelivered {
		t.Fatalf("settle = %+v", settled)
	}
	if stored, err := f.store.Inbox(ctx, uncertain.ID); err != nil || stored.State() != task.DeliveryDelivered {
		t.Fatalf("stored note after the settlement = %+v, %v", stored, err)
	}
	wantError(t, f.ticketCall(ops, MethodInboxSettle, ticketBody(t, map[string]any{
		"noteId": uncertain.ID, "observedDigest": preview.Digest, "decision": "delivered",
	})), "note_not_uncertain")
}
