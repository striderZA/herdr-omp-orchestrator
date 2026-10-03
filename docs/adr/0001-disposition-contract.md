# ADR 0001: Disposition contract (structured outcome + note)

Status: Accepted (issue #5). The maintainer decided the contract before the code was finalised; this record was written after the first code edits, so the ADR trails the implementation rather than preceding it.

## Context

`Disposition` was free text (<= 200 chars). Any non-empty value closed the task (`task.StageOf`), released its slot and reserved paths, and satisfied dependents. A rejected or abandoned task therefore unblocked dependents exactly like an integrated one.

## Decision

1. A decision is a structured **outcome** plus a free-text **note**.
   - Outcomes: `integrated`, `kept` (accepting); `rejected`, `superseded`, `abandoned` (non-accepting).
   - `Task.Outcome` holds the kind; `Task.Disposition` holds the note and defaults to the outcome name when no note is given, so `Disposition != ""` remains the single "closed" fact used by the stage projection, slot release and path release.
2. Every outcome closes the task, frees its slot and releases its reserved paths and checkout eligibility for cleanup.
3. Only accepting outcomes release **dependents**. Dependents of a non-accepting task stay queued (not auto-closed or cascaded); the primary decides later. The queue reason names the dependency's outcome.
4. **Single validation point:** `coord.decisionOf`. The outcome is mandatory for new decisions (bridge and extension). The note is bounded to 200 chars.
5. **Legacy rows:** a closed task with empty `Outcome` is read as accepting (`Task.Accepted`), preserving the behaviour it was recorded under. No data migration: the task is stored as JSON, and the new field is additive.
6. **First decision wins:** a repeat is idempotent only if outcome and note both match; anything else is `ErrAlreadyDecided`.
7. **Surface:** the review confirmation dialog shows the outcome and states whether dependents will be released; the board detail shows the outcome.

## Consequences

- `Review`/`ReviewAtRevision` take `(outcome, note)`; the bridge `review` params gain `outcome`; `orch_review` requires `outcome`.
- No way yet to unblock dependents of a rejected task other than re-dispatching; revisit if needed.
