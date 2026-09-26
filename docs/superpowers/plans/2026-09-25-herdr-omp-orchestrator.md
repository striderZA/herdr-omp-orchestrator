# Herdr-native OMP orchestrator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** From one primary Oh My Pi conversation, delegate up to three independent tasks into Herdr-managed worktrees, route blockers/results back through that primary, and inspect persistent task state on a terminal Kanban board.

**Architecture:** Herdr owns terminal/worktree/OMP lifecycle; a single Go executable implements plugin actions, hooks, bridge, transactional SQLite facts and a terminal board. A separate auto-discovered OMP TypeScript extension registers narrow role-specific tools and exchanges bounded JSON with the executable over stdin/stdout. No orchestrator daemon or second OMP lifecycle reporter.

**Tech Stack:** Go (Windows/amd64 first), pure-Go `modernc.org/sqlite`, Bubble Tea terminal UI, TypeScript executed by OMP's bundled runtime, Herdr plugin manifest/CLI (declared floor 0.9.0 - see compatibility gate 1), Git worktrees.

## Global Constraints

- Approved contract: `docs/superpowers/specs/2026-09-25-herdr-omp-orchestrator-design.md`; AO at `../agent-orchestrator` is read-only reference, not an implementation dependency.
- First release: Windows, one canonical Git repository per primary, default concurrency **three**; no multi-repo, other harnesses, PR/CI automation, desktop UI, auto-merge, force cleanup or daemon.
- Herdr is the sole terminal/worktree/agent lifecycle authority; install and verify `herdr integration install omp` separately, never overwrite its `herdr-omp-agent-state.ts`.
- Store mutable facts only in `HERDR_PLUGIN_STATE_DIR`; source/package and worktree checkouts remain free of plugin state/extension files. Never infer task completion from Herdr `idle`/`done`.
- The bridge keys a live process by its launch-time `HERDR_PANE_ID`; use a **separate current public pane ID** for Herdr CLI/focus. On move update only the public ID; on restart change a launch ID only after verifying OMP session + worktree provenance.
- Plugin actions/hooks/board use one Go executable; the OMP extension uses its host runtime only. `exec.CommandContext`/`spawn` argv arrays, bounded stdin/stdout/stderr, explicit timeouts; never interpolate prompts into shell commands.
- CLI timeouts after a submission are **ambiguous**. Inspect authoritative Herdr state and recorded step before retrying; never blindly resubmit prompts or force-delete dirty worktrees.
- Use `CGO_ENABLED=0` and pure-Go SQLite so the shipped Windows executable does not depend on a C toolchain. Pin module versions and commit `go.mod`/`go.sum`.
- Development prerequisites: Go is **not** currently on PATH and Herdr's OMP integration is **not** installed on this machine. A user-scoped Go 1.27.1 toolchain has been installed at `C:\Users\Jaco\AppData\Local\tools\go` (verified: `go version go1.27.1 windows/amd64`). The shell cannot resolve a bare `go`; every Go command in this plan runs as `MSYS_NO_PATHCONV=1 cmd.exe /c "set PATH=C:\Users\Jaco\AppData\Local\tools\go\bin;%PATH%&&go <args>"` — verified working for `go env`, `go test ./...` and `CGO_ENABLED=0` builds. Set `CGO_ENABLED=0` inside that command for builds. Do not install or alter user-wide Herdr/OMP integration without the user's explicit setup action. The release archive contains a prebuilt `.exe`; GitHub `herdr plugin install` from source requires Go because Herdr runs the manifest build command. A local/prebuilt release uses `herdr plugin link` (which skips builds).
- Run only newly created/modified tests at each task; use a disposable Git repository for Herdr integration and never the AO reference checkout.
- OMP session identity is a TYPED reference: Herdr reports `agent_session = {kind, value}` (observed `kind: "path"` carrying the session file). Compare by kind (`id` -> `getSessionId()`; `path` -> `getSessionFile()`), never a uuid parsed out of a filename. Path comparison is valid for identity only: OMP persists lazily, so `getSessionFile()` may return an allocated path that leads nowhere and `isSessionOnDisk()` is the only proof a session exists on disk - never advertise or build a `--resume` hint from an unmaterialized session. Herdr's integration also reports the reference asynchronously after OMP starts, so one immediate `agent get` can legitimately show none; poll with bounded backoff inside the readiness timeout. Stop at a failed gate, record exact output, and repair the contract rather than weakening the test.

## Compatibility gates (before implementing lifecycle code)

1. Read `herdr --version`, `omp --version`, `herdr api schema --json`, `herdr integration status`, and OMP's **effective profile** extension directory. Captured on this dev machine: `herdr 0.9.1-preview.2026-09-21-0ff0f27e2226`, `omp/18.3.1`, **default** OMP profile (no profile env vars). Herdr's OMP integration was **not installed** during Tasks 1-3, so setup correctly refused; the user has since consented to `herdr integration install omp`, which made `herdr integration status` report `omp: current (v10) (C:\Users\Jaco\.omp\agent\extensions\herdr-omp-agent-state.ts)`. **Status wording lesson:** an installed, up-to-date integration is reported as `current`, not `installed`; parse the `omp:` line by rejecting `not installed` rather than requiring a specific positive word, and treat any other wording (`current`, `installed`, a future `outdated`) as installed. Start with `min_herdr_version = "0.9.0"`. **Corrected by a real link attempt:** declaring `0.9.1` fails with `plugin_requires_newer_herdr`, because semver orders any `0.9.1-preview` *below* `0.9.1` and the installed `0.9.1-preview.2026-09-21` build reports `0.9.0` to the version gate. The floor must therefore be the last released version matching the required feature set, and that feature set was verified against the installed build's own `herdr api schema --json` (pane.move, worktree.create/remove/list, workspace.create, plugin.pane.open, events.subscribe/wait, session.snapshot, agent.start/prompt/get/read, pane.get, pane.report_agent/release_agent, plus the `pane.agent_status_changed` and `pane.moved` hook events). Verify `pane.agent_status_changed`/`pane.moved` **after linking the manifest in Task 6**: inspect `plugin.link`/`plugin.list` `warnings` (unknown hook names are warnings, not link errors) and observe actual events. See https://herdr.dev/docs/plugins/ and https://herdr.dev/docs/socket-api/.
2. OMP 18.3's extension API is **verified against the installed package** `@oh-my-pi/pi-coding-agent@18.3.1` (matches `omp/18.3.1`): `ExtensionAPI`, `defaultInactive`, `pi.getActiveTools()`/`setActiveTools()`, `pi.zod`/`typebox`/`arktype`, and `before_agent_start` returning `{ systemPrompt?: string[] }`. Use the specifier `@oh-my-pi/pi-coding-agent` (do **not** import `@earendil-works/*`, which is the separate Pi product); copy patterns from the installed package's own `examples/extensions/*.ts`. The remaining runtime check is that auto-discovered extension tools are present and active after Herdr's bare `omp --resume=<session>`; that test follows Task 4's bind/start. Sources: https://github.com/can1357/oh-my-pi/blob/v18.3.0/docs/extensions.md and https://github.com/can1357/oh-my-pi/blob/v18.3.0/docs/extension-loading.md. Do not accept a launch-only extension.
3. Capture real stdout/stderr (redacting secrets) for `workspace create`, `worktree create`, `pane get`, `agent start`, `agent prompt`, `agent get`, `worktree list`, `api snapshot` during Tasks 2–4, and both hook event payloads during Task 6. Herdr CLI wraps results under `.result`; plugin event env JSON shape is not promised to match socket subscription payload. Version differences become explicit fixture/adapter changes, not guessed field names.
4. If a gate cannot be exercised because Go, integration or OMP authentication is absent, keep it marked **unverified** and do not call the Windows end-to-end acceptance complete. Unit work can proceed without pretending this is runtime proof.

## File map and cross-task contracts

| Path | Responsibility / producer |
| --- | --- |
| `go.mod`, `go.sum`, `herdr-plugin.toml`, `.gitignore` | Pinned build, Windows manifest (minimal setup in Task 3, extended as features land), ignored generated exe/SQLite |
| `cmd/herdr-omp-orchestrator/main.go` | One binary, subcommands `setup`, `bind`, `startup`, `event`, `open-board`, `board`, `bridge`; cleanup is a confirmed primary bridge tool |
| `internal/task/model.go`, `model_test.go` | Durable Task/Project/Inbox types, stage projection, legal transitions |
| `internal/state/sqlite.go`, `sqlite_test.go` | Open(dir), SQL migration, CAS task updates, project/worker lookup, deduped inbox and notification claims |
| `internal/herdr/client.go`, `client_test.go`, `testdata/*.json` | `Client.Call(ctx,args...)`, bounded JSON/error decoding and exact-ID checks via `HERDR_BIN_PATH` |
| `internal/coord/bind.go`, `dispatch.go`, `dispatch_test.go` | Repo binding, concurrency/dependencies, persisted provisioning step machine |
| `internal/coord/report.go`, `report_test.go` | Worker reports/questions, primary review disposition and answer/dialog relay |
| `internal/coord/reconcile.go`, `reconcile_test.go` | Move/status hooks, startup snapshot, ambiguous-call reconciliation, safe inbox delivery |
| `internal/bridge/bridge.go`, `bridge_test.go` | Versioned stdin/stdout protocol and role/launch-pane authorization |
| `internal/setup/extension.go`, `extension_test.go`, `omp/herdr-orchestrator.ts` | Explicit global extension install/update and OMP tool/ready signal |
| `internal/board/view.go`, `view_test.go`, `board.go` | Read-only five-column projection and keyboard-driven terminal pane |
| `tests/acceptance_windows.ps1` | Disposable repository, real Herdr/OMP launch/move/block/report/reconnect acceptance |

**Shared types and stable names** (define in Task 1; consumers must use these exact names):

```go
package task

type Stage string
const (
    Queued Stage = "queued"
    Running Stage = "running"
    NeedsInput Stage = "needs_input"
    ReadyForReview Stage = "ready_for_review"
    Done Stage = "done"
)
type Phase string
const (
    Intent Phase = "intent"
    WorktreeCreated Phase = "worktree_created"
    AgentStarted Phase = "agent_started"
    PromptSubmitted Phase = "prompt_submitted"
)
type Report struct {
    Kind, Summary, Branch string
    Evidence, Checks []string
    Risk string
}
type Task struct {
    ID, ProjectKey, Title, Brief, BaseRevision, Branch string
    Checks, Dependencies, ReservedPaths []string
    WorktreePath, WorktreeWorkspaceID, CurrentWorkspaceID, TerminalID string
    LaunchPaneID, PublicPaneID, AgentName, OMPSessionID string
    Phase Phase
    Report *Report
    Blocker, Attention, Disposition string
    Activity string
    Revision, CreatedAt, UpdatedAt, ActivitySeenAt int64
}
type Project struct {
    Key, RepoWorkspaceID, CurrentWorkspaceID, TerminalID string
    LaunchPaneID, PublicPaneID, AgentName, OMPSessionID string
}
type Inbox struct { ID, ProjectKey, TaskID, Kind, Summary, Delivery string }
```

`internal/state.Open(dir string) (*Store,error)` owns the DB. `Store.CreateTask(ctx, task.Task) error`, `Store.Task(ctx,id) (task.Task,error)`, `Store.ByLaunchPane(ctx,id) (task.Task,error)`, `Store.ByOMPSession(ctx,sessionID) (Binding,error)` (Binding identifies exactly one primary project or worker task), `Store.UpdateTask(ctx,id,expectedRevision,func(*task.Task) error) (task.Task,error)`, `Store.MovePublicPane(ctx,launchID,oldPublicID,newPublicID string) error`, `Store.RecordReady(ctx,launchID,sessionID string,protocol int) error`, `Store.ConfirmReady(ctx,launchID,sessionID string) error`, `Store.Ready(ctx,launchID,sessionID string) (bool,error)`, `Store.AddInbox(ctx,task.Inbox) (bool,error)` and matching project/CAS methods are the only writers. `internal/herdr.Client.Call(ctx context.Context,args ...string) (json.RawMessage,error)` returns `.result` or typed `CLIError{Code,Message}`, never silently swallows a nonzero exit. `internal/bridge.Handle(ctx,env,request)` is the only extension entry. `internal/coord.Coordinator` receives `*state.Store` and `*herdr.Client`; none of its methods infer a role from tab labels. The bridge resolves project and task launch IDs separately and refuses an ambiguous match.

## Task 1: Persist task facts and legal lifecycle

**Files:** Create `go.mod`, `.gitignore`, `internal/task/model.go`, `internal/task/model_test.go`, `internal/state/sqlite.go`, `internal/state/sqlite_test.go`.

**Interfaces:** Produces the shared types and `Store` methods above. No Herdr dependency. Use `modernc.org/sqlite@v1.34.5`; the driver must work with `CGO_ENABLED=0` on Windows.

- [ ] **Step 1: Write failing domain and persistence tests.** Initialize `go.mod` with `go mod init herdr-omp-orchestrator` and pin SQLite with `go get modernc.org/sqlite@v1.34.5`. In `model_test.go`, assert the following concrete cases; in `sqlite_test.go`, use `t.TempDir()` and two independent `Open` connections to the same file:

```go
func TestStageSeparatesActivityFromCompletion(t *testing.T) {
    if StageOf(Task{Phase: Intent}) != Queued { t.Fatal("unprovisioned intent is queued, not running") }
    x := Task{Phase: PromptSubmitted, Activity: "done"}
    if StageOf(x) != Running { t.Fatalf("Herdr done is not task done: %s", StageOf(x)) }
    x.Blocker = "need an answer"
    if StageOf(x) != NeedsInput { t.Fatal("blocker must be visible") }
    x.Blocker = ""; x.Report = &Report{Kind: "partial", Summary: "more work remains"}
    if StageOf(x) != Running { t.Fatal("partial report is not final") }
    x.Report = &Report{Kind: "result", Summary: "finished"}
    if StageOf(x) != ReadyForReview { t.Fatal("report needs review") }
    x.Disposition = "user chose to keep branch"
    if StageOf(x) != Done { t.Fatal("recorded disposition closes task") }
}
```

Also assert a stale revision update fails without changing data, two attempts to add the same inbox ID yield `(true,nil)` then `(false,nil)`, and `MovePublicPane(ctx,"w1:p2","w1:p2","w2:p4")` leaves `LaunchPaneID == "w1:p2"` and `WorktreeWorkspaceID == "w1"` but changes `PublicPaneID == "w2:p4"` and `CurrentWorkspaceID == "w2"`. `RecordReady(ctx,"w1:p2","omp-session-1",1)` must remain unverified until `ConfirmReady` with that exact ID; a different session cannot pass `Ready`. A concurrent update from two Store connections must have one winner and one `ErrConflict`. This is the regression test for the review finding.

- [ ] **Step 2: Run failing tests.** `go test ./internal/task ./internal/state -run 'TestStage|TestStore|TestMove' -count=1`; expected failure: undefined packages/functions; if Go is absent, satisfy the prerequisite first.
- [ ] **Step 3: Implement minimal domain and store.** `StageOf` must project facts in this priority order:

```go
func StageOf(x Task) Stage {
    switch {
    case x.Disposition != "": return Done
    case x.Blocker != "" || x.Attention != "": return NeedsInput
    case x.Report != nil && x.Report.Kind == "result": return ReadyForReview
    case x.Phase == WorktreeCreated || x.Phase == AgentStarted || x.Phase == PromptSubmitted: return Running
    default: return Queued
    }
}
```

Open one `orchestrator.sqlite` under the provided state dir; create `projects(key PRIMARY KEY, data JSON)`, `tasks(id PRIMARY KEY, project_key, launch_pane_id NULL, public_pane_id, revision, data JSON)`, `inbox(id PRIMARY KEY, project_key, task_id, kind, summary, delivery)` and a `ready_sessions(launch_pane_id PRIMARY KEY, omp_session_id, protocol, verified, seen_at)` table. Use a partial unique index on non-NULL `launch_pane_id`, so several queued tasks without a pane can coexist; encode an unbound pane as SQL NULL, not an empty string. Set `PRAGMA journal_mode=WAL` and `busy_timeout=5000`. Store serialized facts as JSON and indexed IDs separately. `UpdateTask` uses a transaction and `UPDATE tasks SET data=?,revision=revision+1 WHERE id=? AND revision=?`, checks RowsAffected==1 and returns `ErrConflict` otherwise. Reject empty IDs/project keys, duplicate bound launch-pane IDs, and an update that changes task ID/project key. `RecordReady` writes an unverified OMP session ID; `ConfirmReady` marks it verified only after exact `agent get` session comparison. Store transition timestamps with each update. Commit creation of intent before any Herdr call.
- [ ] **Step 4: Run the new tests and commit.** `go test ./internal/task ./internal/state -run 'TestStage|TestStore|TestMove' -count=1` → PASS, including the concurrent-writer test. Do not require `go test -race` in a `CGO_ENABLED=0` Windows build: the race runtime needs a supported C toolchain. Commit these files as `feat: persist orchestrator task facts`.

## Task 2: Typed, bounded Herdr CLI and two pane identities

**Files:** Create `internal/herdr/client.go`, `client_test.go`, `internal/herdr/testdata/{worktree-create,pane-move,cli-error}.json` from observed sanitized responses. Tests may use a temporary fixture executable; no production mock Herdr client.

**Interfaces:** `NewClient(binary string) (*Client,error)`, `Call(ctx,args...) (json.RawMessage,error)`, `WorkspaceCreate`, `WorktreeCreate`, `PaneGet`, `AgentStart`, `AgentPrompt`, `AgentGet`, `AgentRead`, `AgentSendKeys`, `Snapshot`, `WorktreeList`, `WorktreeRemove`. IDs come from Herdr response fields, not labels. The client decodes a pane-move response into `previous_pane_id` and the new public pane ID; Task 6's coordinator validates the binding and invokes `Store.MovePublicPane`. The client never imports `internal/state`.

- [ ] **Step 1: Write failing CLI decoding tests.** Feed actual sanitized response JSON into the decoder; assert `.result.workspace.workspace_id`, `.result.root_pane.pane_id`, `.result.move_result.previous_pane_id` and `.result.move_result.pane.pane_id`. Test a nonzero exit with `{"error":{"code":"agent_blocked","message":"The agent requires an approval choice"}}` returns `CLIError.Code=="agent_blocked"`. Test >1 MiB output and unknown fields/absent required IDs fail closed. Add a table of Windows paths containing spaces and prompt quotes/newlines; verify argv stays discrete and no `cmd.exe` is called.
- [ ] **Step 2: Run failure.** `go test ./internal/herdr -run 'TestDecode|TestCLI|TestMove|TestArgv' -count=1` → FAIL because `Client`/decoder do not exist.
- [ ] **Step 3: Implement JSON envelope and argv execution.** Use `exec.CommandContext(ctx, binary, args...)`, never `sh -c`/`cmd /c`. Bound stdout/stderr each to 1 MiB with a writer that fails instead of silently truncating; use `json.Decoder` and reject missing/contradictory `result`/`error`. Classify `agent_blocked`, `agent_prompt_stalled`, timeouts and identity mismatches separately. For `worktree create --cwd ROOT --branch NAME --base SHA --no-focus`, require workspace/root-pane/worktree provenance in the response. `agent start NAME --kind omp --pane PUBLIC_ID` uses a legal name matching `[a-z][a-z0-9_-]{0,31}`. `agent prompt PUBLIC_ID BRIEF` is a single argv argument; a timeout after input is **ambiguous**, never proof the brief was not submitted. Test the exact syntax against https://herdr.dev/docs/cli-reference/.
- [ ] **Step 4: Run new tests and commit.** `go test ./internal/herdr -run 'TestDecode|TestCLI|TestMove|TestArgv' -count=1` → PASS. Commit as `feat: validate Herdr CLI responses and identities`.

## Task 3: Install and load a role-gated OMP extension

**Files:** Create `herdr-plugin.toml` (setup-only manifest), `cmd/herdr-omp-orchestrator/main.go` (setup/bridge entrypoints), `internal/setup/extension.go`, `internal/setup/extension_test.go`, `omp/herdr-orchestrator.ts`, `internal/bridge/bridge.go`, `internal/bridge/bridge_test.go`.

**Interfaces:** `setup.Install(env map[string]string, executable string) error` creates/updates **only** `herdr-orchestrator.ts` in OMP's effective-profile user extensions directory. Bridge protocol v1 request `{version:1,method:string,ompSessionID:string,params:object}`; response `{ok:true,result:...}` or `{ok:false,error:{code,message}}`. `bridge.Handle(ctx, env, input)` checks `HERDR_ENV=1`, inherited launch `HERDR_PANE_ID`, and OMP session identity. The read-only `resolve` method finds the bound project/task by launch ID or, after a restored process changes launch ID, by unique OMP session ID and verified Herdr terminal/checkout snapshot; it atomically rebinds only after proof. `ready` records BOTH announced identities unverified — the OMP session id (`ctx.sessionManager.getSessionId()`) and its session file (`ctx.sessionManager.getSessionFile()`) — for the coordinator to confirm against `agent get`; mutating methods require verified readiness and exact role/task, and confirmation compares **by the reference kind Herdr reports** (`id` -> the announced id; `path` -> the announced file, path-normalized). Real 0.9.0-preview evidence: `agent_session = {kind:"path", value:"...\sessions\..._<uuid>.jsonl"}`, so id-only matching silently records nothing. A supplied pane ID in params is never authority.

- [ ] **Step 1: Write failing setup/bridge tests.** Use temporary config/profile directories and a compiled test bridge: install leaves existing `herdr-omp-agent-state.ts` byte-identical, generates exact executable/state paths and protocol 1, repeats idempotently, rejects conflicting unmanaged extension and stale executable. With `OMP_PROFILE=qa`, assert setup uses `<OMP_ROOT>/profiles/qa/agent/extensions` and fails explicitly if Herdr integration is absent there; with default profile, assert `PI_CODING_AGENT_DIR` precedence. A worker bound to `w1:p2` cannot dispatch, primary can, mismatched OMP session ID fails, unbound OMP remains inactive. Simulate a verified restored session in a new launch pane: `resolve` rebinds while retaining the worktree ID; a different terminal/checkout or ambiguous session fails with attention.
- [ ] **Step 2: Run failure.** `go test ./internal/setup ./internal/bridge -run 'TestInstall|TestBridge' -count=1` → FAIL with missing implementations.
- [ ] **Step 3: Implement setup and bridge.** Determine selected profile from `OMP_PROFILE` if defined (including empty), otherwise `PI_PROFILE`; `default`/empty means default. A named profile uses `$HOME/$PI_CONFIG_DIR/profiles/<name>/agent` and ignores `PI_CODING_AGENT_DIR`. For default use `PI_CODING_AGENT_DIR` if set, then `$HOME/$PI_CONFIG_DIR/agent`, then `$HOME/.omp/agent` (https://github.com/can1357/oh-my-pi/blob/v18.3.0/docs/config-usage.md#profiles). Check `herdr integration status`, `herdr-omp-agent-state.ts` **in that same effective directory**, and an executable `HERDR_BIN_PATH`; fail before writing if absent. Atomically write generated extension in a same-directory temporary path; never edit OMP config or Herdr's extension. Embed `os.Executable()`, `HERDR_PLUGIN_STATE_DIR`, and the verified `HERDR_BIN_PATH` after JSON escaping; the generated extension passes the latter as `HERDR_ORCH_HERDR_BIN_PATH` to every bridge child because ordinary OMP pane processes are not guaranteed plugin-command environment. Create `herdr-plugin.toml` now with `id="user.omp-orchestrator"`, `name="OMP Orchestrator"`, `version="0.1.0"`, `min_herdr_version="0.9.0"`, `platforms=["windows"]`, `[[build]] command=["go","build","-o","herdr-omp-orchestrator.exe","./cmd/herdr-omp-orchestrator"]`, and `[[actions]] id="setup" command=["./herdr-omp-orchestrator.exe","setup"]`. Wire `setup` and `bridge` in `cmd/herdr-omp-orchestrator/main.go`; Task 4 adds bind, Task 6 hooks, Task 7 board. Link/invoke `user.omp-orchestrator.setup` only with user consent.

OMP extension registration follows OMP 18.3, not AO's Pi reviewer imports. **Schema-builder rule (learned from a real load failure):** never use a mutable literal default such as `.default([])` — OMP's omptype builder throws `ParseError: A mutable default value must be specified as a factory` and the WHOLE extension fails to load, so no tool registers and no handshake can ever arrive. Use `.default(() => [])` or `.optional()`, and cover it with a LOAD test that executes the factory and builds every tool schema (a bun `transpile` check does NOT catch this). Register typed role tools **inactive** at load, then resolve/rebind and activate only the verified role at session start and before each new user turn. The initial dispatch/report registrations are:

```ts
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
export default function (pi: ExtensionAPI) {
  if (process.env.HERDR_ENV !== "1" || !process.env.HERDR_PANE_ID) return;
  const z = pi.zod;
  const primaryNames = ["orch_dispatch"];
  const workerNames = ["orch_report"];
  const ownNames = [...primaryNames, ...workerNames];
  let role: "primary" | "worker" | null = null;
  async function activate(sessionID: string) {
    const binding = await callBridge("resolve", {}, sessionID);
    const nextRole = binding.bound ? binding.role as "primary" | "worker" : null;
    const target = nextRole === "primary" ? primaryNames : nextRole === "worker" ? workerNames : [];
    const current = pi.getActiveTools();
    const desired = [...new Set([...current.filter(name => !ownNames.includes(name)), ...target])];
    if (desired.join("\\0") !== current.join("\\0")) await pi.setActiveTools(desired);
    role = nextRole;
    if (role) await callBridge("ready", {}, sessionID);
    return role;
  }
  pi.on("session_start", async (_event, ctx) => {
    try { await activate(ctx.sessionManager.getSessionId()); }
    catch (error) { ctx.ui.notify(`Orchestrator binding unavailable: ${String(error)}`, "warning"); throw error; }
  });
  pi.on("before_agent_start", async (event, ctx) => {
    const currentRole = await activate(ctx.sessionManager.getSessionId());
    if (!currentRole) return;
    return { systemPrompt: [...event.systemPrompt, currentRole === "primary"
      ? "You are the sole user liaison. Delegate independent bounded work with orch_dispatch, avoid editing paths reserved for workers, relay their questions to the user, and request a decision before integrating reports."
      : "Work only in this assigned worktree. Use orch_question when blocked and orch_report with changed-file/commit evidence and checks when finished. Do not merge or remove the branch."] };
  });
  pi.registerTool({
    name: "orch_dispatch", label: "Delegate task", defaultInactive: true,
    description: "Start a bounded, isolated coding task.",
    parameters: z.object({
      title: z.string().min(1).max(160),
      brief: z.string().min(1).max(16000),
      checks: z.array(z.string().min(1).max(512)).min(1),
      dependencies: z.array(z.string()).default(() => []),
      reservedPaths: z.array(z.string()).default(() => []),
      baseRevision: z.string().min(7),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("dispatch", params, ctx.sessionManager.getSessionId(), signal);
      return { content: [{ type: "text", text: JSON.stringify(result) }], details: result };
    },
  });
  pi.registerTool({
    name: "orch_report", label: "Report result", defaultInactive: true,
    description: "Submit final evidence and checks for review.",
    parameters: z.object({
      kind: z.literal("result"),
      summary: z.string().min(1).max(4000),
      branch: z.string().min(1),
      evidence: z.array(z.string().min(1)).min(1),
      checks: z.array(z.string().min(1)),
      risk: z.string().max(4000),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("report", params, ctx.sessionManager.getSessionId(), signal);
      return { content: [{ type: "text", text: JSON.stringify(result) }], details: result };
    },
  });
}
```

Implement `callBridge` as `spawn(executable,["bridge",method],{windowsHide:true,env:{...process.env,HERDR_ORCH_HERDR_BIN_PATH:herdrBinary}})` with versioned stdin JSON (including caller OMP session ID), timeout/abort, ≤1 MiB stdout/stderr and structured errors; plugin executable, state dir and Herdr binary path are embedded by setup. The Go bridge uses that Herdr CLI binary instead of opening a Windows named pipe or assuming plugin-only env in the OMP pane. `resolve` may change a restored launch ID **only** after verifying the unique persisted OMP session, terminal and checkout against Herdr's authoritative snapshot; failures keep tools inactive, record attention and notify the primary, not a silent permanent unbind. Idempotent `activate` filters only this plugin's tool names, preserving other OMP tools; OMP's `before_agent_start` chain may retry when active tools change. Task 5 adds typed primary tools `orch_tasks`, `orch_inbox`, `orch_answer`, `orch_review`, `orch_approve_dialog`, `orch_cleanup` and worker tools `orch_progress`, `orch_question`, all initially inactive. Go bridge authorizes every **execution** anew. Coordinator compares `ready` with `agent get` and calls `ConfirmReady` before any brief. If resolution cannot prove a binding, never run a worker prompt without its tools.
- [ ] **Step 4: Run setup/bridge tests and link the minimal manifest, then commit.** `go test ./internal/setup ./internal/bridge -run 'TestInstall|TestBridge' -count=1` → PASS. `go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator` → exit 0; after the user's separate integration/setup approval, `herdr plugin link <absolute-plugin-path>` must report `user.omp-orchestrator` and `herdr plugin action list --plugin user.omp-orchestrator` must contain `setup`. A **real fresh/restore OMP smoke** follows Task 4's bind/start (do not claim it here). Commit as `feat: install role-gated OMP tools`.

## Task 4: Bind primary and dispatch isolated workers

**Files:** Modify `herdr-plugin.toml` (add bind action), `cmd/herdr-omp-orchestrator/main.go` (bind route); create `internal/coord/bind.go`, `internal/coord/dispatch.go`, `internal/coord/dispatch_test.go`.

**Interfaces:** `Coordinator.Bind(ctx,repo,workspaceID,rootPaneID string) (task.Project,error)`; `Dispatch(ctx,projectKey string, title,brief string, checks,dependencies,reservedPaths []string, baseRevision string) (task.Task,error)`; `ResumeDispatch(ctx,taskID string) (task.Task,error)`. The bridge calls these methods only after role authorization.

- [ ] **Step 1: Write failing dispatch tests.** Use a temporary real Git repository with two commits and a fixture executable for Herdr JSON. Assert a dirty primary checkout is not implicitly copied, base revision is a committed SHA, three independent requests start workers while a fourth remains Queued, dependent tasks remain queued until a primary records the prerequisite's review disposition (Done; a final worker report at ReadyForReview alone does not unlock them), two workers do not receive overlapping reserved-file ownership, and a retry at each persisted phase does not recreate an existing worktree/start a second OMP/send an identical brief. A failed create/start remains Needs input with recorded resources.
- [ ] **Step 2: Run failure.** `go test ./internal/coord -run 'TestBind|TestDispatch|TestConcurrency|TestRetry' -count=1` → FAIL before implementation.
- [ ] **Step 3: Implement the state machine.** Extend the existing manifest with `[[actions]] id="bind"` using `command=["./herdr-omp-orchestrator.exe","bind"]`; leave startup/status/move hooks for Task 6 and the board action/pane for Task 7. Explicit cleanup is a confirmed primary OMP tool (Task 5), not a context-free plugin action. The `main` binary's `bind` route requires injected `HERDR_PLUGIN_STATE_DIR` and `HERDR_PLUGIN_CONTEXT_JSON`; the bridge remains bounded. Plugin link skips build; build locally before re-linking the updated manifest.

Canonicalize root with `git rev-parse --show-toplevel`, `filepath.Abs`/`EvalSymlinks`, and Windows case-normalization; reject a different repository under the same primary binding. If no existing workspace represents the root, create one with `workspace create --cwd ROOT --no-focus`. Record its root pane as both launch/public ID **before** `agent start ... --kind omp --pane PUBLIC_ID`; require the extension `ready` signal matching `agent get` for that pane **by the reference kind Herdr reports** (`id` -> announced session id; `path` -> announced session file, path-normalized). Generate task ID/branch (e.g. `orch_<12-hex>` and `orch/<12-hex>`). Commit intent; call `worktree create --cwd ROOT --branch BRANCH --base SHA --no-focus`, persist exact returned workspace/worktree/root pane; bind launch/public pane; start named `omp` agent; **poll** `agent get` with bounded backoff inside the readiness timeout until it reports a session reference (Herdr's integration reports identity asynchronously after OMP starts, so a single immediate read can legitimately show none), then confirm the announced identity by kind; only then submit brief once using `agent prompt`. Persist phase after each acknowledged side effect. On ambiguous failure inspect `worktree list`, `agent get`/`read`, and readiness rather than repeating; unknown results remain actionable, never guessed away. The primary stays in its original checkout and focus never moves automatically.
- [ ] **Step 4: Run tests, then a real disposable fresh/restore smoke and commit.** `go test ./internal/coord -run 'TestBind|TestDispatch|TestConcurrency|TestRetry' -count=1` → PASS. After explicit Herdr/OMP integration consent and linking the updated manifest, bind a disposable repository and inspect active `orch_dispatch` on fresh OMP launch. Restore that exact OMP session through Herdr's `omp --resume=<session>` path with no `--extension` - gating the resume hint on the session being **materialized**, because OMP persists lazily (`getSessionFile()` can return an allocated path that leads nowhere until history holds an assistant message; `isSessionOnDisk()` is the only signal): inspect active tool names and effective `before_agent_start` role policy via a disposable test hook, then verify a simulated launch-ID change reactivates the right tools after `resolve` proves terminal/session/worktree identity. Fail this gate if tools/policy are absent, duplicated, or workers receive a brief before readiness; do not proceed to reporting/board tasks. Commit as `feat: bind primary and dispatch worktree agents`.

## Task 5: Worker reports, primary decisions, safe relay and cleanup

**Files:** Create `internal/coord/report.go`, `report_test.go`; extend `internal/bridge/bridge.go`, `bridge_test.go`, `omp/herdr-orchestrator.ts` with typed tool parameters.

**Interfaces:** `Progress(ctx,launchID,summary)`, `Question(ctx,launchID,text)`, `FinalReport(ctx,launchID,task.Report)`, `Answer(ctx,primaryLaunchID,taskID,text)`, `ApproveDialog(ctx,primaryLaunchID,taskID,observedDigest string,keys []string)`, `Review(ctx,primaryLaunchID,taskID,disposition string)`, `Cleanup(ctx,primaryLaunchID,taskID)`; methods recheck project/pane ownership before sending.

- [ ] **Step 1: Write failing behavior tests.** A worker question creates one durable inbox entry and sets Needs input, without switching focus. Final report must contain `Kind=="result"`, summary, branch, changed-file/commit evidence, checks/outcomes and risk; reject `Kind=="partial"` (progress uses `orch_progress` instead), missing evidence or checks. Herdr `done` alone cannot create a report. A final report moves to Ready for review; only a primary-recorded user disposition moves to Done. Answer relay uses the recorded **current public** worker ID and fails if the occupant/session changed. For a blocked full-screen worker, `agent read --source visible` returns the on-screen dialog; require direct UI confirmation for specific keys, re-read the same visible pane/dialog and compare its digest, then `agent send-keys`; changed/unreadable dialog refuses. Cleanup requires a direct UI confirmation naming task ID, branch and path; refusal/no UI performs no mutation. Cleanup refuses tasks that are not reviewed Done, still have a live worker agent, or have a dirty checkout; never calls `--force` or deletes the branch.
- [ ] **Step 2: Run failure.** `go test ./internal/coord ./internal/bridge -run 'TestReport|TestQuestion|TestAnswer|TestDialog|TestCleanup' -count=1` → FAIL.
- [ ] **Step 3: Implement exact authorization and review behavior.** Bridge dispatches each typed method according to persisted role and project; `FinalReport` rejects every kind except `"result"`. A final report is a transactional task update + deduplicated inbox insertion. For `orch_approve_dialog`, fetch a **passive** read-only preview with `agent read PUBLIC_ID --source visible` (not `recent-unwrapped --lines 80`, which can return `agent_not_idle` for blocked full-screen OMP) and digest the visible dialog; require `ctx.hasUI` and `await ctx.ui.confirm("Approve worker choice", \`Task ${taskID}: ${dialogText}\\nKeys: ${keys.join(" ")}\`)` in the primary OMP tool before a bridge call. The bridge re-reads that visible screen, verifies same session/pane/digest and sends only the approved keys via `agent send-keys`. `agent prompt` handles only ordinary questions when worker is not `blocked`. For `orch_cleanup`, use a read-only bridge preview naming task ID, branch, exact worktree path and clean/dirty state, require `ctx.hasUI` and `ctx.ui.confirm("Remove worker checkout", preview)` in the primary OMP tool, then revalidate reviewed Done disposition, no live worker, and exact **worktree checkout provenance** (`git -C WORKTREE status --porcelain` plus Herdr `worktree list` matching the recorded checkout path) in bridge before `worktree remove --workspace ID` **without** `--force`; preserve branch. Identify the worker's checkout by its recorded path/provenance, never by the original workspace id alone: a move into another workspace can close the source workspace (Herdr reports `closed_workspace_id`/`closed_tab_id` on the move result, which the client decodes), and that is expected state rather than a reason to refuse cleanup. No UI or rejected confirmation returns without mutation. The board has no mutation methods. Sources: https://herdr.dev/docs/agent-automation/#alternate-screen-history-reads and https://github.com/can1357/oh-my-pi/blob/v18.3.0/packages/coding-agent/src/extensibility/extensions/types.ts (`ExtensionUIContext.confirm`).
- [ ] **Step 4: Run tests and commit.** `go test ./internal/coord ./internal/bridge -run 'TestReport|TestQuestion|TestAnswer|TestDialog|TestCleanup' -count=1` → PASS. Commit as `feat: route worker reports and reviewed decisions`.

## Task 6: Hook delivery, public pane moves and restart recovery

**Files:** Create `internal/coord/reconcile.go`, `reconcile_test.go`; extend `internal/state/sqlite.go` inbox/claim methods, `cmd/herdr-omp-orchestrator/main.go` event/startup handling, `herdr-plugin.toml` hooks.

**Interfaces:** `HandleEvent(ctx,eventName string,payload []byte) error`; `Reconcile(ctx) error`; `FlushInbox(ctx,projectKey string) error`. Event adapter decodes **actual** `HERDR_PLUGIN_EVENT_JSON` captured and sanitized when Task 6's hooks fire. Startup reconciles from `herdr api snapshot` and `worktree list`, not non-replayed event history.

- [ ] **Step 1: Write failing hook/recovery tests.** Replay the same worker status event twice: one inbox ID. A primary's own status transition produces no worker notice, but a verified primary transition back to `idle` flushes a queued worker inbox entry once. A `pane.moved` from `w1:p2` to `w2:p4` updates only current public pane/workspace IDs while retaining launch ID, original worktree workspace and terminal ID; reporting from `HERDR_PANE_ID=w1:p2` still works. Repeat for a moved primary project binding. Miss the move event and recover from snapshot; reject a mismatched terminal/session or worktree checkout path and mark attention rather than re-key. Cover a move that closes the source workspace (the captured fixture reports `closed_workspace_id`): reconciliation must rebind from the move result and the verified snapshot, keep the task bound by worktree provenance, and not raise attention. When primary is working/missing/ambiguous, keep inbox pending; when idle, submit the task-ID summary once. On prompt timeout leave delivery `uncertain` and inspect `agent get/read` rather than resend. Restart never respawns an unknown occupant or discards unfinished workers/inbox.
- [ ] **Step 2: Run failure.** `go test ./internal/coord -run 'TestEvent|TestMove|TestReconcile|TestInbox|TestPrimaryLoss' -count=1` → FAIL.
- [ ] **Step 3: Implement event and startup pass.** Add `[[startup]] command=["./herdr-omp-orchestrator.exe","startup"]` and two `[[events]]` entries (`on="pane.agent_status_changed"` and `on="pane.moved"`) with `command=["./herdr-omp-orchestrator.exe","event"]` to the existing manifest. Validate event name and JSON size; for move use authoritative previous/current public IDs and the **same terminal ID and OMP session** for a CAS task or project public-ID update. Preserve original worktree workspace provenance; the pane may live in another workspace while its terminal foreground cwd still points to the worktree. When the move response reports `closed_workspace_id`/`closed_tab_id`, treat the source workspace as *closed as an expected consequence of the move*: replace the stale workspace binding from the move result plus the verified snapshot, keep identifying the task by worktree checkout provenance, and never mark the task Needs input merely because the original workspace id no longer exists. Status changes store activity separately from task stage; dedupe `(task, kind, payload digest, observed transition)` and never treat `done` as completion. Primary status changes are not worker notifications, but an idle transition can trigger `FlushInbox` for pending worker entries without self-echo. Reconcile snapshot, terminal/session, checkout cwd and Git worktree list; a **new** launch ID requires a verified restored OMP session and exact worktree/pane. Ambiguous probes become Needs input. Claim inbox delivery transactionally (`pending -> sending -> delivered`); timeout/unknown becomes `uncertain` until inspected, not delivered. Prompt only when primary is idle and verified. Startup is repeat-safe. Return nonzero on hook errors so Herdr logs them.
- [ ] **Step 4: Run tests, inspect real hook registration and commit.** `go test ./internal/coord -run 'TestEvent|TestMove|TestReconcile|TestInbox|TestPrimaryLoss' -count=1` → PASS. Re-link the manifest from Step 3 and inspect `herdr plugin list --plugin user.omp-orchestrator --json`: `warnings` must contain no unrecognized event. Trigger one **actual** status and move event in a disposable session; confirm hook logs and sanitized payload fixtures, then rerun tests against those fixtures. Unknown event names do not make link fail, so warnings are a hard gate. Commit as `feat: reconcile Herdr events and durable inbox`.

## Task 7: Read-only terminal Kanban board

**Files:** Create `internal/board/view.go`, `view_test.go`, `board.go`; wire `board` pane/action in `cmd/herdr-omp-orchestrator/main.go` and manifest.

**Interfaces:** `ProjectCards(ctx,store,projectKey) ([]Card,error)` derives Stage through `task.StageOf`, includes task ID/title/branch/activity/attention and current public pane ID. `board.Run(ctx,store,herdr,projectKey)` is a keyboard-driven Bubble Tea program; the only side-effecting key focuses an explicitly verified worker pane.

**Visual plan:** The single meaningful accent is **attention**, not a decorative header. Respect the terminal's default foreground/background; theme-aware fallback colors are ink `#D8DEE9`, muted `#77839A`, focus `#4EA8DE`, attention `#E6A23C`, success `#69B578`, failure `#D46A6A`. Use the user's terminal monospace; no font download. Five columns at wide widths; compact one-column stage sections when narrow. Plain sentence-case labels, task IDs/branches, no motion. Contrast and ASCII-only fallback on terminals without color/Unicode.

```text
Queued     Running       Needs input      Ready for review    Done
[Q7] refine API   [A2] tests    [B3] blocker     [C1] report        [D8] kept
                  [A4] parser
──────────────────────────────────────────────────────────────────────
Selected B3 · branch orch/b3 · blocked · worker w2:p4
Question: Which error contract should this use?   Enter details · f focus · q close
```

- [ ] **Step 1: Write failing projection/keyboard tests.** Feed Queued, Running/activity `done`, Needs input, Ready for review, Done facts; assert exactly five columns with no activity-driven stage changes. Select a card and assert brief, branch, report/evidence and last observation display. `f` focuses its verified current public pane; wrong/missing pane refuses. `enter`, `up/down`, `tab`, `q` navigate without mutating tasks. In an 80-column viewport, no line exceeds width and section labels remain readable.
- [ ] **Step 2: Run failure.** `go test ./internal/board -run 'TestProjection|TestBoard|TestFocus|TestNarrow' -count=1` → FAIL.
- [ ] **Step 3: Implement projection and UI.** Pin `github.com/charmbracelet/bubbletea@v1.3.4`; use `tea.WindowSizeMsg` for width, `tea.KeyMsg` for navigation and a selected task ID (not mutable label) for drill-down. On focus re-read the task, validate its current pane/worktree/session against Herdr, then `agent focus PUBLIC_ID`; never `pane send-keys` or write stage from board. Query SQLite on refresh/tick without mutating it; show empty-state hint (`Ask your primary to delegate a task`) and actionable errors. Extend the manifest with `[[panes]] id="board" placement="overlay" command=["./herdr-omp-orchestrator.exe","board"]` and `[[actions]] id="board" command=["./herdr-omp-orchestrator.exe","open-board"]`. The action's `open-board` route uses `HERDR_BIN_PATH` to call `herdr plugin pane open --plugin user.omp-orchestrator --entrypoint board --workspace <bound-workspace-id>`; the pane route maps its plugin context back to the bound project and runs Bubble Tea. Users may invoke the pane directly; an optional Herdr `plugin_action` keybinding is documented only on request.
- [ ] **Step 4: Run tests and commit.** `go test ./internal/board -run 'TestProjection|TestBoard|TestFocus|TestNarrow' -count=1` → PASS. Commit as `feat: show read-only terminal task board`.

## Task 8: Windows integration, installation and recovery acceptance

**Files:** Create `tests/acceptance_windows.ps1`; update `herdr-plugin.toml` packaging if the installed preview rejects the manifest; update the **source** files that fail a gate, not test expectations to fit accidental behavior.

**Interfaces:** Real installed Herdr preview (the `herdr` CLI on PATH is 0.9.1-preview, but the RUNNING server the plugin and bridge actually talk to is 0.9.0-preview) + OMP integration; repository under `$env:TEMP` initialized exclusively for the test; source directory linked with `herdr plugin link` after `go build`. No AO checkout or real user project changes.

- [ ] **Step 1: Write the acceptance script with explicit assertions.** Inside PowerShell set `$env:CGO_ENABLED = '0'`, then run `go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator`; create a temporary repo and two disjoint sample files, commit base, link plugin, invoke explicit `setup`, verify `herdr integration status` and that the installed extension lands in the **effective profile** directory, bind the primary, ask for two independent changes, observe two distinct worktree workspace IDs and OMP session IDs, move one worker pane to another workspace so the source workspace closes, and assert that bridge reporting still resolves **and** that worker identification and later cleanup resolve by worktree provenance rather than the closed workspace id, route one ordinary question and one **blocked full-screen** dialog through primary (visible-screen preview plus confirmed keys), reject a `partial` report without leaving Running, obtain two final structured reports, inspect board cards and preserved branches. Detach/reconnect or restart Herdr under supervision; assert launch/public IDs, inbox, worktrees and task stages survive, and that restored OMP still has its active role tools. Gate any resume-hint assertion on the session being MATERIALIZED (`isSessionOnDisk()`): OMP persists lazily, so a freshly bound session may legitimately have no on-disk file yet and a resume command built from it would fail. Refuse dirty worktree cleanup; exercise a timed-out `agent prompt` and inspect before retry. Every assertion should emit the task ID, command and redacted stdout/stderr on failure; finally remove **only** the temp repo/plugin test installation that the script itself created, and do not delete user branches/worktrees.
- [ ] **Step 2: Run the script on Windows after explicit installation consent.** `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1` → all assertions PASS, exit 0. Do not start Herdr as an untracked `bash` background process; during development use the agent's supervised process tool for long-running server/OMP sessions. If the installed preview differs from the docs, correct adapters and rerun this script.
- [ ] **Step 3: Run only the created tests, build and inspect release artifact.** `go test ./internal/task ./internal/state ./internal/herdr ./internal/setup ./internal/bridge ./internal/coord ./internal/board -count=1` → PASS. `go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator` → exit 0; `git diff --check` → no output. Confirm `/*.exe` and state DB are ignored. Package manifest + compiled exe for Windows release; from-source `herdr plugin install` continues to require Go. Commit as `test: prove Windows Herdr OMP workflow`.

## Self-review / acceptance coverage

- Primary conversation, auto-delegation, three-worker cap, dependencies, committed bases and non-stealing focus: Tasks 3–4.
- Distinct task stages/activity, transactional facts and inbox, report/evidence, explicit disposition and no auto merge: Tasks 1, 5–6.
- Worker questions, blocked permission dialogs (passive visible read plus confirmed keys), stale identity refusal and clean-only explicit cleanup: Tasks 5–6.
- Launch-time vs public pane identity, move event + snapshot recovery, restart-safe role activation and OMP resume: Tasks 1–4, 6, 8.
- Profile-aware extension install, Herdr manifest/ID contract, hook-warning gate, board navigation, no independent inbox/GUI: Tasks 3–4, 6–8.
- Disposable real Herdr/OMP reconnect, partial-report rejection, dirty cleanup refusal and ambiguous prompt: Task 8. Until that run succeeds, report integration as unverified rather than claiming completion.

**Sources:** https://herdr.dev/docs/plugins/ · https://herdr.dev/docs/cli-reference/ · https://herdr.dev/docs/socket-api/ · https://herdr.dev/docs/integrations/#omp · https://github.com/can1357/oh-my-pi/blob/v18.3.0/docs/extensions.md · https://github.com/can1357/oh-my-pi/blob/v18.3.0/docs/extension-loading.md
