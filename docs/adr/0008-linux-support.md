# ADR 0008: Linux support (x86_64)

Status: Accepted for issue #83. The Windows behavior it describes is unchanged.

## Context

The plugin was built, declared and installed for Windows only. Herdr publishes Linux builds for x86_64 and aarch64, so a Linux user of Herdr who runs OMP agents had no supported way to install or run the orchestrator (issue #83). The design (`docs/design/2026-10-17-linux-support-design.md`) verified the Herdr behavior the Linux path depends on (its facts F1 to F8, recorded in [Herdr facts](../herdr-facts.md#linux-issue-83-2026-10-17)) and chose the decisions below. Linux support is x86_64 only. arm64 is untested, and macOS is refused in code and by `install.sh`.

## Decision

1. **D1 Manifest: a Linux manifest beside the Windows one.** The root `herdr-plugin.toml` stays Windows-only and unchanged. `linux/herdr-plugin.toml` has the same `id`, `name`, `version` and `min_herdr_version`, `platforms = ["linux"]`, the same action, pane, startup and event entries, and commands that name `./herdr-omp-orchestrator` with no `.exe`. It has no `[[build]]`: `install.sh` builds the binary into `build/linux-plugin`. A Go test (`cmd/herdr-omp-orchestrator/manifest_test.go`) compares the two manifests and pins the root manifest to Windows.
2. **D2 Process identity on Linux.** `internal/state/process_linux.go` takes a process's start time from field 22 of `/proc/<pid>/stat`, in clock ticks since boot, read from the text after the last `)` so a command name with spaces or parentheses parses. A missing `/proc` entry means not alive. Any other read or parse error is returned, so the claim is kept. State `Z` or `X` means not alive. A different start time means the PID was reused, so not alive. Other systems return an error from both identity functions, so claims and workers refuse there. Rejected: an absolute time from `/proc/stat` `btime`, because it is derived from the wall clock and a clock step could make a live owner look dead.
3. **D3 Detached recovery worker on Linux.** `cmd/herdr-omp-orchestrator/recovery_process_linux.go` sets `SysProcAttr{Setsid: true}` and leaves stdio nil, so `os/exec` opens `/dev/null`. The shared launcher no longer assigns stdio; each platform file owns it. Windows keeps `io.Discard` for stdout and stderr and its breakaway flags. A worker that writes to a Go pipe after its parent exits is killed mid-write (F8), so `/dev/null` is the stdio the Linux worker needs.
4. **D4 Reserved paths fold case on every OS.** `coord/dispatch.go`'s `normalizeReservedPath` always lowercases, and `state/sqlite.go`'s `pathsOverlap` always lowercases. Project keys, worker names and primary names already fold case on every OS, and two existing tests already expect case-insensitive conflicts. Over-refusal is the fail-closed direction. Comparisons of identities (Herdr checkout paths, bind, session references) stay platform-correct.
5. **D5 Planner brief cap: unchanged.** The cap stays at 14,000 bytes on every platform. It is the Windows command-line bound (`CreateProcessW`'s 32,767-character limit, less room for quoting and the other arguments). Linux's argument limit is larger, so the cap is safe there. The comments in `internal/coord/tickets.go` and `docs/intent.md` say so.
6. **D6 Installer: `install.sh`.** It mirrors `install.ps1`'s steps and messages, with `set -euo pipefail`. Preflight requires `uname -s` to be `Linux` and `herdr`, `go` and `jq` on `PATH`, reads the current link from `herdr plugin list --json`, and refuses if Herdr links another folder. It then copies `linux/herdr-plugin.toml` into `build/linux-plugin/`, links `omp/` into it (setup reads its extension source from the plugin root), builds with `CGO_ENABLED=0`, checks `herdr integration status` and installs the integration only when it reports `omp` not installed, links `build/linux-plugin`, enables the plugin if disabled, and runs the `setup` action, reading its answer from the plugin log. The script never unlinks and never overwrites Herdr's integration. Its stub-based tests are `tests/install_linux_tests.sh`.
7. **D7 Smoke path: `tests/acceptance_linux.sh`.** It requires `install.sh` to have run, and it never links or unlinks. It runs an isolated Herdr server in a named session with a temp config, pins the model through an OMP config overlay (default `anthropic/claude-haiku-5-5:max`, the configured default), and covers setup, bind, dispatch through the bridge, worker reaching idle, ticket move, restart reconciliation and scoped cleanup. Its state cleanup deletes only rows for its own project key and panes, and it proves every other row is unchanged, as the Windows suite does. Python's `sqlite3` module does that work (`tests/acceptance_linux_state.py`, tested by `tests/acceptance_linux_state_test.py`). Review, reattach and the board UI are not driven. The consent dialog is not exercised; the smoke makes the call the extension makes after the user confirms.
8. **D8 Release: amd64.** `.github/workflows/release.yml` gains a `linux-amd64` matrix entry on `ubuntu-24.04` (`GOOS=linux`, `GOARCH=amd64`, `CGO_ENABLED=0`, `-trimpath`, the same `-ldflags`, no `.exe`, and a `--version` check). The publish job's `ASSETS` gains `herdr-omp-orchestrator-linux-amd64`, and `SHA256SUMS` covers both binaries. The tag check reads `linux/herdr-plugin.toml` from the tagged commit and requires the same version. The Windows job is unchanged, and arm64 is not built.
9. **D9 Documentation.** `README.md`, `AGENTS.md` and `docs/intent.md` say which platform each statement covers. `docs/herdr-facts.md` gains a dated Linux section for F1 to F8. Issue #83 records the out-of-scope workflows and criterion 1's wording: the Linux manifest meets the criterion in substance, and the root manifest does not declare Linux.

## Consequences

- A Windows user sees no new action, pane, id or binary name. The Windows manifest, installer, identity, worker and tests are unchanged.
- The root manifest does not declare Linux. Issue #83's criterion 1 is met by `linux/herdr-plugin.toml`, not by the root manifest. The issue records that wording change.
- Two manifests must stay in step. `TestLinuxManifestMirrorsTheWindowsManifest` fails when their ids, fields, actions or versions diverge, so a change to one must change the other.
- On a case-sensitive Linux directory, `Notes.md` and `notes.md` conflict. The refusal names both paths.
- A Linux claim whose owner cannot be checked is kept, not taken over, as on Windows. That fails closed, and the claim stays until its owner exits.
- macOS and other systems refuse claims and workers. arm64 is not built, tested or documented as supported.
- Setup reads its extension source through the `omp/` link in `build/linux-plugin`, so an edit to `omp/` reaches the next setup run without a rebuild.

## Residuals

- **Reboot identity collision (D2).** The identity is unique within one boot. After a reboot, a recorded PID and start time can match a new process. That reads as alive, so the claim is kept. This fails closed, and the claim can stay stuck until that process exits. Accepted.
- **Case-folded reservations (D4).** Folding case refuses distinct files on case-sensitive filesystems, such as `Notes.md` and `notes.md` in one directory. The refusal names both paths. Accepted as over-refusal.
- **arm64 is untested.** This ADR and the design cover x86_64 only. Herdr publishes an arm64 build; this repository does not build, test or support it.
- **The smoke path leaves `bind_intents` rows alone**, as the Windows helper does.
- **Rebuilding over a running executable.** On this host, `go build -o` replaced a running executable without error. The measurement used a running `sleep` copy, not the board process. So the install step's `text file busy` hint in `install.sh` is probably unreachable on Linux. It is kept because `tests/install_linux_tests.sh` covers the message.
- **Out of scope (issue #83).** Review, reattach and the board UI are not verified on Linux. They are recorded on the issue.
- **The release workflow has not run on GitHub.** The Linux release job is written, but no tag has run it.
- **An interrupted smoke run leaves its session behind.** `tests/acceptance_linux.sh` has no signal trap, so Ctrl+C skips its cleanup. A run whose step fails still reaches cleanup (L8). After an interrupt, remove the `orch-acc-linux-*` session and its run root by hand.

## Verification

Run on Linux x86_64 (WSL2, kernel 6.18) with Herdr 0.9.5, OMP 18.11.3, Go 1.27.1 and jq 1.8.1. The Go results come from commit `78f7a42`; no Go file changed after it.

The Windows test suites did not run on this machine: `go test` on Linux skips Windows-only files, and the PowerShell suites need Windows. Windows is checked here only by cross-compile (V2).

- **V1, Go suite.** `go test -count=1 -timeout 30m ./...` passes in every package. `tests/install_linux_tests.sh` passes 22 of 22 scenarios. `tests/acceptance_linux_state_test.py` passes its 3 tests.
- **V2, cross-compile.** `GOOS=windows` and `GOOS=darwin` `go vet ./...` pass. `GOOS=linux GOARCH=arm64 go build ./...` passes; arm64 is compile-only.
- **V3, release build.** Built with the release workflow's flags (`CGO_ENABLED=0`, `go build -trimpath -ldflags "-X main.version=0.1.0"`). The build reproduces, and the binary prints `herdr-omp-orchestrator 0.1.0`.
- **V4, install.** `./install.sh` exits 0. Herdr reports `omp: current`. The plugin is linked to `build/linux-plugin`, enabled, with no warnings and six actions.
- **V5, smoke.** `tests/acceptance_linux.sh` exits 0 with 33 PASS checks and one INFO. The INFO is the default session's focus projection, which was unchanged; the default server was running when the smoke started. The checks cover setup, bind, dispatch, a verified worker, a ticket move that starts its planner, restart reconciliation, the pinned model in every assistant turn, and scoped state cleanup that left every other row identical.

Found and fixed by these runs, before the passing results above:

- The build folder did not contain `omp/`, which setup reads for the extension source. Fixed by linking it (`79c7f9f`). The stub harness did not check the link, so it passed without it. It now does (`de0149b`).
- The first smoke run refused at `L0.plugin`. Its checks read plugin and log answers without their `result` envelope. Fixed (`5ed5703`).
- `TestStoreProvisioningSlotClaimIsIdempotentAndReleasable` failed in about 18% of runs (55 of 300 on this branch). It fails the same way at the base commit `f19289d`, so it predates this work. It asserted that slot tokens were fresh across tasks. It now checks only that a token is issued (`37a580c`). Per-task freshness stays covered by the renewal test in `gate_test.go`.
- `TestProcessIdentityDistinguishesLiveExitedAndReusedPIDs` needed the same exit-text fix as the store test, a type check on `*exec.ExitError`. The plan did not list it.
- Six mutants of the Linux code were each detected: worker stdio, zombie treated as alive, reused PID, a command name containing `)`, reservation folding, and manifest parity.

Git hooks in this clone are not executable, so `.githooks` did not run. The commit subjects follow Conventional Commits anyway.
