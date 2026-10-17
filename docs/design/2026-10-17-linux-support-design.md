# Linux support for the OMP orchestrator (issue #83)

Issue: #83. Base: `f19289d`. Status: ready for your approval; builder check done and your answers applied. Date: 2026-10-17.

## Your answers and the calls still open

- **D1 manifest:** your pick C. The root `herdr-plugin.toml` stays Windows-only and unchanged. Criterion 1's letter is not met; the Linux manifest meets it in substance.
- **D4 reserved paths:** you confirmed folding case on every OS.
- **Scope:** review, reattach and the board UI are out of scope for the Linux verification. The issue records that.
- **Smoke model:** your configured default, `anthropic/claude-haiku-5-5:max`.
- **Real install and default server:** the plan stops and asks before writing OMP's integration or starting your default server.
- **jq:** real jq 1.8.1 is installed at `/usr/bin/jq`. The installer tests run against it.
- **Still open, for your approval with this document:** D2 identity, D3 worker stdio, D5 brief cap, the smoke's bridge dispatch (it skips the consent dialog), and the `docs/design/` location.

## Intent

The plugin is built, declared and installed for Windows only. Herdr publishes Linux builds for x86_64 and aarch64, so a Linux user of Herdr who runs OMP agents has no supported way to install or run the orchestrator. This design lets a Linux x86_64 user run the same workflow as on Windows, on Herdr's Linux build.

## Goals

- A Linux user installs with `install.sh`, which mirrors `install.ps1`.
- Setup, bind, dispatch, restart reconciliation and ticket moves work on Linux, as proven by the smoke path.
- Process identity on Linux distinguishes a reused PID from the original process.
- `go test ./...` passes on Linux. Each baseline failure is fixed, given a Linux equivalent, or skipped with a stated reason.
- A Linux smoke path runs on real Herdr and OMP sessions.
- The release publishes a Linux amd64 asset.

## Non-goals

- macOS: refused in code and by `install.sh`.
- arm64: not built, tested or documented as supported. The README says so.
- A Linux port of the PowerShell acceptance suite. The smoke path covers setup, bind, dispatch, restart and ticket move.
- Review, reattach and the board UI on Linux. They are out of scope for this issue and recorded on it.
- Changes to the OMP extension, Herdr's protocol (22), or Windows behavior: binary name, manifest, installer, identity or worker.

## Anti-goals (failure even if the tests pass)

- A Windows user sees a new action, pane, id or binary name in Herdr.
- A Linux worker dies silently after its route returns.
- An identity check reports a live owner dead, so its claim is taken over.
- `install.sh` overwrites Herdr's OMP integration.
- A Linux path counts as covered because its test was skipped.

## Verified facts

Environment: Herdr 0.9.5 on Linux x86_64 (WSL2, kernel 6.18), go1.27.1 (fetched from `go.mod`). Probes ran in an isolated named session (`--session`). Afterward the probe links were removed, the registry was empty, the session was deleted, and the default Herdr server was never started. The shell tool's `jq` is a builtin that reports jaq; the real `jq` is 1.8.1 at `/usr/bin/jq`, which scripts use.

| # | Fact | Evidence |
|---|---|---|
| F1 | Protocol 22 on Linux | `herdr api schema` and a live `api snapshot` on the isolated server |
| F2 | Duplicate action ids are rejected at link, even across platforms | `plugin link` failed with `duplicate action id 'probe'` |
| F3 | Unique ids with item-level `platforms` link and run per platform | The Linux entry ran. The Windows entry was refused with `platform_unsupported` |
| F4 | `plugin action list` shows entries for every platform | Both entries were listed on Linux |
| F5 | A plugin whose `platforms` excludes Linux still links, with no refusal | `platforms = ["windows"]` linked and enabled |
| F6 | The plugin registry is global across sessions | The isolated session listed the default session's link |
| F7 | `plugin link` and `plugin list` work with no server. `plugin action list` and `plugin unlink` return `server_not_running` without one. `invoke` was tested only with a server | Observed errors |
| F8 | A `Setsid` child survives its action. With a Go pipe as stderr, the child never completes a late stderr write. With `/dev/null` it completes it | Discard child wrote `alive`, not `stderr-ok`. Devnull child wrote both |
| F9 | `CLK_TCK` is 100 | `getconf CLK_TCK` |
| F10 | Herdr's OMP integration is not installed. `~/.omp/agent/extensions` does not exist | `herdr integration status`, and a listing of `~/.omp/agent` |
| F11 | Baseline `go test ./...` on Linux: 11 failing tests in 5 packages, 265 s | Appendix |
| F12 | Python 3 with its `sqlite3` module (SQLite 3.46.1). No `sqlite3` CLI | Probe |
| F13 | OMP's configured roles: default `anthropic/claude-haiku-5-5:max`, advisor `openai-codex/gpt-6-luna:max` | `omp config get modelRoles --json` |

## Decisions

### D1 Manifest: a Linux manifest beside the Windows one (your pick C)

- `herdr-plugin.toml` (root) is unchanged, with `platforms = ["windows"]`.
- New `linux/herdr-plugin.toml`: the same `id`, `name`, `version` and `min_herdr_version`, with `platforms = ["linux"]`. It has the same action, pane, startup and event entries, with the same ids, titles, contexts, placements and `on` kinds. Commands name `./herdr-omp-orchestrator`, with no `.exe`. It has no `[[build]]`: `install.sh` builds, and a build step inside `build/linux-plugin` would have no source tree.
- A Go test compares the two manifests. Ids and fields match. Commands match after removing `.exe`. Versions match. Windows keeps its build step. Linux has none.
- `install.sh` copies `linux/herdr-plugin.toml` to `build/linux-plugin/herdr-plugin.toml`, builds the binary there, and links that folder.
- `.gitignore` gains `/build/` and `/herdr-omp-orchestrator`.
- Consequence: the root manifest does not declare Linux. Issue #83's criterion 1 needs its wording updated.

### D2 Process identity on Linux

- New `internal/state/process_linux.go` (`//go:build linux`). `StartTime` is field 22 of `/proc/<pid>/stat`, in clock ticks since boot.
- Parse the text after the last `)`, because the command name can contain spaces and parentheses. The first field there is the state. The start time is the 20th field after the `)`, which is field 22 overall.
- `ProcessAlive`: `ENOENT` means not alive. Any other read or parse error is returned as an error, so the claim is kept. State `Z` or `X` means not alive: exited and unreaped, the analog of the Windows exit check. A different start time means not alive, a reused PID. Otherwise alive.
- `CurrentProcessIdentity` reads this process's own stat line.
- `internal/state/process_other.go` becomes `//go:build !windows && !linux` and returns an error from both functions. Claims and workers then refuse on macOS and other systems.
- Residual: the identity is unique within one boot. After a reboot, a recorded PID and start time can match a new process. That reads as alive, so the claim is kept. This fails closed, and the claim can stay stuck until that process exits. Accepted.
- Rejected: absolute time from `/proc/stat` `btime`. [INFERENCE: `btime` is derived from the wall clock, so a clock step could make a live owner look dead. Not probed.]

### D3 Detached recovery worker on Linux

- New `cmd/herdr-omp-orchestrator/recovery_process_linux.go` (`//go:build linux`): `SysProcAttr{Setsid: true}`, with stdio left nil so os/exec opens `/dev/null`.
- The shared launcher stops assigning stdio. Each platform file owns it. Windows keeps `io.Discard` for stdout and stderr exactly as today, and its breakaway flags do not change.
- `recovery_process_other.go` becomes `//go:build !windows && !linux`, and it keeps refusing.
- Why `/dev/null`: F8. A worker that writes an error to stderr after its parent exits is killed mid-write, before its deferred cleanup runs.
- Tests: a Linux counterpart of `TestDetachedRecoveryChildSurvivesTheForegroundRoute`. The detached child writes a file after the parent exits, then writes to stderr, then writes a second file. The Windows tests stay as they are.
- `TestDetachedRecoveryWorkerDoesNotPinTheCallersDirectory` stays Windows-only. Its skip reason changes to say the pin matters on Windows, where a process holds its working directory open. [POSIX behavior, not probed: Linux lets a directory be removed while it is some process's working directory.]

### D4 Reserved paths fold case on every OS (you confirmed)

- `coord/dispatch.go`: `normalizeReservedPath` always lowercases. `isCaseInsensitivePath` is deleted.
- `state/sqlite.go`: `pathsOverlap` always lowercases. The `runtime.GOOS` check goes.
- Why: project keys (`normalizedKey`), worker names and primary names already fold case on every OS. Two unguarded tests already expect case-insensitive conflicts (`gate_test.go:208`, `dispatch_test.go:2109`). Over-refusal is the fail-closed direction.
- Cost: on a case-sensitive Linux directory, `Notes.md` and `notes.md` conflict. The refusal names both paths.
- Unchanged: comparisons of identities (Herdr checkout paths, bind, session references) stay platform-correct.

### D5 Planner brief cap: unchanged

The cap stays at 14,000 bytes on every platform. Linux's argument limit is larger, so the cap is safe. Reword the comment at `coord/tickets.go:325-328` and `docs/intent.md:53` to say it is the shared bound set by the Windows command line.

### D6 Installer: `install.sh` (your pick 2a)

It mirrors `install.ps1`'s steps and messages, with `set -euo pipefail`. The repository is the folder that holds `install.sh`, whatever the working directory.

1. Preflight: `uname -s` must be `Linux`, so macOS is refused. `herdr`, `go` and `jq` must be on `PATH`. Read the current link from `herdr plugin list --json`, and refuse if it links another folder.
2. `[1/4]` Copy the Linux manifest into `build/linux-plugin/`. Build `herdr-omp-orchestrator` there with `CGO_ENABLED=0`.
3. `[2/4]` Run `herdr integration status`. Run `herdr integration install omp` only when the `omp:` line says not installed, then check again.
4. `[3/4]` Run `herdr plugin link build/linux-plugin`. Enable the plugin if it is disabled. Refuse on manifest warnings or a missing `setup` action.
5. `[4/4]` Run `herdr plugin action invoke setup --plugin user.omp-orchestrator`. Poll `herdr plugin log list --plugin user.omp-orchestrator --limit 100` until the entry finishes, up to 120 seconds. Parse the stdout with `jq`.

It never unlinks, and it never overwrites Herdr's integration. Step 4 needs a running Herdr server (F7), as `install.ps1` does.

Tests: `tests/install_linux_tests.sh` uses stub `herdr` and `go` on `PATH`, as `tests/install_tests.ps1` does, and runs the scenarios that apply on Linux. It runs against real `jq` 1.8.1.

### D7 Smoke path: `tests/acceptance_linux.sh` (your pick 3b)

It requires `install.sh` to have run: the plugin is linked to `build/linux-plugin`, enabled, with no warnings. The script never links or unlinks, as the Windows suite does.

Isolation, as the Windows suite does it:
- Server: `herdr --session orch-acc-linux-<hex> server`, with `HERDR_CONFIG_PATH` pointing to a temp config whose worktree directory lies inside the run's temp root.
- Model: an OMP config overlay in the run's temp root, named in `PI_CONFIG_FILES` on the isolated server. The run checks that OMP resolves the overlay, and that every transcript used the pinned model. The pin is `anthropic/claude-haiku-5-5:max`, your configured default (F13).
- The default session is never addressed. When its server is running, the run compares the default session's projection before and after.

Steps and assertions:
1. `setup` on the isolated server answers `ok`.
2. `bind` on a pane at the root of a temp git repository records a project. The primary's session announces.
3. Dispatch goes through the bridge as the verified primary, using the primary's pane and session identity. Those are the same environment variables the Windows suite's bridge calls set. This is the call the extension makes after the user confirms. The consent dialog is not exercised here.
4. The worker's checkout exists under the temp root. Its pane reaches idle with a verified session.
5. Ticket move through the bridge (`ticket_move`). It arms the same recoverable worker that the board's ticket moves arm, through `startRecoveryWorker`. The board UI is not driven.
6. Stop the server, then start it again. Startup reconciles, and the project and task still resolve to their live pane and session.
7. Cleanup: stop the server, delete the session, and remove the worktrees and temp root the run created.

State: the plugin state store is shared. The run deletes only rows for its own project key and panes, and proves every other row is unchanged, as the Windows suite does (`Z4.*`). `--keep-resources` and `--skip-state-cleanup` match the Windows flags. Python's `sqlite3` module does the work (F12).

Output: one line per assertion, `PASS`, `FAIL` or `INFO`, with an id, as in the Windows suite.

Out of scope, recorded on the issue: review, reattach and the board UI. The smoke makes no direct event-hook assertion. Event delivery shares the Windows code path, and the smoke may assert it if a status change happens during the run.

### D8 Release: amd64 (your pick 4)

- `release.yml` gets a `linux-amd64` matrix entry on `ubuntu-24.04`: `GOOS=linux`, `GOARCH=amd64`, `CGO_ENABLED=0`, `-trimpath`, the same `-ldflags`, output without `.exe`, and a `--version` check.
- The publish job's `ASSETS` gains `herdr-omp-orchestrator-linux-amd64`. `SHA256SUMS` covers both binaries.
- The tag check also reads `linux/herdr-plugin.toml` from the tagged commit and requires the same version.
- The Windows job is unchanged. arm64 is not built.

### D9 Documentation

- `README.md`: Windows and Linux (x86_64). A Linux install section. `jq` in the requirements. arm64 untested. macOS unsupported.
- `AGENTS.md`: platform statements. `install.sh`, `linux/herdr-plugin.toml` and the parity test. The Linux build command. The Linux install and smoke tests. The Linux release asset.
- `docs/intent.md`: lines 9, 53 and 105.
- `docs/herdr-facts.md`: a 2026-10-17 entry for the Linux facts, F1 to F8.
- `docs/adr/0008-linux-support.md`: decisions D1 to D5.
- Issue #83: a comment at closeout, recording the out-of-scope items and criterion 1's wording (D1).

## Acceptance mapping (issue #83)

| Criterion | Proof |
|---|---|
| 1 Manifest | Parity test. Install step 4 links `build/linux-plugin` with no warnings and lists `setup`. Setup invoke runs (F3) |
| 2 Identity | Linux state tests: reused PID, zombie, stat parsing. Existing cross-platform identity tests |
| 3 Startup, bridge routes, board | Linux survival test. Smoke step 5 (ticket move, the same worker the board arms) and step 6 (startup reconcile) |
| 4 `install.sh` | `tests/install_linux_tests.sh`. A real run on this machine, with your approval |
| 5 `go test` | A recorded Linux run with 0 failures. Skips listed with reasons |
| 6 Linux acceptance | `tests/acceptance_linux.sh` as in D7. Review, reattach and the board UI are recorded on the issue as out of scope |
| 7 Docs | Reviewed, not tested |
| 8 Release | The workflow runs on a tag. Locally, reproduce the build and `--version` |

Cross-checks: `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...` compile the test code for those platforms. Neither runs here.

## Test changes for the 11 baseline failures

| Test | Fix | Kind |
|---|---|---|
| `state/process_test.go:61` reused PID | Linux identity (D2) | product |
| `state/gate_test.go:210` `NOTES.md` vs `notes.md` | Fold case (D4) | product |
| `coord/dispatch_test.go:2112` `SRC\MAIN.TXT` | Fold case (D4) | product |
| `state/process_test.go:187` Wait error after Kill | After `Kill`, ignore the Wait error. Its text differs by platform: `exit status 1` on Windows, `signal: killed` on Unix | test |
| `coord/dispatch_test.go:3050` primary name stable | Use forward-slash spellings on every platform. Keep the backslash spelling as a Windows-only case | test |
| `coord/canonical_root_test.go:41` error type | Assert `errors.Is(err, fs.ErrNotExist)`. It keeps the cause and is portable | test |
| `task/model_test.go:120` other separators | Windows-only case for separators. On Linux, use a spelling that `filepath.Clean` maps to the same path, such as `//` | test |
| `setup/extension_test.go:901` `PI_CODING_AGENT_DIR` | Fixture uses an absolute path from `t.TempDir()` | test |
| `setup/extension_test.go:1082` "unescaped" literal | Delete the source-text assertion. The decode round trip at line 1078 is the behavior. On non-Windows, add `"` and `\` to the odd directory name so escaping is exercised | test |
| `herdr/client_test.go:516` worktree open decode | On non-Windows, the request uses the response's own spelling. The Windows backslash request stays for Windows | test |
| `herdr/client_test.go:689` backslash spelling | Move the backslash case into the Windows-only block | test |

Incidental assertions, such as the `*exec.Error` type check and the `exit status` text, are replaced with behavior. They are not re-pinned.

New Linux tests cover behavior, not wiring: stat-line parsing with a command name containing spaces and `)`; an unreaped killed child reads as not alive; the late-write survival test (D3); the manifest parity test (D1); the install stub scenarios (D6).

## Risks and residuals

- Reboot identity collision (D2).
- Case-folded reservations refuse distinct files on case-sensitive filesystems (D4).
- A backslash in a reserved path is a separator on Linux. `normalizeReservedPath` already converts `\` on every OS. Unchanged.
- The root manifest, if linked by hand on Linux, is inert: its commands are `.exe` and Windows-only. `install.sh` refuses to link over another folder, so the user must unlink first.
- `install.sh` is tested against real `jq` 1.8.1. Other `jq` versions are not checked.
- arm64 is untested.
- The probes covered action runners only (F8), not the startup or event runners. The smoke restart covers startup.
- Rebuilding `build/linux-plugin/herdr-omp-orchestrator` while a board pane runs it is untested. The plan verifies it.
- Review, reattach and the board UI are out of scope for this issue and not exercised on Linux. They share Go code with Windows, and the platform-specific parts are identity, the worker and paths.

## Open items (needed before implementation)

1. **Real install gate.** The plan stops and asks before writing OMP's integration into `~/.omp/agent/extensions/`, linking the plugin in your Herdr registry, or starting your default Herdr server.
2. **Load check.** `omp/load-extension.check.ts` skips because `~/node_modules/@oh-my-pi/omptype` is missing. The extension is not changed, so the skip stays unless you want that builder installed.
3. **Approval of the open calls.** D2, D3, D5, the smoke's bridge dispatch, and the `docs/design/` location.

## Left to the plan

File-level edits and their order. Exact test names and files. The shape of the stat parser. The parity test's comparison. The install script's structure and its `jq` filters. The smoke script's helpers and its Python state helper. The ADR text. Verifying a rebuild while a board runs.

## Appendix: baseline failures

`go test -count=1 -timeout 30m ./...` on Linux at `f19289d`, go1.27.1, 265 s. Eleven failing tests: `state` (3), `coord` (3), `herdr` (2), `setup` (2), `task` (1). The table above lists each one with its fix.
