# ADR 0007: Settle an uncertain inbox note

Status: Accepted and implemented (issue #79); extends ADR 0006.

## Context

An inbox note whose notification prompt has an ambiguous outcome is recorded `uncertain` and is never resent (`internal/coord/reconcile.go`). ADR 0006 decided that `sending` and `uncertain` notes block forgetting. Nothing could clear an `uncertain` note: `SettleInbox` moves a `sending` note, or an `uncertain` note that carries the startup-recovery marker, and no bridge method, CLI route or extension tool settled a note. A project with one such note could not be forgotten, and ADR 0006 exists to replace deleting the state by hand.

## Decision

1. **A user decision settles an uncertain note.** A user's confirmed decision is the only path that moves a note out of `uncertain` by a human. There are two outcomes, named as `settle_input` names them:
   - `delivered` ends the note. It is recorded as `delivered` with the detail "the user confirmed the notification reached the primary", and it is never sent again.
   - `not_delivered` returns the note to `pending`. The primary's next idle turn delivers it, as it delivers any pending note. The user has said the notification did not arrive, so a delivery is wanted.
2. **Preview, confirm, then write.** The primary's tool `orch_settle_note` reads the note through the bridge method `inbox_preview`, a read-only view of the task, kind, label, summary, state and detail, with a digest over those facts. The user confirms a dialog built from that preview, never from the model's text. The write is the bridge method `inbox_settle`, which takes the note id, the digest the user saw and the decision.
3. **The write is bound to what the user saw.** `inbox_settle` refuses with `evidence_changed` unless the note still matches the digest. The store writes only while the note is still `uncertain`, belongs to the project and carries the detail the decision was made on (`state.Store.SettleUncertainInbox`, a compare-and-swap). A note has no revision, so its detail is what a settlement changes. A note that is not uncertain is refused with `note_not_uncertain`. A note of another project is refused with `bad_request`, as if it did not exist, so one project cannot read another project's notes through this path.
4. **The project comes from the verified principal.** Both methods are `RolePrimary` through `mutate`, so the coordinator's `agent get` confirmation runs on every call. As with `inbox_list`, the project comes from the principal and never from the request body.
5. **`SettleInbox` is unchanged.** Its guard protects the live delivery path. A human decision needs its own narrower write, not a wider guard on the existing one.

## Rejected alternatives

- **Accept the block.** The project stays unforgettable through the plugin, and the only way out is editing the state database by hand, which ADR 0006 was written to replace.
- **Let forget delete uncertain notes unseen.** This reverses ADR 0006's review decision and discards the only record of an ambiguous delivery.
- **A board action.** The board is read-only for tasks and writes only tickets, through `TicketOps`. A note decision belongs to the primary, made with the user in the primary's conversation, as `settle_input` is.
- **Resend on `not_delivered` without the consent dialog.** Not used. The decision is confirmed through the dialog, and the note is then delivered only at an idle turn, like any pending note.

## Consequences

- An `uncertain` note can be cleared, and a project with one can be forgotten once the decision is recorded. A `sending` note still blocks until its delivery settles.
- `not_delivered` can produce a second notification in the primary's conversation if the first one did arrive. The user looked at the pane before deciding, which is the only evidence the plugin can use.
- The protocol change is additive: two bridge methods and one new refusal code (`note_not_uncertain`). The protocol stays at version 1.
- The forget refusal for an uncertain note now names the action to take (`internal/task/forget.go`).

## Verification

- `internal/state`: `TestSettleUncertainInboxChangesOnlyAnUncertainNoteOfTheProject` (pending, other-project, changed-detail, invalid-target and missing notes write nothing; `delivered` ends the note once) and `TestSettleUncertainInboxNotDeliveredRequeuesTheNote`.
- `internal/task`: `TestInboxDigestChangesWithEveryFactTheUserInspects`, `TestInboxDigestHasNoFieldBoundaryCollisions`, and the forget blocker tests `TestForgetBlockersRefusesEachInFlightKind` and `TestForgetBlockersReasonsHaveStableOrder`.
- `internal/coord`: `TestInboxPreviewShowsOnlyAnUncertainNoteOfTheProject`, `TestInboxSettleDeliveredEndsTheUncertaintyWithoutResendingTheNote` (a later flush sends nothing, and the note stops blocking forget), `TestInboxSettleNotDeliveredRequeuesTheNoteForTheNextIdleTurn` and `TestInboxSettleRefusesAStaleDigestAndAnUnknownDecision`.
- `internal/bridge`: `TestBridgeInboxSettleMethodsArePrimaryOnly`, `TestBridgeInboxSettleShowsAndSettlesOneUncertainNote` and `TestBridgeAdjudicationRefusalsAreCoded` (the new code).
- `internal/setup` and `bun run omp/load-extension.check.ts`: the tool is registered and its schema builds under OMP's builder. The confirmation helper refuses a note that is not uncertain, a missing digest and a decision the user cannot make, and the primary role keeps the tool.
