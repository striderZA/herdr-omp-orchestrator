# ADR 0002: Repeat detection for dispatch (open work is returned, not duplicated)

Status: Accepted (issue #4). The duplicate was reproduced before any mechanism was chosen; the maintainer then chose content-based detection against open tasks (2026-10-03). The decision is recorded alongside the code that implements it.

## Context

`Coordinator.Dispatch` minted a fresh task for every request and never compared it with existing tasks. The extension kills the bridge child after 60 s (`BRIDGE_TIMEOUT_MS`) while the executable itself allows two minutes, and the task row is written at the `intent` phase before any provisioning. A dispatch that outlives the extension's timeout therefore leaves a persisted task and a lost answer, and a caller that repeats `orch_dispatch` got a second task, a second worktree and a second agent for the same work.

Reproduced with the coordinator fixture (real repository, real store, stand-in Herdr CLI) by stalling the brief prompt, cancelling the caller and repeating the identical dispatch. A cancelled context stands in for the process kill; the repeat never reads the first task's state, so the difference cannot change the count. Not shown: that a model really re-calls the tool after `bridge_timeout`, and that a real dispatch takes longer than 60 s.

The only existing brake was the reserved-path guard, which is a path lock: it refused the repeat only while the first task was open and held the same paths, and a request with no reserved paths (such as a planner) had none.

## Decision

1. **Same work** is `task.Task.Fingerprint` (version `dispatch/v1`): the SHA-256 of one canonical JSON array of title, brief, checks (ordered), dependencies and reserved paths (sets, sorted), base revision and ticket id. Task identity and progress are not part of it.
2. A request for work an **open** task of the same project holds is a **repeat**. A task is open until it records a disposition (ADR 0001): a queued task is open, and every outcome, rejected included, closes it.
3. A repeat creates and touches nothing (no task, worktree, agent, slot, or queue drain) and returns the existing task as it stands. `DispatchResult` gains an additive `duplicate: true`, omitted otherwise; `bridge.Protocol` stays 1.
4. It is decided twice. `Coordinator.dispatch` checks read-only before the queue drain; the authoritative check is `state.ClaimTask`, inside the claim's write transaction, so two processes racing with the same work record exactly one task. Both use `task.OpenHolderOf`, so the rule has one definition.
5. The check runs before the reserved-path comparison: an identical request is a repeat, not a second worker that wants the same files. Different work on held paths is still `ErrReservedConflict`.
6. There is no caller-supplied key, no new column and no migration. Stored tasks are fingerprinted from their own fields, so existing rows take part; where earlier duplicates exist, the oldest open one is returned.
7. A repeat does not resume or repair the task it returns. A stalled task is reported with its recorded blocker or attention, and resuming stays an explicit action.

## Consequences

- A deliberate identical second worker is answered with the first while that one is open. Vary the title or brief, or record a decision on the first.
- Dispatching the same work again after a decision, including a rejection, creates a new task.
- A ticket is part of the work: the same text for two tickets is two tasks.
- Rejected: a caller-supplied key (a per-invocation key cannot catch a model that re-invokes the tool, and needs a protocol change); a ticket-scoped guard for the planner (does not close the general duplicate); no change (a persisted, blocked task is visible, but the duplicate it invites starts a second agent).
- Follow-up, not decided here: a task killed before its first provisioning step is resumed only by startup reconciliation in `internal/coord` (callers outside that package were not checked), so a repeat that returns it does not move it forward.
