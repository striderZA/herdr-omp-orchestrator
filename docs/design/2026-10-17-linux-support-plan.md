# Linux Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Linux x86_64 user installs and runs the orchestrator with the same workflow as Windows (issue #83), proven by a recorded Linux `go test` run, install stub tests, and a Linux smoke path.

**Architecture:** One Go binary per platform. Linux differs in four places: process identity (`/proc` start ticks), the detached recovery worker (`Setsid`, `/dev/null` stdio), reserved-path case folding (every OS), and a second Linux-only manifest that `install.sh` links from `build/linux-plugin/`. The shell and Python scripts mirror the Windows install and acceptance scripts; nothing is generalized across them.

**Tech Stack:** Go 1.27.1 (`go.mod`), `CGO_ENABLED=0` for every binary build; POSIX `sh` and bash; `jq` 1.8.1; Python 3 with `sqlite3`; Herdr 0.9.5 (protocol 22); OMP 18.11.3.

**Spec:** `docs/design/2026-10-17-linux-support-design.md`. This plan implements its decisions D1 to D9 and its acceptance mapping. Where the two disagree, stop and ask.

**Location:** `docs/superpowers/` is git-ignored in this repository (`.gitignore`: `*superpowers*`), and the git hooks reject staged files there (AGENTS.md). The plan therefore lives beside the design in `docs/design/`, not in the skill's default path.

## Global Constraints

- Go 1.27.1 (`go.mod`). Every binary build uses `CGO_ENABLED=0`.
- Herdr protocol 22 on Linux (design F1). `min_herdr_version = "0.9.0"`, unchanged.
- Plugin id `user.omp-orchestrator`. Windows binary `herdr-omp-orchestrator.exe`; Linux binary `herdr-omp-orchestrator`.
- The root `herdr-plugin.toml` stays Windows-only and byte-for-byte unchanged. Only Task 4 adds a manifest.
- `install.sh` never unlinks a plugin and never writes Herdr's integration file (`herdr-omp-agent-state.ts`). It asks Herdr to install the integration, as `install.ps1` does.
- Reserved paths fold case on every OS. Identity comparisons (checkout paths, session references) stay platform-correct.
- Planner brief cap: 14,000 bytes on every platform.
- Linux support is x86_64 only. arm64 is compile-checked only. macOS is refused (`process_other.go`, `install.sh`).
- Windows behavior is unchanged. The only Windows-visible edits are test fixes that keep the Windows assertions.
- `omp/herdr-orchestrator.ts` is not changed.
- The smoke path never addresses Herdr's default session, never starts the default server, and never links or unlinks the plugin.
- Commit subjects are Conventional Commits (`.githooks`). Each task ends with one commit.

## Review Focus

1. **A reused PID, and a command name containing spaces or `)`.** Expected: a PID whose start time differs is not alive, and the start time is read after the last `)`. Pinned by `TestParseProcStatReadsStartTimeAfterTheCommandName` and `TestProcessIdentityDistinguishesLiveExitedAndReusedPIDs` (Task 1).
2. **An owner that exited but was never reaped (state `Z`).** Expected: `ProcessAlive` is false, so its claim can be recovered. Pinned by `TestProcessAliveIsFalseForAnUnreapedExitedProcess` (Task 1).
3. **A detached worker that writes to stderr after its foreground process exits.** Expected: the worker's later write completes. Pinned by `TestDetachedRecoveryWorkerSurvivesAStderrWriteAfterTheForegroundExits` (Task 3).
4. **A smoke cleanup that could delete another project's rows.** Expected: only this run's rows go, every other row is unchanged, and a foreign reference stops the delete. Pinned by `test_delete_scoped_removes_only_this_project_rows` and `test_delete_scoped_refuses_when_a_foreign_row_references_the_run` (Task 8).
5. **An install from a symlinked checkout path, or while another checkout is linked.** Expected: a symlink to the same folder is the same folder, and another folder's link is refused before any change. Pinned by install scenarios 3 and 5 (Task 7).

## File Structure

Created:
- `linux/herdr-plugin.toml`: Linux manifest (Task 4)
- `internal/state/process_linux.go`, `internal/state/process_linux_test.go`: Linux identity (Task 1)
- `cmd/herdr-omp-orchestrator/recovery_process_linux.go`, `cmd/herdr-omp-orchestrator/recovery_linux_test.go`: Linux worker (Task 3)
- `cmd/herdr-omp-orchestrator/manifest_test.go`: manifest reader and parity tests (Task 4)
- `install.sh`, `tests/install_linux_tests.sh` (Task 7)
- `tests/acceptance_linux.sh`, `tests/acceptance_linux_state.py`, `tests/acceptance_linux_state_test.py` (Task 8)
- `docs/adr/0008-linux-support.md` (Task 10)

Modified:
- `internal/state/process.go` (comment), `internal/state/process_other.go` (build tag and refusal), `internal/state/process_test.go` (one assertion): Tasks 1 and 5
- `internal/state/sqlite.go` (`pathsOverlap`, the `runtime` import, a comment) and `internal/coord/dispatch.go` (`normalizeReservedPath`, `isCaseInsensitivePath`): Task 2
- `internal/coord/dispatch_test.go`, `internal/coord/canonical_root_test.go`: Task 5
- `cmd/herdr-omp-orchestrator/recovery_process.go`, `recovery_process_windows.go`, `recovery_process_other.go`, `main_test.go`: Task 3
- `cmd/herdr-omp-orchestrator/reattach_test.go` (`readManifestSections` delegates to `readManifestDoc`): Task 4
- `internal/task/model_test.go`, `internal/setup/extension_test.go`, `internal/herdr/client_test.go`: Task 6
- `.gitignore`: Task 7
- `.github/workflows/release.yml`: Task 9
- `README.md`, `AGENTS.md`, `docs/intent.md`, `docs/herdr-facts.md`, `internal/coord/tickets.go` (comments): Task 10

## Task 0: Branch and design commit

**Files:** `docs/design/2026-10-17-linux-support-design.md`, `docs/design/2026-10-17-linux-support-plan.md`.

**Interfaces:** none. No code.

- [ ] **Step 1: Check the tree.** `git status --short` must list only the two design files, both untracked. Anything else: stop and ask.
- [ ] **Step 2: Enable the hooks if needed.** If `git config core.hooksPath` is empty, run `git config core.hooksPath .githooks` (AGENTS.md, Git Hooks).
- [ ] **Step 3: Create the branch.** `git switch -c linux-support`.
- [ ] **Step 4: Commit the design and plan.** `git add docs/design/2026-10-17-linux-support-design.md docs/design/2026-10-17-linux-support-plan.md && git commit -m "docs(design): add the Linux support design and plan"`. Expected: `git status` is clean.

## Task 1: Linux process identity

**Files:**
- Create: `internal/state/process_linux.go` (`//go:build linux`)
- Create: `internal/state/process_linux_test.go` (`//go:build linux`, `package state`, so it can reach `parseProcStat`)
- Modify: `internal/state/process_other.go` (build tag becomes `!windows && !linux`; both functions refuse)
- Modify: `internal/state/process.go:6-7` (comment on `ProcessIdentity`)

**Interfaces:**
- Consumes: `ProcessIdentity` and its `valid()` method in `process.go` (unchanged: `valid()` is `PID != 0 && StartTime > 0`).
- Produces (Linux): `CurrentProcessIdentity() (ProcessIdentity, error)`; `ProcessAlive(identity ProcessIdentity) (bool, error)`; unexported `parseProcStat(data string) (state byte, start int64, err error)`.
- Produces (other platforms): `errUnsupportedProcessIdentity`, returned by both functions.

- [ ] **Step 1: Write the failing tests** in `process_linux_test.go`.
  - Helper `procStatLine(comm string, state byte, start int64) string` (test-only): `4242 (<comm>) ` followed by fields 3 to 22, where field 3 is `state`, fields 4 to 21 are `0`, and field 22 is `start`.
  - `TestParseProcStatReadsStartTimeAfterTheCommandName`: for each comm in `sleep`, `my app`, `a)b`, `x) (y`, `parseProcStat(procStatLine(comm, 'S', 987654))` returns state `'S'` and start `987654`, with no error.
  - `TestParseProcStatRejectsLinesItCannotRead`: a line with no `)`; a line with fewer than 20 fields after the command name; a line whose field 22 is not a number. Each returns an error.
  - `TestProcessAliveIsFalseForAnUnreapedExitedProcess`: start `sh -c "exit 0"` and do not wait for it. Poll `/proc/<pid>/stat` until the state is `Z`, with a 5 s deadline. Read the child's start time from that line. Assert `ProcessAlive(identity)` returns `(false, nil)`. Then call `cmd.Wait()`.
  - Existing `TestProcessIdentityDistinguishesLiveExitedAndReusedPIDs` (`internal/state/process_test.go:47`) fails at line 61 today and must pass after this task.
- [ ] **Step 2: Run to see the failures.** `go test ./internal/state -run 'TestParseProcStat|TestProcessAliveIsFalseForAnUnreapedExitedProcess|TestProcessIdentityDistinguishes' -count=1`. Expected: build failure (`parseProcStat` undefined), and the reused-PID test fails at `process_test.go:61`.
- [ ] **Step 3: Implement `process_linux.go`.**
  - Stat path: `/proc/<pid>/stat`.
  - `parseProcStat`: take the text after the last `)`, split it with `strings.Fields`, and require at least 20 fields. The state is the first field's first byte. The start time is the 20th field (index 19, which is field 22 overall), parsed as int64.
  - `CurrentProcessIdentity`: read this process's own stat line (`os.Getpid()`), build `ProcessIdentity{PID, StartTime}`, and return an error when it is not `valid()`.
  - `ProcessAlive`: an identity that is not `valid()` is an error. A read that reports `fs.ErrNotExist` gives `(false, nil)`. Any other read or parse error gives `(false, err)`, so the claim is kept. State `Z` or `X`, or a start time that differs, gives `(false, nil)`. Otherwise `(true, nil)`.
- [ ] **Step 4: Replace the bodies of `process_other.go`.** Build tag `//go:build !windows && !linux`. Add `var errUnsupportedProcessIdentity = errors.New("process identity is not supported on this platform")`. Both functions return it. This replaces the current `StartTime: 1` placeholder and the `Signal(0)` check.
- [ ] **Step 5: Update the comment on `ProcessIdentity`** in `process.go`: `StartTime` is the creation time on Windows, and the start time in clock ticks since boot (field 22 of `/proc/<pid>/stat`) on Linux.
- [ ] **Step 6: Run to pass.** The Step 2 command reports PASS, including `TestProcessIdentityDistinguishes…`. Then `GOOS=darwin go vet ./internal/state` succeeds.
- [ ] **Step 7: Commit.** `feat(state): read Linux process start times from /proc`.

## Task 2: Reserved paths fold case on every OS

**Files:**
- Modify: `internal/coord/dispatch.go:380-403` (`normalizeReservedPath`: always lowercase) and `:466-470` (delete `isCaseInsensitivePath`)
- Modify: `internal/state/sqlite.go:15` (remove the `"runtime"` import; its only use is the GOOS check), `:1606-1609` (comment), `:1628-1643` (`pathsOverlap`: always lowercase)

**Interfaces:** Consumes and produces the existing `normalizeReservedPath(path string) (string, error)` and `pathsOverlap(a, b string) (string, bool)`. Signatures unchanged.

- [ ] **Step 1: Run the two tests that pin the rule.** `go test ./internal/state ./internal/coord -run 'TestStoreClaimTaskRespectsTheBudgetAndReservations|TestDispatchRefusesOverlappingReservedPaths' -count=1`. Expected: FAIL at `gate_test.go:210` and `dispatch_test.go:2112`.
- [ ] **Step 2: Implement.** In `normalizeReservedPath`, lowercase the cleaned path unconditionally, and delete `isCaseInsensitivePath` (after confirming with `xd://lsp` references that it has no other caller). In `pathsOverlap`, lowercase both paths unconditionally. Remove the `runtime` import. Reword the two comments to say the rule holds on every platform.
- [ ] **Step 3: Run to pass.** The Step 1 command reports PASS. Then `go test ./internal/state -count=1` (fast) and `go vet ./internal/state ./internal/coord`. The `coord` package runs fully in V1.
- [ ] **Step 4: Commit.** `fix(coord): fold reserved paths case on every platform`.

## Task 3: Detached recovery worker on Linux

**Files:**
- Create: `cmd/herdr-omp-orchestrator/recovery_process_linux.go` (`//go:build linux`)
- Create: `cmd/herdr-omp-orchestrator/recovery_linux_test.go` (`//go:build linux`, `package main`)
- Modify: `cmd/herdr-omp-orchestrator/recovery_process.go:149-151` (drop the `cmd.Stdout` and `cmd.Stderr` assignments; keep `cmd.Stdin = nil`; remove the `io` import only if nothing else in the file uses it)
- Modify: `cmd/herdr-omp-orchestrator/recovery_process_windows.go` (set `cmd.Stdout` and `cmd.Stderr` to `io.Discard` before setting `SysProcAttr`; add the `io` import)
- Modify: `cmd/herdr-omp-orchestrator/recovery_process_other.go` (build tag `!windows && !linux`; message `detached recovery workers are supported only on Windows and Linux`)
- Modify: `cmd/herdr-omp-orchestrator/main_test.go` (constants at lines 896-901, the late-write branch in `recoveryProcessWorkerMain`, and the skip reason at line 938)

**Interfaces:**
- Consumes, unchanged: `launchRecoveryChild(executable string, args, environ []string, markerPath, readyPath string) (func(bool) error, error)` (`recovery_process.go:145`), and `configureDetachedRecoveryChild(cmd *exec.Cmd) error`, one per platform (the Windows version is at `recovery_process_windows.go:11`).
- Produces: the Linux `configureDetachedRecoveryChild` sets `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}` and leaves `cmd.Stdout` and `cmd.Stderr` nil, so os/exec opens `/dev/null` for them.

- [ ] **Step 1: Write the failing test** `TestDetachedRecoveryWorkerSurvivesAStderrWriteAfterTheForegroundExits` in `recovery_linux_test.go`. Model it on `TestDetachedRecoveryChildSurvivesTheForegroundRoute` (`main_test.go:903-934`, Windows-only). The foreground environment adds `ORCH_RECOVERY_PROCESS_LATE=1` and `ORCH_RECOVERY_PROCESS_LATE_FILE=<dir>/worker.late`. Assert that the `recovered` file appears, and that the late file appears within 5 s after the foreground process exits.
- [ ] **Step 2: Run it.** `go test ./cmd/herdr-omp-orchestrator -run TestDetachedRecoveryWorkerSurvivesAStderrWriteAfterTheForegroundExits -count=1`. Expected: FAIL, because the worker is refused on Linux before it starts.
- [ ] **Step 3: Add the late branch to the worker child.** In `main_test.go`, add `recoveryProcessLateEnv = "ORCH_RECOVERY_PROCESS_LATE"` and `recoveryProcessLateFileEnv = "ORCH_RECOVERY_PROCESS_LATE_FILE"` to the constant block. In `recoveryProcessWorkerMain`, when the late variable is `1`: record `parent := os.Getppid()` at start; after writing the `recovered` file, poll every 10 ms (10 s bound) until `os.Getppid() != parent`, which means the foreground process has exited and the worker was reparented. Then write `late stderr write` to `os.Stderr`, and write the late file only when that write returns no error. Read `recoveryProcessParentMain` to confirm it passes `os.Environ()` to `launchRecoveryChild`, so the variables reach the worker.
- [ ] **Step 4: Implement the platform split.** Create `recovery_process_linux.go` as described in Interfaces. Remove the stdout and stderr assignments from `recovery_process.go`. In the Windows file, set both to `io.Discard` and keep the existing flags unchanged. Give the other-platform file its new build tag and message.
- [ ] **Step 5: Run to pass.** The Step 2 command reports PASS. Then `GOOS=windows go vet ./cmd/herdr-omp-orchestrator` and `GOOS=darwin go vet ./cmd/herdr-omp-orchestrator` succeed.
- [ ] **Step 6: Update the skip reason** of `TestDetachedRecoveryWorkerDoesNotPinTheCallersDirectory` (`main_test.go:936-939`) to: the working-directory pin matters only on Windows, where a process holds its directory open. Its Linux behavior is not tested, by design.
- [ ] **Step 7: Commit.** `feat(cmd): run the recovery worker detached on Linux`.

## Task 4: Linux manifest and parity tests

**Files:**
- Create: `linux/herdr-plugin.toml`
- Create: `cmd/herdr-omp-orchestrator/manifest_test.go` (`package main`)
- Modify: `cmd/herdr-omp-orchestrator/reattach_test.go:32-51` (`readManifestSections(t)` returns `readManifestDoc(t, "../../herdr-plugin.toml").sections`; the existing reader body moves into `readManifestDoc`)

**Interfaces:**
- Produces: `type manifestDoc struct { top map[string]string; sections []manifestSection }` and `func readManifestDoc(t *testing.T, path string) manifestDoc`. Values keep their raw text, quotes included. `top` holds the keys before the first `[[…]]` section. Keep an index into `sections`, not a pointer, because `append` can reallocate.

- [ ] **Step 1: Write the failing tests** in `manifest_test.go`.
  - `TestLinuxManifestMirrorsTheWindowsManifest`: the top-level `id`, `name`, `version` and `min_herdr_version` values are equal in both files. The root's `platforms` is `["windows"]` and Linux's is `["linux"]`. The root has exactly one `build` section and Linux has none. Apart from `build`, the two files have the same sections in the same order, with the same kinds and field keys, and equal values for every field except `command`. For each `command`, `strings.ReplaceAll(rootCommand, ".exe", "")` equals the Linux command.
  - `TestLinuxManifestCommandsNameTheLinuxBinary`: every Linux command starts with `"./herdr-omp-orchestrator"` and contains no `.exe`.
  - `TestRootManifestStaysWindowsOnly`: the root's `platforms` is `["windows"]`, and every root command names `./herdr-omp-orchestrator.exe`.
- [ ] **Step 2: Run to see the failures.** `go test ./cmd/herdr-omp-orchestrator -run 'TestLinuxManifest|TestRootManifest' -count=1`. Expected: build failure (`readManifestDoc` undefined) and the missing file.
- [ ] **Step 3: Implement `readManifestDoc`.** Use the same line rules as the existing reader (`reattach_test.go:32-51`): skip blank and `#` lines, `[[kind]]` opens a section, and `key = value` goes to `top` before the first section and to the current section after it.
- [ ] **Step 4: Write `linux/herdr-plugin.toml`.** It is the root manifest with `platforms = ["linux"]`, no `[[build]]` section, and every `./herdr-omp-orchestrator.exe` changed to `./herdr-omp-orchestrator`. Its header comment says it is the Linux manifest that `install.sh` copies into `build/linux-plugin/`.
- [ ] **Step 5: Run to pass.** The Step 2 command reports PASS. `go test ./cmd/herdr-omp-orchestrator -run TestManifestDeclares -count=1` and `go test ./internal/coord -run TestManifestDeclaresTheInstalledHookEvents -count=1` stay green.
- [ ] **Step 6: Commit.** `feat(manifest): add the Linux manifest and parity tests`.

## Task 5: Baseline fixes in state and coord tests

**Files:**
- Modify: `internal/state/process_test.go:183-188` (the `Kill` and `Wait` after the owner exits)
- Modify: `internal/coord/canonical_root_test.go:34-42` (the start-failure assertion)
- Modify: `internal/coord/dispatch_test.go`, `TestAgentNamesAreUniqueAndLegal` (near line 3050; add the `runtime` import if missing)

**Interfaces:** none (test-only).

- [ ] **Step 1: Run the three tests.** `go test ./internal/state ./internal/coord -run 'TestStoreClaimStepDoesNotEnterAResourceTaskOwnedByAnotherLiveProcessBetweenSteps|TestCanonicalRootGitStartFailureIsNotNotGitRepository|TestAgentNamesAreUniqueAndLegal' -count=1`. Expected: FAIL at `process_test.go:187`, `canonical_root_test.go:41` and `dispatch_test.go:3050`.
- [ ] **Step 2: Fix `process_test.go`.** After `cmd.Process.Kill()` succeeds, call `_ = cmd.Wait()`, with a comment: the owner was killed on purpose, and its exit status differs by platform (`exit status 1` on Windows, `signal: killed` on Unix), so only the wait matters. Keep the `Kill` error check.
- [ ] **Step 3: Fix `canonical_root_test.go`.** Replace the `*exec.Error` assertion (lines 39-42) with `errors.Is(err, fs.ErrNotExist)`, reporting the error on failure. The start failure of an absolute missing path is a `*fs.PathError` wrapping ENOENT on Linux and a not-found error on Windows, and both satisfy `errors.Is(err, fs.ErrNotExist)`. Add the `io/fs` import. `os/exec` stays, because the test uses it elsewhere.
- [ ] **Step 4: Fix `TestAgentNamesAreUniqueAndLegal`.** Use forward-slash spellings on every platform: `first := PrimaryAgentName("E:/work/repo-a")`, `again := PrimaryAgentName("e:/WORK/repo-a")`, `other := PrimaryAgentName("E:/work/repo-b")`. Keep the other assertions. Add a `runtime.GOOS == "windows"` block that asserts `PrimaryAgentName(`E:\work\repo-a`) == first`. Backslash is a separator only on Windows.
- [ ] **Step 5: Run to pass.** The Step 1 command reports PASS.
- [ ] **Step 6: Commit.** `test: make state and coord baseline checks platform-neutral`.

## Task 6: Baseline fixes in task, setup and herdr tests

**Files:**
- Modify: `internal/task/model_test.go:68-123` (the case list in `TestAnnouncementProvesTheRecordedSessionInEitherForm`; the case at lines 90-91 is the one that fails)
- Modify: `internal/setup/extension_test.go:869-879` (the profile table) and `:1036-1085` (`TestInstallEmbedsEscapedPathsThatDecodeBackToTheEnvironment`)
- Modify: `internal/herdr/client_test.go:511-545` (`TestDecodeWorktreeOpenResponse`) and `:671-712` (`TestWorktreeOpenAcceptsTheSpellingsHerdrReallyUses`)

**Interfaces:** none (test-only).

- [ ] **Step 1: Run the five tests.** `go test ./internal/task ./internal/setup ./internal/herdr -run 'TestAnnouncementProvesTheRecordedSessionInEitherForm|TestExtensionDirResolution|TestInstallEmbedsEscapedPathsThatDecodeBackToTheEnvironment|TestDecodeWorktreeOpenResponse|TestWorktreeOpenAcceptsTheSpellingsHerdrReallyUses' -count=1`. Expected: FAIL at `model_test.go:120`, `extension_test.go:901` and `:1082`, `client_test.go:516` and `:689`.
- [ ] **Step 2: `model_test.go`.** The case at lines 90-91 ("the recorded path spelled with other separators") stays on Windows only, where `\` and `/` are the same separator. On other platforms, add this case to the same list instead: an announcement `Announcement{File: native}` with `native := filepath.Join("/omp", "sessions", "-work-orch", "<id>.jsonl")`, recorded value `native`, reported as `SessionKindPath` with the separator after `/omp` doubled, expected `true`. `SameSessionReference` cleans both paths, so redundant separators name the same file. Leave the other cases unchanged.
- [ ] **Step 3: `extension_test.go` profile table.** Before the table, declare `customAgent := filepath.Join(t.TempDir(), "custom-agent")`. Change the case at lines 877-879 to `env: map[string]string{"PI_CODING_AGENT_DIR": customAgent}` and `want: filepath.Join(customAgent, "extensions")`. The subtest joins only relative `want` values with the home directory, so an absolute `want` is compared as it is. Keep the other cases unchanged.
- [ ] **Step 4: `extension_test.go` escaped paths.** At line 1042, on non-Windows only, add `"` and `\` to the odd directory name. Windows forbids both in file names, so its name stays as it is. Delete the source-text assertion at lines 1081-1083 (the `was embedded unescaped` check). The decode round trip at lines 1068-1080 is the behavior.
- [ ] **Step 5: `client_test.go` `TestDecodeWorktreeOpenResponse`.** The checkout argument (line 514) and the argv expectation (lines 540-541) use the backslash spelling. On Windows, keep it in both places. Elsewhere, use the spelling the response uses, `C:/work/herdr/worktrees/orch-probe-repo/probe-moved`, in both places. The root argument (line 513) already uses forward slashes.
- [ ] **Step 6: `client_test.go` `TestWorktreeOpenAcceptsTheSpellingsHerdrReallyUses`.** Move the case `backslashes and a trailing separator` (line 677) into the `runtime.GOOS == "windows"` block (lines 679-683). The same-spelling case (line 676) stays in the ungated list.
- [ ] **Step 7: Run to pass.** The Step 1 command reports PASS.
- [ ] **Step 8: Commit.** `test: make task, setup and herdr baseline checks platform-neutral`.

## Task 7: install.sh and its stub tests

**Files:**
- Create: `install.sh` (mode 0755, repository root)
- Create: `tests/install_linux_tests.sh` (mode 0755)
- Modify: `.gitignore` (add `/build/` and `/herdr-omp-orchestrator`)

**Interfaces:**
- `install.sh` takes no arguments. Its repository is the folder that holds it. It builds into `build/linux-plugin/`, copies `linux/herdr-plugin.toml` there, and links that folder.
- Its header has one line, `# tools: <command names>`, listing every external command it uses except `herdr`, `go` and `uname`. The harness links exactly those commands from the system into each scenario's tool folder, plus `jq`. `herdr`, `go` and `uname` come only from the scenario's bin folder.

- [ ] **Step 1: Write the harness first**, `tests/install_linux_tests.sh`.
  - Per scenario: a temp root with `fake-repo/` holding copies of `install.sh` and `linux/herdr-plugin.toml`; `bin/` holding stub `herdr`, `go` and `uname`; `tools/` holding symlinks to the system commands from the header line plus `jq`; `PATH=bin:tools`. Nothing from `/usr/bin` is reachable except through `tools/`, so a system `go` (this machine has `/usr/bin/go`) cannot leak into scenario 13.
  - Stub `herdr` (POSIX sh, state in `state.json`, reading it with `jq`): appends `herdr <args joined by single spaces>` to `calls.log` and `[a1][a2]…` to `argv.log` on every call. It answers the JSON shapes below. An unknown call writes `stub: unexpected herdr call: <args>` to stderr and exits 99.
  - Stub `go`: appends `go CGO_ENABLED=[<value>] <args>` to `calls.log`. If `GO_EXIT` is set and nonzero, it writes `GO_STDERR` to stderr and exits with `GO_EXIT`. Otherwise it writes `placeholder` to the path after `-o` (the stub must honor `-o`, because `install.sh` checks that the file exists), unless `GO_NO_OUTPUT=1`, and exits 0.
  - Stub `uname`: prints `Linux`, or `Darwin` when `UNAME_OS` says so.
  - Output: `ok   <name>` or `FAIL <name> :: <problems>` per scenario, then `install script check failures: N of M scenarios`. Exit 1 on any failure.
  - Stub JSON answers (one line each): `plugin list --json` with no plugin is `{"id":"cli:plugin","result":{"plugins":[]}}`. With the plugin it is `{"id":"cli:plugin","result":{"plugins":[{"plugin_id":"user.omp-orchestrator","plugin_root":"<root>","enabled":<bool>,"actions":[{"id":"setup"},{"id":"bind"},{"id":"board"}]}]}}`, plus `"warnings":[…]` after `actions` when set. `plugin link <path>` answers `{"id":"cli:plugin","result":{"linked":"<path>"}}`. `plugin action invoke setup --plugin user.omp-orchestrator` answers `{"id":"cli:plugin","result":{"log":{"log_id":"plugin-log-901"}}}`. `plugin log list --plugin user.omp-orchestrator --limit 100` answers `{"id":"cli:plugin","result":{"logs":[{"log_id":"plugin-log-901","status":"<running|succeeded>","exit_code":<n>,"stdout":"<answer>","stderr":""}]}}`, with `running` for the first `runningPolls` polls. `integration status` prints plain lines, `pi: not installed (<path>)` and `omp: <current (v10)|outdated (v9)|not installed> (<path>)`.
- [ ] **Step 2: The scenarios.** Rows 1 to 19 are the Windows scenarios of `tests/install_tests.ps1` with their Linux names and expectations (the `T:` line numbers are where they sit in that file). Rows 20 and 21 are new and Linux-only. Row 22 covers `install.ps1:218`, which no Windows scenario exercises.

| # | Scenario (Linux name) | Source | Exit | Output must match | Calls must match | Calls must not match |
|---|---|---|---|---|---|---|
| 1 | fresh install runs every step | T:213 | 0 | `Installed\.`, `extension: ` | `CGO_ENABLED=\[0\] build -o .*/build/linux-plugin/herdr-omp-orchestrator \./cmd/herdr-omp-orchestrator`; `^herdr integration install omp$`; `^herdr plugin link `; `^herdr plugin action invoke setup --plugin user\.omp-orchestrator$` | `plugin unlink`, `plugin enable` |
| 2 | already linked and installed: no reinstall, no unlink | T:219 | 0 | `Installed\.` | `^herdr plugin link `, `^herdr plugin action invoke setup` | `integration install`, `plugin unlink` |
| 3 | a symlink to the same folder is the same folder | T:225 | 0 | `Installed\.` | none | `plugin unlink` |
| 4 | an outdated integration counts as installed | T:230 | 0 | `Installed\.` | none | `integration install` |
| 5 | a link to another folder stops before any change | T:235 | 1 | `herdr plugin unlink user\.omp-orchestrator` | none | `^go `, `integration install`, `plugin link`, `action invoke`, `plugin enable` |
| 6 | a disabled plugin is enabled | T:240 | 0 | `Installed\.` | `^herdr plugin enable user\.omp-orchestrator$` | `plugin unlink` |
| 7 | manifest warnings stop before setup | T:246 | 1 | `manifest warnings` | none | `action invoke` |
| 8 | a setup refusal names its code | T:251 | 1 | `setup refused: integration_missing` | none | none |
| 9 | a setup action still running is awaited | T:255 | 0 | `Installed\.` | `^herdr plugin log list` at least 3 times | none |
| 10 | a build failure stops before Herdr is changed | T:260 | 1 | `go build failed`, `boom: cannot compile` | none | `plugin link`, `integration install`, `action invoke` |
| 11 | a build blocked by a running binary names the task board | T:266 | 1 | `close the task board pane` | none | none |
| 12 | herdr missing from PATH stops at once | T:271 | 1 | `herdr was not found on PATH` | none | `^herdr `, `^go ` |
| 13 | go missing from PATH stops at once | T:276 | 1 | `go was not found on PATH` | none | `^herdr `, `^go ` |
| 14 | a failing herdr plugin list stops before anything is built | T:281 | 1 | `herdr plugin list failed` | none | `^go `, `integration install`, `plugin link`, `action invoke` |
| 15 | a link failure stops before setup | T:286 | 1 | `herdr plugin link failed`, `requires a newer Herdr` | none | `action invoke` |
| 16 | an integration install failure stops before the link | T:291 | 1 | `herdr integration install omp failed` | none | `plugin link`, `action invoke` |
| 17 | an integration still missing after install stops | T:296 | 1 | `still does not report` | none | `plugin link`, `action invoke` |
| 18 | a folder without the manifest stops before Herdr is asked | T:301 | 1 | `linux/herdr-plugin\.toml is not in` | none | `^herdr `, `^go ` |
| 19 | a repository path with spaces reaches herdr as one argument | T:306 | 0 | `Installed\.` | argv line `[plugin][link][<path with spaces>/build/linux-plugin]` | none |
| 20 | jq missing from PATH stops at once | new | 1 | `jq was not found on PATH` | none | `^herdr `, `^go ` |
| 21 | a non-Linux host is refused | new | 1 | `install\.sh supports Linux only` | none | `^herdr `, `^go ` |
| 22 | a build that writes no binary is reported | I:218 | 1 | `go build succeeded but` | none | `plugin link` |

Scenario 3 creates a symlink to the fake repository and reports that path as `plugin_root`. Scenario 19 keeps the Windows folder name with spaces.

- [ ] **Step 3: Run to see the failures.** `bash tests/install_linux_tests.sh`. Expected: every scenario FAILs, because `install.sh` does not exist yet.
- [ ] **Step 4: Implement `install.sh`.**
  - Start with `set -euo pipefail`, the `# tools:` header line, and the repository folder from the script's own location (`cd "$(dirname "$0")"`).
  - Message strings follow `install.ps1` at the lines in the table, with the Linux names: `herdr-omp-orchestrator`, `linux/herdr-plugin.toml`, and `build/linux-plugin`. The Herdr and Go version text in the preflight messages is unchanged.
  - Preflight order: `uname -s` must be `Linux`; then `herdr`, `go` and `jq` on `PATH` (`command -v`), with a jq message that suggests `sudo apt install jq`; then `linux/herdr-plugin.toml` in the repository; then the registration from `herdr plugin list --json` (`jq -r` filter on `.result.plugins[] | select(.plugin_id == "user.omp-orchestrator")`). Compare `plugin_root` with the build folder by `realpath -m` on both, so a symlink to the same folder counts as the same folder.
  - Step `[1/4]`: copy `linux/herdr-plugin.toml` into `build/linux-plugin/`, then run `CGO_ENABLED=0 go build -o "$BUILD_DIR/herdr-omp-orchestrator" ./cmd/herdr-omp-orchestrator` from the repository root. A failure whose stderr matches `text file busy` adds the task-board hint.
  - Steps `[2/4]` to `[4/4]` as in `install.ps1`. `Wait-PluginLog` polls once a second for at most 120 seconds, as the PowerShell does. Use `jq -r` for every answer.
- [ ] **Step 5: Measure rebuilding over a running binary.** Copy `/usr/bin/sleep` to `$T/running`, start `"$T/running" 30 &`, then run `go build -o "$T/running"` on a one-line `package main` module. Record whether it succeeds or fails with `text file busy`, in the install test's header comment. The row 11 expectation holds either way, because the hint covers the `text file busy` case. The result goes into ADR 0008's Risks.
- [ ] **Step 6: Run to pass.** `bash tests/install_linux_tests.sh` reports `install script check failures: 0 of 22 scenarios` and exits 0. Then `bash -n install.sh tests/install_linux_tests.sh`.
- [ ] **Step 7: Commit.** `feat(install): add install.sh and its stub tests`.

## Task 8: Linux smoke path

**Files:**
- Create: `tests/acceptance_linux_state.py` (state helper)
- Create: `tests/acceptance_linux_state_test.py` (helper unit tests, `unittest`)
- Create: `tests/acceptance_linux.sh` (mode 0755)

**Interfaces:**
- `python3 tests/acceptance_linux_state.py <db> dump` prints `{"ok": true, "tables": {name: [row, …]}}`.
- `… <db> plan-scoped <project_key> <panes_csv>` prints `{"ok", "counts", "foreign"}` and writes nothing.
- `… <db> delete-scoped <project_key> <panes_csv>` prints `{"ok", "deleted", "others_identical", "ours_remaining"}`. When a row of another project references this run, it exits 3 with `{"ok": false, "error", "foreign", "counts"}` and writes nothing.
- `tests/acceptance_linux.sh [--keep-resources] [--skip-state-cleanup] [--model <provider/model[:thinking]>]`. The default model is `anthropic/claude-haiku-5-5:max` (design F13). `--model ''` keeps OMP's own default, as the Windows `-Model ''` does.

**Tables:** the helper scopes the five tables the Windows helper reads (`TABLES` at `tests/acceptance_windows.ps1:1124`): `projects`, `tasks`, `inbox`, `ready_sessions`, `tickets`. The sixth table, `bind_intents` (`internal/state/sqlite.go:144`), is not read by the Windows helper and is not read here. A run leaves it as the Windows run does, and the record names it as a residual.

- [ ] **Step 1: Write the helper tests** in `tests/acceptance_linux_state_test.py`. Build a temp database with the five tables, using the columns that `internal/state/sqlite.go:91-147` defines. The helper needs `projects(key, data)`, `tasks(id, project_key, launch_pane_id, public_pane_id, data)`, `inbox(id, project_key, task_id, data)`, `ready_sessions(launch_pane_id, omp_session_id, omp_session_file, omp_session_on_disk, verified, seen_at)` and `tickets(id, project_key, revision, data)`. Tests:
  - `test_delete_scoped_removes_only_this_project_rows`: two projects, delete one. Its rows are gone, the other project's rows are unchanged (`others_identical` is true), and `ours_remaining` is 0.
  - `test_delete_scoped_refuses_when_a_foreign_row_references_the_run`: a row of another project references this run's task or session id. Exit 3, and no row changes.
  - `test_plan_scoped_writes_nothing`: the database file's bytes are identical before and after.
- [ ] **Step 2: Run to see the failures.** `python3 tests/acceptance_linux_state_test.py -v`. Expected: FAIL (helper missing).
- [ ] **Step 3: Implement the helper** by porting `$script:StatePython` (`tests/acceptance_windows.ps1:1121-1268`): the same operations, JSON keys and exit codes. `dump` and `plan-scoped` open the database read-only (`file:<path>?mode=ro` URI).
- [ ] **Step 4: Run the helper tests to pass.** Three PASS.
- [ ] **Step 5: Write `tests/acceptance_linux.sh`.** Output one line per check: `PASS|FAIL|INFO <id> <what>`. Exit 0 when every check passes, 1 on any FAIL, and 2 when a precondition refuses the run before anything is created.

  - **L0 preflight, before anything is created.** `herdr`, `omp`, `git`, `jq`, `python3` and `setsid` are on `PATH`. `build/linux-plugin/herdr-omp-orchestrator` exists. `herdr plugin list --json` lists `user.omp-orchestrator` with `plugin_root` equal to `<repo>/build/linux-plugin` (compare with `realpath -m`), `enabled: true`, no `warnings`, and a `setup` action. No `orch-acc-linux-*` session is running (`herdr session list --json`). Otherwise refuse with exit 2. Never link or unlink.
  - **Run setup.** `RUN=$(mktemp -d)`. The session is `orch-acc-linux-<8 hex>`. Write `$RUN/herdr-config.toml` with a `[worktrees]` table whose `directory` is `$RUN/worktrees`. Write `$RUN/omp-model-overlay.yml` with two lines: `modelRoles:` and `  default: "<model>"` (as `Get-OmpModelOverlayText` does, `tests/acceptance_windows.ps1:2253-2260`). `PI_CONFIG_FILES` is the non-empty entries of the existing value without the overlay, then the overlay, joined with `:` (`Join-OmpConfigFiles`, `:2262-2277`).
  - **Start the isolated server.** `setsid env HERDR_CONFIG_PATH=… PI_CONFIG_FILES=… herdr --session <name> server >"$RUN/server.log" 2>&1 </dev/null &`. Record `$!`. Wait up to 60 s for `herdr session list --json` to list the session with `running: true`. Remove every key of `$script:HerdrCallerEnvKeys` (`tests/acceptance_windows.ps1:501-508`) from the environment of every Herdr and bridge call, and from the server's environment (as `New-RunServerStartInfo` does, `:3555-3557`). Isolated Herdr calls carry `--session <name>`.
  - **L1 setup.** `herdr --session <name> plugin action invoke setup --plugin user.omp-orchestrator`. Take `result.log.log_id` and poll `herdr --session <name> plugin log list --plugin user.omp-orchestrator --limit 100` once a second, at most 120 s, until that entry's `status` is not `running`. Expected: stdout contains `"ok":true`. The answer's `extension` field names the installed `herdr-orchestrator.ts`. Read `PLUGIN_STATE_DIR` from its `const PLUGIN_STATE_DIR = "…";` line (the canonical source declares it at `omp/herdr-orchestrator.ts:19`). The store is `<that dir>/orchestrator.sqlite` (`tests/acceptance_windows.ps1:3859`). Do not compute the state directory yourself.
  - **L2 bind.** `git init` a repository with one commit at `$RUN/repo`, using `git -c user.name=acceptance -c user.email=acceptance@example.invalid commit`. Run `herdr --session <name> workspace create --cwd "$RUN/repo" --no-focus --label accept-<run>`. The ids are `result.workspace.workspace_id`, `result.root_pane.pane_id` and `result.tab.tab_id`, and `result.root_pane.cwd` must be the repository (`tests/acceptance_windows.ps1:3685-3697`). Run `herdr --session <name> plugin action invoke bind --plugin user.omp-orchestrator` with `HERDR_ENV=1`, `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID` and `HERDR_TAB_ID` in its environment (`:3894-3900`). Then poll a bridge `resolve` as the primary for at most 180 s, until it answers `ok`.
  - **Bridge helper** `bridge_as <launch_pane> <method> <params-json>`. Read the `ready_sessions` row for `launch_pane_id`; no row is a local `no_handshake` answer. The body is `{"version":1,"method":…,"ompSessionID":omp_session_id,"ompSessionFile":omp_session_file,"ompSessionOnDisk":(omp_session_on_disk == 1),"params":…}`. Run `build/linux-plugin/herdr-omp-orchestrator bridge <method>` with the body on stdin, from the repository root, with `HERDR_ENV=1`, `HERDR_PANE_ID=<pane>`, `HERDR_SOCKET_PATH=<socket_path from session list>`, `HERDR_PLUGIN_STATE_DIR=<state dir>`, `HERDR_ORCH_HERDR_BIN_PATH=<herdr>`, and the caller keys removed. Timeout 180 s. Exit 0 with an answer on stdout (`ok` true or false); exit 2 is a bridge error. This mirrors `Invoke-BridgeAs` (`tests/acceptance_windows.ps1:3006-3095`).
  - **L3 dispatch.** `bridge_as <primary> dispatch` with `{"title":"ACCEPT LINUX: helper in a.txt","brief":"<one line>","checks":["git log -1 --format=%H"],"dependencies":[],"reservedPaths":["a.txt"],"baseRevision":"<HEAD, 40 hex>"}`. `baseRevision` is `git rev-parse HEAD` (`:3678-3680`). The field names are those of `DispatchParams` (`internal/coord/dispatch.go:20-26`). Expected: `ok` true, and the result's `worktreePath` lies under `$RUN/worktrees`.
  - **L4 worker.** Poll `bridge_as <primary> task_list` until the dispatched task has a `launchPaneId`. Then poll `herdr --session <name> agent get <that pane>` until `agent_status` is `idle` or `done` (`tests/acceptance_windows.ps1:3103` and `:3339-3341`), at most 300 s. The worker's `ready_sessions` row must have `verified = 1`.
  - **L5 ticket.** `bridge_as <primary> ticket_create {"title":"ACCEPT LINUX TICKET","body":"…"}` returns `ticketId`. `ticket_move_preview {"ticketId":…}` returns a TicketView with a `digest` (`internal/coord/tickets.go:94-101`). `ticket_move {"ticketId":…,"digest":…}` answers `ok` with no `plannerNotStarted`, and `ticket_list` shows the ticket with `column` `todo`. The move starts one planner task linked to the ticket, which must appear in `task_list` within 120 s. Its completion is not required.
  - **L6 restart.** Record the current time in milliseconds as `W`. Run `herdr --session <name> server stop` and poll `herdr session list --json` until the session's `running` is false (60 s). Start the server again with the L0 command, and poll until `running` is true (90 s). Then poll `herdr --session <name> plugin log list --plugin user.omp-orchestrator --limit 50` until an entry with `started_unix_ms` after `W` has a status other than `running`, and its stdout contains `"ok":true` (120 s; `tests/acceptance_windows.ps1:7314-7356`). Then `resolve` as the primary answers `ok`, and `task_list` still shows the L3 task with its worker's launch pane.
  - **L7 model.** With `PI_CONFIG_FILES` set, `omp config get modelRoles --json` returns `value.default` equal to the pin (`S1.2b`, `tests/acceptance_windows.ps1:3615-3618`). For each session file in `ready_sessions` (the primary and the worker), every assistant message's provider and model must match the pin, as the Windows transcript check does (`Get-OmpTranscriptModels`, `:2283-2317`; read its callers for the exact comparison before porting). A transcript that cannot be read is a FAIL while a model is pinned (AGENTS.md, pinned-model rule).
  - **L8 cleanup**, skipped with `--keep-resources` for the session and the run root. In this order: `server stop`, then wait until `running` is false (60 s). Unless `--skip-state-cleanup`, run `plan-scoped` and then `delete-scoped` on `<state dir>/orchestrator.sqlite` with the project key and this run's launch panes. `others_identical` must be true. Then `herdr session delete <name>` (the session must be stopped; `tests/acceptance_windows.ps1:7711-7726`), and verify the session is gone from `herdr session list --json`. If the server PID recorded from `$!` still exists and `/proc/<pid>/cmdline` contains `--session <name>`, send TERM to it; otherwise leave it alone. Remove `$RUN` (repository and worktrees) unless `--keep-resources`.
  - **INFO default session.** If the default session's server is running, take `herdr api snapshot` (no `--session`, read-only) before the run and after L8, and compare the focus projection (`tests/acceptance_windows.ps1:7734-7735`). Otherwise record INFO: not compared. The run never starts the default server.
- [ ] **Step 6: Verify the refusal path now.** Before `install.sh` has run, `bash tests/acceptance_linux.sh` must refuse at L0 with exit 2, create no isolated session, and leave no new run root under `${TMPDIR:-/tmp}`. Check both afterward. `herdr plugin list --json` works without a server (design F7), so the check needs no default server.
- [ ] **Step 7: Commit.** `feat(acceptance): add the Linux smoke path and its state helper`.

## Task 9: Release workflow for linux-amd64

**Files:** Modify `.github/workflows/release.yml`: the header comment (line 1), the tag check (`git show HEAD:herdr-plugin.toml`, line 67), the matrix (lines 37-43), the build output (line 87), the `--version` check (lines 89-95), the upload path (line 100), and `ASSETS` (lines 111-112).

**Interfaces:** none. Checks are the YAML parse and a local reproduction of the Linux build.

- [ ] **Step 1: Parse check before the edit.** `python3 -c "import yaml" 2>/dev/null && python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))"`. If PyYAML is missing, record that and rely on Step 4.
- [ ] **Step 2: Edit the workflow.**
  - Matrix: the Windows row gets `ext: .exe`. Add a row with `target: linux-amd64`, `goos: linux`, `goarch: amd64`, `os: ubuntu-24.04`, `runs: true` and `ext: ""`. Both rows use the same keys.
  - The build output, the `--version` check and the upload path use `dist/herdr-omp-orchestrator-${{ matrix.target }}${{ matrix.ext }}`.
  - `ASSETS` gains `herdr-omp-orchestrator-linux-amd64`. The existing `sha256sum $ASSETS > SHA256SUMS` (line 126) and the upload (line 133) cover every name in `ASSETS`, so `SHA256SUMS` covers both binaries without further change.
  - The tag check also reads `git show HEAD:linux/herdr-plugin.toml` and requires `version = "<version>"` there, with the same error style as the existing check.
  - The header comment (line 1) says the workflow attaches the Windows and Linux binaries and SHA256SUMS.
- [ ] **Step 3: Parse check again** (as Step 1). It must succeed.
- [ ] **Step 4: Reproduce the Linux build locally.** Use the same flags as the Windows build step (read it at the build line) with `GOOS=linux GOARCH=amd64` and `CGO_ENABLED=0`, then run `--version`. For example: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=0.1.0" -o /tmp/herdr-omp-orchestrator-linux-amd64 ./cmd/herdr-omp-orchestrator && /tmp/herdr-omp-orchestrator-linux-amd64 --version`. Expected output: `herdr-omp-orchestrator 0.1.0`, the line the workflow checks at line 95. Remove the file.
- [ ] **Step 5: Commit.** `ci(release): publish a Linux amd64 binary`.

## Task 10: Documentation, ADR and comments

**Files:** `README.md`, `AGENTS.md`, `docs/intent.md`, `docs/herdr-facts.md`, `docs/adr/0008-linux-support.md` (new), `internal/coord/tickets.go:133-134` and `:325-328` (comments only).

**Interfaces:** none. Documentation has no tests; the checks are Steps 7 and 8.

- [ ] **Step 1: `README.md`.**
  - Line 3: a Windows and Linux (x86_64) plugin.
  - Lines 16-20: the plugin runs on Windows and on Linux (x86_64). Add `jq` to the Linux requirements.
  - Add a Linux install section beside the Windows one: `./install.sh` from the repository root. The Herdr server must be running for the setup step.
  - Qualify lines 109-110 (`install.ps1` or `install.sh` again), line 121 (the executable name for each platform), line 123 (`%USERPROFILE%` is Windows-only), line 134, line 144 (add the Linux build command) and line 147 (`CGO_ENABLED=0` on Linux too).
  - State that arm64 is untested and macOS is unsupported.
- [ ] **Step 2: `AGENTS.md`.** Update each line that the docs inventory lists (5, 22, 34, 42, 45, 47, 49-51, 54, 56-57, 60-64, 67, 81, 87, 103, 113, 115, 120) so that it covers both platforms. Add `linux/herdr-plugin.toml`, the parity tests, `install.sh`, `tests/install_linux_tests.sh`, `tests/acceptance_linux.sh` and its helper tests, the Linux build command, and the Linux release asset. Line 81: the tag check reads both manifests.
- [ ] **Step 3: `docs/intent.md`.**
  - Line 9: a Windows and Linux (x86_64) plugin.
  - Lines 53 and 74: the planner brief bound is 14,000 bytes on every platform, and it is the Windows command-line bound, which every platform shares.
  - Line 86: add the Linux smoke path.
  - Lines 94-100: keep the existing rows, and add rows for the Linux install stubs (`tests/install_linux_tests.sh`), the Linux smoke path (`tests/acceptance_linux.sh`), and the Linux identity, worker and manifest tests.
  - Line 105: "**Linux (x86_64): smoke path and install stubs. Not verified: review, reattach and the board UI (out of scope for issue #83). macOS: refused.**"
- [ ] **Step 4: `docs/herdr-facts.md`.** Insert a dated section, `## Linux (issue #83, 2026-10-17)`, before `## Fixtures and their provenance` (line 32). List facts F1 to F8 from the design, one line each, with their evidence.
- [ ] **Step 5: `docs/adr/0008-linux-support.md`.** Follow the layout of an existing ADR, for example `docs/adr/0004-recover-abandoned-claims.md`. Record decisions D1 to D5, the consequences, and the residuals: reboot identity, case-folded reservations, arm64, `bind_intents` left by the smoke, the rebuild measurement from Task 7 Step 5, and the out-of-scope workflows. Add a verification record after V1 to V5.
- [ ] **Step 6: Comments.** `internal/coord/tickets.go:133-134` and `:325-328`: the cap applies on every platform, and it is the Windows command-line bound. No behavior change.
- [ ] **Step 7: Check.** `go vet ./...` is clean. Read the three documents once for any sentence that still says Windows-only without a platform qualifier.
- [ ] **Step 8: Commit.** `docs: record Linux support, install and acceptance`.

## Verification

- [ ] **V1 Linux test suite.** `go test -count=1 -timeout 30m ./...`: every package reports `ok`. Also run `bash tests/install_linux_tests.sh`, `python3 tests/acceptance_linux_state_test.py -v`, and `bash -n install.sh tests/install_linux_tests.sh tests/acceptance_linux.sh`. Record the results, and list every skipped test with its reason, in ADR 0008. Expected: zero failures.
- [ ] **V2 Cross-compile.** `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...`, `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go vet ./...`, and `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` (arm64 is compile-only). All succeed. The record says that the Windows tests did not run on this machine.
- [ ] **V3 Release build.** Repeat Task 9 Step 4.
- [ ] **V4 Install gate.** STOP and ask the user for approval first (design open item 1). The install writes Herdr's OMP integration, links the plugin in the global Herdr registry, and needs Herdr's default server running (the user starts it or approves the start). After approval, run `./install.sh`. Expected: `Installed.`. Then `herdr plugin list --json` shows the plugin linked to `build/linux-plugin`, enabled, with no warnings, and `herdr integration status` shows `omp: current`.
- [ ] **V5 Smoke run.** After V4, run `bash tests/acceptance_linux.sh`. Expected: every L check PASS, and `others_identical: true` in cleanup. Record the summary in ADR 0008.
- [ ] **V6 Issue record.** Comment on issue #83 with the out-of-scope workflows (review, reattach, board UI), the criterion 1 wording change (the Linux manifest), the arm64 and macOS decisions, the verification record, and the residuals. Post it only after V1 to V5 pass.
