# Repository Guidelines

## Project Overview

This is a Windows-only Herdr plugin that connects Oh My Pi (OMP) agents to a durable task orchestrator. The Go executable handles plugin actions, event/startup hooks, the interactive board (read-only for tasks; it creates and moves tickets through one narrow interface), and a versioned bridge used by the OMP TypeScript extension. Task progress is based on recorded reports and decisions—not terminal activity. Tickets (a backlog and a todo column) are durable facts that exist before any task; a ticket's stage is derived from the tasks linked to it and from the user's decision, never stored.

## Architecture & Data Flow

- `cmd/herdr-omp-orchestrator` is the single executable entry point. `herdr-plugin.toml` maps its subcommands to setup, bind, startup, event, board, reattach, and bridge routes. Herdr runs action commands without a terminal, so an action that needs the user's confirmation only opens a plugin pane (`open-board`, `open-reattach`) and the pane asks.
- `internal/bridge` validates the extension's versioned JSON stdin/stdout protocol and authorizes calls using inherited Herdr pane identity plus the OMP session identity. Keep bridge errors machine-readable and fail closed on unverified identity.
  - The coordinator's `agent get` confirmation runs on every call through `mutate` (previews, listings and unwired methods included) at one shared seam: `bridge.Ops.VerifyCaller`, wired by `CoordinatorOps` to `coord.Coordinator.VerifyCaller`. After the role and `Ready` checks and before the operation, it re-reads Herdr's live agent for the principal's recorded pane (the public pane, else the launch pane) and requires the recorded agent kind, a session reference the confirmed handshake proves is the recorded session, the recorded checkout as its cwd and the recorded workspace; a binding that records no checkout or workspace is refused. A mismatch, a vanished agent or a malformed answer is `agent_changed`; an unreachable Herdr is `herdr_unavailable`. It reads persisted facts only and writes nothing. `resolve` and `ready` are the handshake and stay outside it; an `Ops` with no `VerifyCaller` refuses every method. Do not copy the check into individual operations.
  - The primary's read-only views of its own project are the additive bridge methods `task_list` and `inbox_list` (tools `orch_task_list`, `orch_inbox_list`; protocol stays 1). They are `RolePrimary` only, run through the same `mutate` authorization, take the project from the verified principal, and write nothing: listing an inbox note never claims or settles it, so an `uncertain` note stays uncertain and its `deliveryDetail` is the primary-facing way to read why. Answers are bounded by a row limit (default 50, at most 200), a rune bound on each text field and a byte budget that accounts for JSON escaping, and a cut list says `truncated`. Open tasks and undelivered notes come first so the cut loses decided or delivered history before anything waiting.
  - Trust boundary: the bridge's caller identity is an *attribution* boundary inside one user account, not a security boundary against it. `HERDR_ENV`, `HERDR_PANE_ID`, `HERDR_PLUGIN_STATE_DIR` and the announced OMP session are all supplied by the caller, and a same-user process can already read the store and drive any pane through Herdr's own CLI, so spoofing them grants no authority the account lacks. Do not add a local secret (it would be readable by the same user) or treat the bridge as defending against that attacker. Instead keep the blast radius bounded: every operation must derive its project, task and pane from persisted facts (a caller that names one in `params` changes nothing), keep the role check and the coordinator's `agent get` confirmation on every mutating call, and keep destructive or input-injecting steps behind explicit user confirmation.
- `internal/coord` implements orchestration: bind/dispatch/review/cleanup, event observation, inbox delivery, and startup reconciliation. Events are one-shot observations; startup re-derives live identity from Herdr's snapshot and worktrees. A primary that is absent or unproven is recorded on the project (`task.Project.PrimaryAttention`) by reconciliation and the paths that probe the primary, and cleared once the primary is verified live; the board only reads that fact and draws a marker, and recording it changes no task, stage or inbox note.
  - `Bind` verifies its prerequisites before it records or starts anything: `coord.Options.Preflight` (wired by the executable to the read-only `setup.Verify`) must prove Herdr's OMP integration and the plugin's own extension are present in the effective OMP profile, or `Bind` refuses with `coord.ErrNotReady` (answered with `setup`'s own `integration_missing` / `extension_missing` code and the remedy) before `CreateProject`, workspace creation, any pane lookup or agent start. The check runs for a re-bind of a recorded project too, since reopening can start the primary's agent. `setup.Verify` shares `checkIntegration` with `Install`, never writes, and does not judge whether the installed extension is current; the extension's runtime `ready` handshake remains the second check. A coordinator built without a `Preflight` refuses to bind (`ErrNotReady`) instead of skipping the check, so the gate cannot be bypassed by omission; no other operation consults it.
  - A pane names one binding. `state.Store.CreateProject` refuses, inside its transaction, a launch or public pane that another project or any task records (`state.ErrPaneTaken`, deliberately not `ErrConflict`), and `UpdateProject` refuses moving a project onto one; a write that keeps the panes is never refused for it. `Bind` does not adopt a named pane that any record claims (it creates a dedicated workspace instead), and a store refusal surfaces as `coord.ErrAlreadyBound` (`already_bound`). Databases written before this still open: a launch pane two projects record makes `ProjectByLaunchPane`/`ByPane` fail closed with `state.ErrAmbiguous`, and the bridge resolves it as `ambiguous_session` without adopting a session.
  - Reattaching a lost primary (`coord.PlanReattach` / `coord.Reattach`, routes `open-reattach` and `reattach`) is the one explicit, user-confirmed way to move a project to a replacement pane; `Bind` keeps refusing silent substitution and its refusals name the action. The plan is read-only and is what the user confirms; `Reattach` re-derives it and refuses unless it still matches, records the binding in one compare-and-swap of the project row (the displaced identities go to `task.Project.Replaced` in the same write), and then confirms the handshake with one notice to the replacement session plus `awaitAgentReady`. The write precedes the notice because the replacement's extension can only announce once the project records its session, so a failure afterwards is `ErrReattachPending` with the binding left recorded and marked in `PrimaryAttention` - never reported as success. The notice is an input injection, so its attempt (`task.Project.ReattachNotice`, with the pane, session and session-reference kind it went to) is persisted before the prompt is sent and follows the inbox rule across retries: a notice Herdr acknowledged (`sent`) or whose outcome is unknown (`uncertain`: an ambiguous or unreadable answer, or a stopped process) is never sent again by a later reattach, which only waits for the handshake. Only a refusal that proves nothing was written withdraws the attempt. Identity checks fail closed: a recorded session and Herdr's reference are only compared in the same form (id or session file), so a recorded pane whose session is now reported in the other form is refused unless a verified handshake proves it is a different session; and the flow goes on to the notice only when the readiness store reports no announcement for the pane (`state.ErrNotFound`) or an earlier session's (`state.ErrMismatch`) - any other confirmation failure, including `ErrAgentChanged` after a recorded confirmation, sends nothing. If the session still has not announced, the user sends it a message and `bind` confirms it. Reattach never replaces a primary that is verified live and never adopts a pane or session that a worker or another project owns; running it again on a recorded replacement does not record the displaced primary twice.
- `internal/herdr` is the CLI boundary: execute argv directly (never through a shell), decode Herdr's JSON into typed values, and leave policy/state changes to the coordinator.
- `internal/state` is the only writer of durable orchestrator facts (tasks and tickets), stored in SQLite under Herdr's plugin state directory. `internal/task` defines those facts and derives the board stage (and a ticket's stage from its linked tasks). `internal/board` presents a view that is read-only for tasks: the human can create a ticket and move a backlog ticket to todo only through its injected `TicketOps`, which the executable backs with `internal/coord`; the board never writes a task, never dispatches, and never calls Herdr for a ticket write. `internal/setup` installs the extension without overwriting Herdr's integration.
- Data generally flows from plugin/extension input → `bridge` or CLI route → `coord` policy → `state` persistence, with the board projecting persisted facts back to the user (and, for tickets only, writing through `coord` via `TicketOps`). `omp/herdr-orchestrator.ts` is the canonical extension source; edit it here, then rerun setup to refresh the installed copy.

## Key Directories

- `cmd/herdr-omp-orchestrator/` — process routing and CLI tests.
- `internal/` — Go domain, persistence, CLI integration, orchestration, bridge, setup, and board packages; each package has adjacent `_test.go` files.
- `omp/` — canonical OMP extension and its schema/load check.
- `tests/` — Windows PowerShell helper-contract and full acceptance suites.

## Development Commands

Run from the repository root:

```sh
go test ./...
go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator
```

The plugin manifest builds without CGO; on Windows the equivalent is:

```powershell
$env:CGO_ENABLED = '0'
go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator
```

Windows acceptance checks:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_helper_tests.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1
```

The full acceptance test drives real Herdr and OMP sessions in an isolated named session and temporary worktree root; it supports `-TimeoutScale`, `-KeepResources`, `-SkipStateCleanup`, and `-Model`. `-Model` (default `openai-codex/gpt-6-luna`, spelled as `omp --model` spells it; `-Model ''` keeps OMP's own default) pins the primary and every worker to one model without touching the user's OMP settings: the run writes a one-file OMP config overlay into its temp root and names it in `PI_CONFIG_FILES` on the isolated Herdr server, whose agents inherit it. The run first checks that OMP resolves the overlay (`S1.2b`, fail-closed), then reads each session's transcript to assert every assistant turn used that model (`N7.4`, `SW0.1b`, `W2.<A|B>-model`). With a model pinned those checks fail closed: a session whose transcript cannot be read, or that ran any turn on another model, is a FAIL, not an INFO. With no model pinned they only record the model OMP chose. The extension load check can also be run with Bun and OMP's schema builder available: `bun run omp/load-extension.check.ts` (see `omp/load-extension.check.ts` for builder resolution options).

## Git Hooks

Enable the repository hooks once per clone:

```sh
git config core.hooksPath .githooks
```

The hooks require Conventional Commit subjects (`<type>(optional-scope): <short description>`) and reject staged files under `docs/superpowers/` or `.superpowers/`.

## Code Conventions & Common Patterns

- Use standard Go formatting and idioms; keep package responsibilities and existing boundaries intact. Test files use Go's `testing` package and are named beside the implementation as `*_test.go`.
- Pass `context.Context` through external operations and bound subprocess/protocol calls. Herdr commands use `exec` with argv, not shell strings; enforce input/output limits where the protocol defines them.
- Compare stable errors by code or with `errors.Is`/`errors.As`, not by matching prose. Preserve the bridge's JSON envelope and protocol version when changing either side of the Go/TypeScript seam.
- Treat durable records as facts and derived views as projections. Do not infer task completion from Herdr's idle/done activity; a final report and explicit disposition drive the task lifecycle. Event hooks apply one observation; reconciliation handles missed events/restarts.
- A final report's checks are `task.Check` values that each state an outcome (`passed`, `failed` or `skipped`); `coord.validateReport` is the one place that requires it (ADR 0003). A report stored with free-text checks decodes to checks with no outcome and is rendered as recorded: never invent an outcome for one. Change the extension schema, the worker instructions and the Go validation together.
- Keep `internal/state` as the persistence writer. Inject narrow client interfaces where a package needs Herdr behavior, and maintain fail-closed identity checks and explicit user confirmation for input injection or destructive cleanup.
- TypeScript schemas must use factory defaults for mutable values (for example, `.default(() => [])`), not shared mutable defaults.

## Important Files

- `go.mod` / `go.sum` — Go module and dependency versions; module declares Go 1.27.1.
- `herdr-plugin.toml` — plugin ID, Windows platform, minimum Herdr version, build command, hooks, actions, and pane routes.
- `cmd/herdr-omp-orchestrator/main.go` — subcommand routing and route-level bounds.
- `internal/bridge/bridge.go` — extension protocol and caller authorization.
- `internal/coord/` — task lifecycle and reconciliation behavior.
- `internal/state/sqlite.go` / `internal/task/model.go` — durable schema and domain projections.
- `omp/herdr-orchestrator.ts` — canonical OMP extension source; `omp/load-extension.check.ts` validates it against OMP's actual schema builder.

## Runtime/Tooling Preferences

- Primary runtime/toolchain: Go 1.27.1. Dependencies are managed by Go modules (`go.mod`, `go.sum`); there is no Node package manifest or separate Go lint/formatter config in the repository.
- Target platform is Windows (`herdr-plugin.toml`); the SQLite driver is pure Go, and the plugin build uses `CGO_ENABLED=0`.
- Bun is needed only for the TypeScript extension load check. That check also needs OMP's schema builder available locally; the corresponding Go test skips when Bun or the builder is missing.
- PowerShell 5.1 or newer runs the acceptance scripts. Avoid editing Herdr's installed integration or the installed extension copy directly; setup owns installation of the extension source.

## Testing & QA

- `go test ./...` runs package-level tests. Tests use temporary SQLite databases, fixture data, and—in coordinator tests—a real temporary Git repository plus a child process acting as a Herdr CLI stand-in, so retry and argv behavior are exercised at the boundary.
- `tests/acceptance_helper_tests.ps1` tests pure helper contracts without Herdr, an OMP session, or network access. `tests/acceptance_windows.ps1` is the stateful end-to-end acceptance path and cleans up only resources it created unless explicitly told to keep them.
- Extension loading is verified with OMP's real schema builder via `omp/load-extension.check.ts`; the Go setup test invokes it when local prerequisites exist.
- When changing protocol behavior, lifecycle rules, or state migrations, update the affected package tests and preserve fail-closed behavior; run the focused package test(s) and the relevant helper/load/acceptance check for the changed behavior.
