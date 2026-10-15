# ADR 0006: Forget a project

Status: Accepted (design). Approved in chat and reviewed; implementation pending.

## Context

A repository can be bound but never removed. `bind` refuses a pane that a project or task records, and it reopens the recorded primary for a repository that is already bound. Starting over on one repository currently means deleting the plugin's state by hand, which no document describes. No store method deletes a project, the plugin has no uninstall action, and `cleanup` removes only a reviewed worker's checkout.

## Decision

1. **Forget deletes one project's plugin records and nothing else.** It removes the project's `projects` row, its `tasks`, `tickets` and `inbox` rows, and the `ready_sessions` rows for its launch pane and its tasks' launch panes. It never changes Git (the repository, branches and worktrees), Herdr (workspaces, panes and agent sessions) or the installed extension.
2. **Entry is a pane, as reattach is.** The `forget` action (`open-forget`, contexts `pane` and `workspace`) resolves the project with `board.ProjectFor`, which refuses when no project or more than one matches. It passes the resolved key to the `forget` overlay pane. The action writes nothing.
3. **Plan first, read-only.** `coord.PlanForget` reports the counts (tasks decided and undecided, tickets, notes by delivery state, launch and public pane claims), the refusal reasons, and what stays: the repository, its `orch/` branches, the worktree paths, the Herdr workspace, and the primary and worker panes. Planning writes nothing and makes no mutating Herdr call. When a refusal applies, the pane shows the reasons and stops.
4. **Typed confirmation.** Otherwise the pane asks the user to type the last path element of the project key. The comparison trims the text and ignores case. Anything else, including an empty line, cancels and deletes nothing.
5. **Refusals,** checked in the plan and again inside the write transaction:
   - a task with an empty disposition. The message names an outcome to record first, such as `abandoned`;
   - a task with `slot_claimed_at > 0` or a non-empty `step_claim_owner`;
   - a note in state `sending` or `uncertain`. A `pending` note is not in flight, so it does not block; the plan counts it as lost;
   - a bind intent for the project's key, which is the canonical root;
   - a reattach notice in state `uncertain`.

   Recording a decision on an undecided task needs a verified primary. If the primary is gone, the refusal says to reattach it first.
6. **One fingerprint, one transaction.** `internal/state` computes a fingerprint over the project row, each task's ID and revision, each note's ID and state, each ticket's ID and revision, and whether the project's session and intent rows exist. `ForgetFingerprint` computes it for the plan. `ForgetProject(key, fingerprint)` recomputes it inside the write transaction, applies every refusal in rule 5, and refuses with `forget_plan_changed` if the fingerprint differs. Deletion happens in that transaction or not at all.
7. **Identity.** The key comes from the invoking pane, never from typed text. The write checks that it receives the key the plan named.
8. **Output and codes.** As reattach does, the pane prints the outcome and the leftover list to stdout and writes the envelope to stderr. New codes: `forget_refused` (reasons in the message), `forget_plan_changed` and `forget_not_confirmed`. `project_unknown` covers an unresolved or ambiguous pane.
9. **The primary keeps its read-only restriction** until its OMP session restarts (AGENTS.md). The pane says so and names the two ways to clear it: restart the session, or close its pane.

## Rejected alternatives

- **Archive (keep the records, stop dispatch).** Archived records keep their pane claims, so that pane cannot host another project. Re-binding the same repository would also need an unarchive path on every read path.
- **Full teardown (also close panes and remove clean checkouts).** Closing the user's OMP session and removing worktrees makes partial failure likely, and it goes beyond forgetting. A later ADR can add it.
- **A plain yes, as reattach uses.** A stray Enter would delete the records, and forgetting cannot be undone.
- **A JSON export before deleting.** It adds a file format and a cleanup policy. It is an option for later.

## Consequences

- Forgetting is irreversible. The counts in the plan and the typed confirmation are the guard, with the refusals as the backstop.
- Worktrees and branches stay on disk. A later `bind` of the same repository starts a new project, and nothing records the old worktrees, so nothing adopts them.
- A board open on the forgotten project should fail its read until it is closed. This is expected and not yet observed.
- No bridge method, extension change or protocol change.

## Decided in review

- **Pending notes do not block forget** (review decision). They are counted in the plan and deleted with the project. `sending` and `uncertain` notes still block.

## Verification

- `internal/state`: each refusal in rule 5 leaves every row unchanged; a pending note does not block and is removed with the project; forget deletes this project's rows and no other project's; a later `CreateProject` on the freed pane succeeds; a changed fingerprint refuses the write.
- `internal/coord`: planning writes nothing and makes no mutating Herdr call; `Forget` never calls `worktree remove`; a confirmed plan that no longer matches refuses.
- `cmd/herdr-omp-orchestrator`: the manifest declares the action and pane with the expected commands; a wrong typed name deletes nothing; `open-forget` refuses an unknown or ambiguous project; an event for a forgotten pane is a no-op.
- Gate: `go test -timeout 30m ./...` and `go build`. A real-Herdr acceptance run is a separate decision, because it creates sessions and workspaces.
