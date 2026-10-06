# Product intent and contract

This document is the tracked, contributor-facing statement of what the plugin is for and which boundaries must remain true. For implementation conventions and commands, see [AGENTS.md](../AGENTS.md).

## Goal and boundary

Make parallel coding in one Git repository feel like talking to one OMP agent instead of managing terminal tabs. A human works through one primary OMP session. The primary delegates independent, bounded tasks to workers; the plugin records task facts, reports, and inbox notes; and the human decides what to accept. Herdr remains the owner of the terminal and agent runtime. The plugin coordinates it; it does not replace it.

The supported product is a local, Windows-only Herdr plugin for one bound repository/project at a time. Its durable state lives under Herdr's plugin state directory, not in the project checkout or the installed extension. The task board is a terminal view, not a separate web or desktop product.

The founding design draft was removed in commit `82e84ef`; its source remains in Git history (`git show 82e84ef^:docs/superpowers/specs/2026-09-25-herdr-omp-orchestrator-design.md`). This document restates the current contract rather than copying that earlier draft verbatim.

## Scope

### In scope

- Bind a Git repository to a primary OMP session and record its verified Herdr identities.
- Dispatch independent work to isolated Herdr-managed worktrees and OMP worker sessions, subject to the coordinator's concurrency limit (three workers by default).
- Record worker progress, questions, blockers, and final reports durably; deliver notes to the primary without treating delivery or terminal activity as task completion.
- Let the primary review a final report and record an explicit outcome. Reconcile persisted facts with Herdr's live snapshot and worktrees after missed events or restarts.
- Provide a terminal board for task stages, details, and focusing a verified worker pane.
- Keep durable project tickets in `backlog` or `todo`. The board may create a ticket or move a backlog ticket to todo only through its injected `TicketOps`; task links and ticket stages are derived from recorded facts.

### Out of scope

- Multiple repositories under one ticket, remote workers, other agent harnesses, GitHub PR/CI/review automation, automatic merge or push, agent-to-agent hierarchy, mobile/browser UI, or a standalone network daemon.
- Treating Herdr's `idle`/`done` activity as evidence that work is complete. A task needs a final report and an explicit disposition.
- Automatic planning or worker startup when a ticket moves to `todo`. Phase 1 records the move only; the accepted design deferred the automatic planner pending issue [#4](https://github.com/striderZA/herdr-omp-orchestrator/issues/4), which remains open.

## Success run

A successful first task run starts a primary in a repository, asks it for two independent changes, and observes two isolated OMP workers and task cards. The primary handles a worker's question or a specifically approved blocked dialog. The workers return reports with evidence and check outcomes; the user reviews the results, sees the branches, and records dispositions. After detaching/reconnecting or restarting Herdr, the same task facts and board state are recovered without silently adopting an unknown agent or resending an input whose outcome is uncertain.

The ticket path is additive: create a backlog ticket, move it to `todo`, observe that this starts no task or process, then dispatch explicitly linked implementation work. After its tasks are closed, the user makes a separate ticket decision. A ticket does not become done just because its tasks or agents stop running.

## Ownership model

| Owner | Responsibility |
| --- | --- |
| **Herdr** | Persistent terminal processes, panes and workspaces, native worktree operations, focus, OMP agent lifecycle/session restore, and the plugin runtime environment. The plugin uses Herdr's CLI as the boundary for these operations. |
| **Plugin core (Go)** | The executable and routes, setup/bind/dispatch/review/cleanup policy, event observation and startup reconciliation, bridge authorization, durable project/task/ticket/inbox facts, and board projection. `internal/state` is the only persistence writer; `internal/coord` owns orchestration policy. |
| **OMP extension (TypeScript)** | The role-specific primary and worker tools and confirmation interactions. `omp/herdr-orchestrator.ts` is canonical; it sends the versioned, bounded bridge protocol to the Go executable. Setup installs this plugin's extension alongside, and never over, Herdr's OMP integration. |
| **Board** | Projects stored facts into a terminal view. It is read-only for tasks: it never writes or dispatches a task. Its only ticket writes are create and backlog-to-todo through `TicketOps`, backed by the coordinator. It may focus a verified worker pane, but does not call Herdr to write a ticket. |

## Safety invariants

1. **Facts before projections.** Persist task/ticket facts and explicit decisions. Derive stages from those facts. Agent activity, an idle session, and a closed pane do not complete work or a ticket.
2. **Identity from recorded facts.** Resolve project, task, and pane from the verified caller binding and persisted records, not caller-supplied IDs or labels. A bridge caller identity is attribution within one user account, not a defense against another process running as that same user; do not add a local secret that cannot protect this boundary.
3. **Fail closed on live identity.** Every bridge method routed through `mutate`—including previews and listings as well as writes—runs the role/readiness checks and the shared coordinator `agent get` confirmation; `resolve` and `ready` remain handshake methods. Refuse missing or contradictory agent/session/checkout/workspace evidence. Do not copy this check into individual operations.
4. **User confirmation for consequential actions.** Keep input injection, approval, primary replacement, and destructive cleanup behind the explicit user-confirmed paths. A stale plan/dialog or unknown identity must not be acted on. An ambiguous input-delivery result is not permission to blindly send it again.
5. **No silent replacement or forced deletion.** Reattaching a lost primary is explicit. Reconcile known facts; never respawn or adopt an unknown occupant. Remove only a positively identified, clean, unused checkout, without force-removing it or deleting its branch.
6. **One writer and narrow board authority.** Write durable facts only through `internal/state`. The board's ticket operations stay behind `TicketOps`; the board never gains a task write or dispatch path.
7. **Ticket move is not dispatch.** A backlog-to-todo move records a user decision only. It must not start a planner, worktree, or agent while automatic planning is deferred.

## Ticket design decisions (Phase 1)

These are the accepted Phase 1 decisions from the design for [issue #9](https://github.com/striderZA/herdr-omp-orchestrator/issues/9), preserved here because design drafts under `docs/superpowers/` are not a reliable tracked reference in a normal checkout:

- A ticket is a durable fact that can exist before a task. Its stored columns are only `backlog` and `todo`; subsequent stages derive from linked tasks and the ticket's own recorded decision. A task's immutable ticket link is set when the task is created.
- A human may create a ticket and move it from backlog to todo on the board. A primary may also create tickets; its move to todo requires the extension's preview-and-confirm interaction. Workers have no ticket authority. Ticket access is project-scoped.
- Moving to todo does not launch a planner in Phase 1. Planning is deferred; approving a plan does not auto-dispatch implementation work. The primary dispatches only after the human checkpoint.
- Ticket completion is a separate explicit review using the outcome set from [ADR 0001](adr/0001-disposition-contract.md), and is refused while a linked task is open. Completing the last linked task does not decide its ticket.
- No cross-project tickets or stored child-task list. Linked tasks, ticket stage, and board display are derived rather than duplicated.

For compatibility assumptions about Herdr's protocol, CLI answers, fixtures, and verification, start at [Herdr facts](herdr-facts.md). For the task outcome contract, see [ADR 0001](adr/0001-disposition-contract.md).

## Validation plan and requirement-to-test map

Use the broad suite for Go behavior, the focused extension load check for OMP's schema builder, and the Windows acceptance path for real Herdr/OMP integration. The exact development commands and prerequisites are in [AGENTS.md](../AGENTS.md). The map below names representative direct tests and the end-to-end acceptance IDs that exercise each contract; an ID is not evidence that a run passed unless that run's report says so.

| Requirement | Automated proof |
| --- | --- |
| Task stages come from reports and decisions, not agent activity; only accepting dispositions release dependents. | `go test ./...`: `internal/task.TestStageSeparatesActivityFromCompletion`, `internal/coord.TestDependentsReleaseOnlyOnAcceptingOutcomes`, `internal/coord.TestReportOnlyAPrimaryDispositionMovesATaskToDone`; acceptance `PR2.1–PR3.3` and `V1.2`. |
| Dispatch records isolated work and respects concurrency/dependencies; retries and restart recovery use recorded facts. | `go test ./...`: `internal/coord.TestConcurrencyCapsAtThreeAndQueuesTheFourth`, `internal/coord.TestRetryAtEachPersistedPhaseRepeatsNoSideEffect`, `internal/coord.TestReconcileRefusesToRekeyAnUnknownOccupantAndNeverRespawns`; acceptance `D1.2`, `D2.1`, `D3.1`, `T4.1–T5.2`. |
| Reports, questions, and worker identity are bound to the recorded task/session; pane moves keep launch identity distinct from current public identity. | `go test ./...`: `internal/coord.TestQuestionIsDurableDeduplicatedAndNeedsInput`, `internal/coord.TestReportMovesToReadyForReviewAndNotesOnce`, `internal/bridge.TestVerifyCallerIsTheSharedSeam`, `internal/coord.TestMoveWorkerUpdatesOnlyThePublicIdentityAndKeepsReporting`; acceptance `Q1.1`, `PR2.1–PR3.3`, `M4.1–M6.7`, `T5.project-session`. |
| Destructive or input-injecting actions fail closed and require the confirmed target/plan. | `go test ./...`: `internal/coord.TestReattachNeedsTheConfirmedPlan`, `internal/board.TestFocusReReadsTheTaskBeforeItActs`, `internal/bridge.TestLiveCallerCheckRefusesAWorkerHerdrNoLongerVouchesFor`; acceptance `K3.1–K7.2`, `C3.1–C3.4`, `C5.1–C5.3`, `C7.2–C7.4`. |
| Board task view is read-only; ticket changes go only through `TicketOps`; ticket move starts no work, ticket stage derives from linked facts, ticket decision is explicit, and workers lack ticket authority. | `go test ./...`: `internal/board.TestBoardNavigationMovesSelectionWithoutMutatingTasks`, `internal/board.TestMoveKeyActsOnlyOnASelectedBacklogTicket`, `internal/board.TestBoardWithoutTicketOpsStaysReadOnly`, `internal/task.TestTicketStageOf`, `internal/state.TestStoreConcurrentTicketUpdateHasOneWinner`, `internal/state.TestDecideTicketRefusesWhileALinkedTaskIsOpen`, `internal/coord.TestMoveTicketRecordsTodoAndStartsNothing`, `internal/coord.TestMoveTicketConcurrentHasOneWinner`, `internal/bridge.TestBridgeTicketMethodsArePrimaryOnly`, `internal/bridge.TestBridgeTicketRoundTrip`, `internal/bridge.TestBridgeTicketRefusalsAreCoded`; acceptance `A4.1`, `TK1.3`, `TK1.3e`, `TK1.4`, `TK1.5`, `TK1.6`, `TK1.6b`. |
| Setup uses the effective OMP profile without replacing Herdr's integration; extension schemas load under OMP's builder. | `go test ./...`: `internal/setup.TestInstallWritesOnlyItsOwnExtension`, `internal/setup.TestInstallIsIdempotent`, `internal/setup.TestVerifyNeverWrites`, and `internal/setup.TestExtensionLoadsUnderOMPSchemaBuilder`; direct load command: `bun run omp/load-extension.check.ts`; Windows acceptance `U2.1–U4.4`, `N7.4`. The Go load test skips if Bun or OMP's schema builder is unavailable. |
| Windows plugin build, real binding, restored identities, and available restored role tools. | Windows acceptance `B1.1–B1.4`, `N3.1–N5.6`, `T4.1–T8.2`; model pin checks include `S1.2b`, `N7.4`, and (when applicable) `SW0.1b` / `W2.<A|B>-model`. |
| PowerShell helper contracts for command-line quoting, process/env handling, path containment, redaction, projections, bridge envelopes, extension rendering, resume hints, and scoped cleanup. | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_helper_tests.ps1`; named helper checks include `Invoke-Native argv[1] spaces`, `hint: unmaterialized but offering a resume fails`, and `cleanup invariant includes Branch`. |
| Full user-visible lifecycle against real Herdr and OMP sessions, with isolated resources and explicit cleanup. | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1`; key IDs include `D1.2–D3.1`, `Q1.*`, `K3.1–K7.2`, `A2.columns`, `A4.1`, `TK1.1–TK1.6b`, `C3.1–C7.4`, and `T4.1–T8.2`. Run its helper suite separately. |

### Known gaps in automated proof

- **Standalone `omp --resume=<session>` without `--extension`: not directly exercised.** A static read of `tests/acceptance_windows.ps1` shows Herdr server restart/reconciliation and restored role-tool checks (`T4.1–T8.2`), but it does not run that OMP command as a standalone invocation without the flag. Do not claim that exact invocation is proven by the acceptance suite.
- **Non-Windows hosts: no automated proof and not a supported target.** The manifest is Windows-only; the current commands and stateful acceptance suite target Windows.
- **Human comprehension: not automatable.** Acceptance drives the same visible confirmation paths and checks their exact content (for example `K5.1–K5.3` and `TK1.3c`), but it cannot prove that a real user understood a prompt.
