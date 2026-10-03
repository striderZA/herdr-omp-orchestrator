# Repository Guidelines

## Project Overview

This is a Windows-only Herdr plugin that connects Oh My Pi (OMP) agents to a durable task orchestrator. The Go executable handles plugin actions, event/startup hooks, the interactive task board, and a versioned bridge used by the OMP TypeScript extension. Task progress is based on recorded reports and decisions—not terminal activity.

## Architecture & Data Flow

- `cmd/herdr-omp-orchestrator` is the single executable entry point. `herdr-plugin.toml` maps its subcommands to setup, bind, startup, event, board, and bridge routes.
- `internal/bridge` validates the extension's versioned JSON stdin/stdout protocol and authorizes calls using inherited Herdr pane identity plus the OMP session identity. Keep bridge errors machine-readable and fail closed on unverified identity.
  - Trust boundary: the bridge's caller identity is an *attribution* boundary inside one user account, not a security boundary against it. `HERDR_ENV`, `HERDR_PANE_ID`, `HERDR_PLUGIN_STATE_DIR` and the announced OMP session are all supplied by the caller, and a same-user process can already read the store and drive any pane through Herdr's own CLI, so spoofing them grants no authority the account lacks. Do not add a local secret (it would be readable by the same user) or treat the bridge as defending against that attacker. Instead keep the blast radius bounded: every operation must derive its project, task and pane from persisted facts (a caller that names one in `params` changes nothing), keep the role check and the coordinator's `agent get` confirmation on every mutating call, and keep destructive or input-injecting steps behind explicit user confirmation.
- `internal/coord` implements orchestration: bind/dispatch/review/cleanup, event observation, inbox delivery, and startup reconciliation. Events are one-shot observations; startup re-derives live identity from Herdr's snapshot and worktrees.
- `internal/herdr` is the CLI boundary: execute argv directly (never through a shell), decode Herdr's JSON into typed values, and leave policy/state changes to the coordinator.
- `internal/state` is the only writer of durable orchestrator facts, stored in SQLite under Herdr's plugin state directory. `internal/task` defines those facts and derives the board stage. `internal/board` presents a read-only view; `internal/setup` installs the extension without overwriting Herdr's integration.
- Data generally flows from plugin/extension input → `bridge` or CLI route → `coord` policy → `state` persistence, with the board projecting persisted facts back to the user. `omp/herdr-orchestrator.ts` is the canonical extension source; edit it here, then rerun setup to refresh the installed copy.

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

The full acceptance test drives real Herdr and OMP sessions in an isolated named session and temporary worktree root; it supports `-TimeoutScale`, `-KeepResources`, and `-SkipStateCleanup`. The extension load check can also be run with Bun and OMP's schema builder available: `bun run omp/load-extension.check.ts` (see `omp/load-extension.check.ts` for builder resolution options).

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
