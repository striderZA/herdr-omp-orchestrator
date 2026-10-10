# ADR 0005: Successors for superseded decisions and re-pointing queued dependents

Status: Accepted (issue #61). Implemented with the change that adds `successor` to task and ticket decisions.

## Context

A `superseded` decision (ADR 0001) said that work was replaced but not where it went. A queued dependent of a superseded task could not be pointed at the replacement, and a superseded ticket could not name the ticket that replaces it. Only accepting outcomes release dependents, so a dependent of superseded work stayed queued until someone re-dispatched it.

## Decision

1. **A superseded task decision names a successor task, and a superseded ticket decision names a successor ticket.** The successor must belong to the same project and must not be the decided subject. A task successor may be in any stage, because the dependents it releases still wait on the successor's own accepting outcome. A ticket successor may be in any column and may already be decided, because it is only a reference. This settles the two questions issue #61 left open.
2. **Only a superseded decision may name a successor.** A superseded decision with no successor is refused (`successor_required`). Any other outcome that names one is refused (`successor_not_allowed`). A rejected decision therefore never has a successor.
3. **One validation point.** `coord.checkSuccessor` is the single check that task and ticket decisions pass. It runs only for a decision that is not yet recorded, so an idempotent repeat is compared with the recorded decision first. The store repeats the same rules inside the write transaction: `checkSuccessorTx` for task writes and `checkTicketSuccessorTx` for ticket decisions (ADR 0002 decision 4).
4. **Recording `superseded` never releases a dependent.** Only accepting outcomes release dependents (ADR 0001 decision 3), and that rule is unchanged.
5. **A repeated decision is idempotent only when its successor matches too**, extending ADR 0001 decision 6.
6. **A re-point is a separate action that the user confirms.** `repoint_preview` (read-only) and `repoint` (the write) are primary-only bridge methods. The `orch_repoint` tool shows the exact dependents and their revisions and sends nothing until the user confirms. The write rewrites only the named dependents that are queued, that wait on the superseded task, and that hold no provisioning slot. It replaces that one dependency with the successor in one store transaction, and it refuses if any confirmed revision or the successor has changed since the preview. It also refuses a rewrite that would make the project's dependencies cyclic, counting the re-pointed dependencies. Afterwards the queue is drained, so a dependent whose successor already accepted starts now.
7. **A re-pointed dependent is released only when its successor has an accepting outcome.** The admission gate (`unsatisfiedDependency`) is unchanged.
8. **A ticket successor is never re-pointed and creates no dependency.** A ticket decision changes no linked task.
9. **Legacy rows keep their behaviour.** A superseded row that names no successor reads and repeats as before (ADR 0001 decision 5). A re-point refuses it, because it names no successor to re-point to.
10. **Queue reasons and the board name the path.** A dependent of a superseded task that names a successor says the primary can re-point it. The board shows `Successor:` on superseded tasks and tickets. Every other queue reason is unchanged.

## Rejected alternatives

- **Release dependents automatically on `superseded`.** A non-accepting outcome would then unblock work, which is the free-text problem ADR 0001 removed.
- **Re-point ticket successors.** A ticket successor is a reference. No dependency edge exists to move.

## Consequences

- The bridge answers these new stable codes: `successor_required`, `successor_not_allowed`, `successor_unknown`, `successor_self`, `repoint_refused`, `repoint_changed` and `repoint_cycle`. A ticket successor that is missing or belongs to another project answers `ticket_unknown`, the existing rule for tickets. The protocol version stays 1: the review and ticket-review params gain an optional `successor`, and the two repoint methods are additive.
- Rejected tasks still have no successor, so their dependents still need re-dispatching. ADR 0001's consequence stands for them.
- The extension refuses a successor that does not fit the outcome before any dialog. The coordinator and the store enforce the same rules again, so a bypassed extension still cannot record an invalid decision.
