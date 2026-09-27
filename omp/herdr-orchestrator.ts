// herdr-omp-orchestrator managed OMP extension (edit this source, not the installed copy).
//
// The plugin's explicit setup action substitutes the three @@…@@ placeholders
// below with this machine's absolute executable, plugin-state and Herdr-binary
// paths (JSON escaped) and installs the result as herdr-orchestrator.ts next to
// — never over — Herdr's own herdr-omp-agent-state.ts, in OMP's effective
// profile extensions directory. An installed copy is refreshed by editing
// omp/herdr-orchestrator.ts in the plugin and re-running the setup action; it
// is never edited in place.
//
// The extension never decides its own role at load time: it registers its tools
// inactive and asks the Go bridge to prove the binding. If the bridge cannot
// prove it, the tools stay inactive and the failure is recorded as attention —
// never a silent, permanent unbind.
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { spawn } from "node:child_process";

const PLUGIN_EXECUTABLE = "@@OMP_ORCH_EXECUTABLE@@";
const PLUGIN_STATE_DIR = "@@OMP_ORCH_STATE_DIR@@";
const HERDR_BINARY = "@@OMP_ORCH_HERDR_BINARY@@";
const BRIDGE_PROTOCOL = 1;

// Bounds mirror the Go bridge: one request and each response stream stay under
// 1 MiB, and a call that never answers is abandoned instead of hanging a turn.
const MAX_OUTPUT_BYTES = 1 << 20;
const BRIDGE_TIMEOUT_MS = 60_000;
const DETAIL_LIMIT = 200;

type Role = "primary" | "worker";

interface BridgeErrorBody {
  code: string;
  message: string;
}

interface ResolveBinding {
  bound: boolean;
  role: Role | null;
  reason: string;
  attention: string;
  taskId: string;
  projectKey: string;
}

class BridgeError extends Error {
  readonly code: string;

  constructor(body: BridgeErrorBody) {
    super(`${body.code}: ${body.message}`);
    this.name = "BridgeError";
    this.code = body.code;
  }
}

// readErrorBody narrows one untrusted bridge error body, so every consumer
// below works with a typed value instead of re-checking the shape.
function readErrorBody(value: unknown): BridgeErrorBody | null {
  if (typeof value !== "object" || value === null) return null;
  const code = "code" in value ? value.code : undefined;
  const message = "message" in value ? value.message : undefined;
  if (typeof code !== "string" || code.length === 0) return null;
  if (typeof message !== "string" || message.length === 0) return null;
  return { code, message };
}

// readResolveBinding narrows the resolve result the same way. A malformed body
// yields an unbound binding: the extension then leaves its tools inactive
// rather than guessing a role.
function readResolveBinding(value: unknown): ResolveBinding {
  const record: Record<string, unknown> =
    typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
  const role = record.role;
  const text = (key: string): string => (typeof record[key] === "string" ? String(record[key]) : "");
  return {
    bound: record.bound === true,
    role: role === "primary" || role === "worker" ? role : null,
    reason: text("reason"),
    attention: text("attention"),
    taskId: text("taskId"),
    projectKey: text("projectKey"),
  };
}

// SessionIdentity is what this process tells the bridge about its own session:
// both native identities OMP exposes, plus whether the session file has actually
// been written. The bridge records all three and matches Herdr's reported
// reference by kind, so a path-form reference and an id-form reference describe
// the same running process without either side parsing a file name.
interface SessionIdentity {
  id: string;
  file: string;
  onDisk: boolean;
}

// sessionIdentity reads the identity surface once per call. getSessionFile() may
// be undefined and isSessionOnDisk() may be false: OMP writes a session lazily,
// on its first assistant message, so an allocated path is not yet a resumable
// session.
function sessionIdentity(sessionManager: {
  getSessionId(): string;
  getSessionFile(): string | undefined;
  isSessionOnDisk(): boolean;
}): SessionIdentity {
  return {
    id: sessionManager.getSessionId(),
    file: sessionManager.getSessionFile() ?? "",
    onDisk: sessionManager.isSessionOnDisk(),
  };
}

// ---------------------------------------------------------------------------
// user-confirmed gates
// ---------------------------------------------------------------------------
//
// Recording a review, injecting input and removing a checkout all require the
// primary user's confirmation. confirmThen checks for an interactive UI first
// and returns before invoking the action when confirmation is unavailable or
// declined, so a refusal has no side effect to undo.

interface ConfirmRequest {
  title: string;
  message: string;
}

// UIGate is the part of OMP's extension context these gates need: whether an
// interactive UI exists at all, and the confirmation dialog itself.
interface UIGate {
  hasUI: boolean;
  confirm(title: string, message: string): Promise<boolean>;
}

// uiGate adapts the tool execution context to a UIGate. It is exported so the
// load check can exercise the gate with a stubbed context, and so the
// "a session with no UI can confirm nothing" rule is testable rather than
// assumed.
export function uiGate(ctx: {
  hasUI: boolean;
  ui: { confirm(title: string, message: string): Promise<boolean> };
}): UIGate {
  return {
    hasUI: ctx.hasUI === true,
    confirm: (title: string, message: string) => ctx.ui.confirm(title, message),
  };
}

// DialogPreview is what the bridge read off a worker's screen: the on-screen
// dialog and the digest that ties an approval to exactly that text.
interface DialogPreview {
  taskId: string;
  publicPaneId: string;
  agentName: string;
  agentStatus: string;
  blocked: boolean;
  dialog: string;
  digest: string;
}

// CleanupPreview is what the bridge found about one checkout: which task, which
// branch, which exact path, and whether removing it is allowed at all.
interface CleanupPreview {
  taskId: string;
  branch: string;
  path: string;
  workspaceId: string;
  reviewed: boolean;
  clean: boolean;
  liveWorker: boolean;
  removable: boolean;
  detail: string;
}

// Persisted report and revision returned by the primary-only report_preview
// bridge operation. No model-supplied report text is used for confirmation.
interface ReportPreview {
  taskId: string;
  revision: number;
  stage: "ready_for_review" | "done";
  summary: string;
  branch: string;
  evidence: string[];
  checks: string[];
  risk: string;
}

// BriefInspection is the evidence a primary shows before deciding an ambiguous
// brief submission: the task and its phase and blocker, the pane that was read, the
// revision the decision must be tied to, and the digest of the terminal text shown.
interface BriefInspection {
  taskId: string;
  title: string;
  phase: string;
  blocker: string;
  paneId: string;
  revision: number;
  digest: string;
  terminal: string;
}

// EVIDENCE_LIMIT bounds how much of a worker's terminal a confirmation shows: the
// digest covers the whole read, and the user is asked to check the pane itself.
const EVIDENCE_LIMIT = 1200;

type ConfirmPlan = { ok: true; request: ConfirmRequest } | { ok: false; reason: string };
type GateOutcome<T> = { ok: true; value: T } | { ok: false; reason: string };

function previewText(value: unknown, key: string): string {
  if (typeof value !== "object" || value === null || !(key in value)) return "";
  const field = value[key];
  return typeof field === "string" ? field : "";
}

function previewNumber(value: unknown, key: string): number {
  if (typeof value !== "object" || value === null || !(key in value)) return 0;
  const field = value[key];
  return typeof field === "number" && Number.isFinite(field) ? field : 0;
}

function previewFlag(value: unknown, key: string): boolean {
  if (typeof value !== "object" || value === null || !(key in value)) return false;
  return value[key] === true;
}

function readDialogPreview(value: unknown): DialogPreview {
  return {
    taskId: previewText(value, "taskId"),
    publicPaneId: previewText(value, "publicPaneId"),
    agentName: previewText(value, "agentName"),
    agentStatus: previewText(value, "agentStatus"),
    blocked: previewFlag(value, "blocked"),
    dialog: previewText(value, "dialog"),
    digest: previewText(value, "digest"),
  };
}

function readCleanupPreview(value: unknown): CleanupPreview {
  return {
    taskId: previewText(value, "taskId"),
    branch: previewText(value, "branch"),
    path: previewText(value, "path"),
    workspaceId: previewText(value, "workspaceId"),
    reviewed: previewFlag(value, "reviewed"),
    clean: previewFlag(value, "clean"),
    liveWorker: previewFlag(value, "liveWorker"),
    removable: previewFlag(value, "removable"),
    detail: previewText(value, "detail"),
  };
}

export function readReportPreview(value: unknown): ReportPreview | null {
  if (typeof value !== "object" || value === null) return null;
  const taskId = previewText(value, "taskId");
  const summary = previewText(value, "summary");
  const branch = previewText(value, "branch");
  const risk = previewText(value, "risk");
  const revision = previewNumber(value, "revision");
  const stage = previewText(value, "stage");
  const evidence = "evidence" in value ? value.evidence : null;
  const checks = "checks" in value ? value.checks : null;
  if (!taskId || !summary || !branch || !risk || !Number.isSafeInteger(revision) || revision <= 0 ||
    (stage !== "ready_for_review" && stage !== "done") ||
    !Array.isArray(evidence) || evidence.length === 0 || !evidence.every((item) => typeof item === "string") ||
    !Array.isArray(checks) || checks.length === 0 || !checks.every((item) => typeof item === "string")) {
    return null;
  }
  return { taskId, revision, stage, summary, branch, evidence, checks, risk };
}

// dialogConfirmRequest is the confirmation one blocked worker's dialog needs: the
// exact on-screen text and the exact keys, so the user approves a specific choice
// for a specific dialog rather than a blanket yes.
export function dialogConfirmRequest(preview: DialogPreview, keys: string[]): ConfirmPlan {
  if (keys.length === 0) {
    return { ok: false, reason: "no keys were requested; an approval must name the exact keys" };
  }
  if (preview.digest.length === 0) {
    return { ok: false, reason: "the dialog has no digest, so an approval cannot be tied to it" };
  }
  if (!preview.blocked) {
    return {
      ok: false,
      reason: `task ${preview.taskId} is not blocked on a dialog (agent status ${
        preview.agentStatus.length > 0 ? preview.agentStatus : "unknown"
      }), so there is nothing to approve`,
    };
  }
  return {
    ok: true,
    request: {
      title: "Approve worker choice",
      message: `Task ${preview.taskId}: ${preview.dialog}\nKeys: ${keys.join(" ")}`,
    },
  };
}

// cleanupConfirmRequest is the confirmation one checkout removal needs: the task,
// the branch and the exact path, with the state the user is deciding on. It
// refuses before the user is asked when the removal could not be honored anyway.
export function cleanupConfirmRequest(preview: CleanupPreview): ConfirmPlan {
  if (!preview.reviewed) {
    return {
      ok: false,
      reason: `task ${preview.taskId} is not a reviewed done task, so its checkout may not be removed`,
    };
  }
  if (preview.liveWorker) {
    return { ok: false, reason: `the worker agent of task ${preview.taskId} is still running` };
  }
  if (!preview.clean) {
    return { ok: false, reason: `the checkout ${preview.path} is not clean: ${preview.detail}` };
  }
  if (preview.path.length === 0) {
    return { ok: false, reason: `task ${preview.taskId} records no checkout that can be removed` };
  }
  // A checkout Herdr no longer holds in a workspace is still removable: the bridge
  // opens it by its recorded path first, which it does for the moved state a pane
  // move leaves behind. The user is told which of the two is about to happen.
  const where =
    preview.workspaceId.length > 0
      ? `workspace ${preview.workspaceId}`
      : "no workspace currently holds it, so it is opened by its recorded path first";
  return {
    ok: true,
    request: {
      title: "Remove worker checkout",
      message:
        `Task ${preview.taskId}: remove ${preview.path} on branch ${preview.branch} (${where}). ` +
        `The branch is kept and the removal is never forced.`,
    },
  };
}

// settleConfirmRequest is the confirmation one settlement needs: the model proposes
// what the user should have seen in the worker's pane, and only the user's own
// confirmation turns that into a settlement. Until it is confirmed, nothing else can be
// sent to that worker, so the user is told what this decision allows or prevents.
export function settleConfirmRequest(taskId: string, decision: string): ConfirmPlan {
  if (taskId.length === 0) {
    return { ok: false, reason: "no task was named for the settlement" };
  }
  if (decision !== "delivered" && decision !== "not_delivered") {
    return {
      ok: false,
      reason: `"${decision}" is not a settlement; the user reports that the input either "delivered" or "not_delivered"`,
    };
  }
  const observed = decision === "delivered" ? "DID arrive" : "did NOT arrive";
  return {
    ok: true,
    request: {
      title: "Settle worker input",
      message:
        `Task ${taskId}: confirm that the input sent to this worker earlier ${observed}. ` +
        "Inspect the worker's pane first: this decides whether anything else may be sent to it.",
    },
  };
}

// adjudicateConfirmRequest is the confirmation an ambiguous brief submission needs:
// the model proposes one of the two outcomes, and the user sees the exact evidence
// the inspection read — the task, its pane, the revision and the terminal text —
// before anything is recorded. The text is what the digest was taken over, so the
// coordinator can refuse a decision made on a screen that has since changed.
export function adjudicateConfirmRequest(inspection: BriefInspection, decision: string): ConfirmPlan {
  if (inspection.taskId.length === 0) {
    return { ok: false, reason: "the inspection named no task" };
  }
  if (decision !== "delivered" && decision !== "not_delivered") {
    return {
      ok: false,
      reason: `"${decision}" is not a decision; the user reports that the brief either "delivered" or "not_delivered"`,
    };
  }
  if (inspection.paneId.length === 0 || inspection.digest.length === 0) {
    return { ok: false, reason: "the inspection carries no pane and digest, so nothing can be decided" };
  }
  const outcome =
    decision === "delivered"
      ? "the brief DID reach the worker (it will be recorded as submitted and never sent again)"
      : "the brief did NOT reach the worker (only this blocker is cleared and the brief is sent once)";
  const shown = inspection.terminal.length > EVIDENCE_LIMIT
    ? `${inspection.terminal.slice(0, EVIDENCE_LIMIT)}\n… (truncated)`
    : inspection.terminal;
  return {
    ok: true,
    request: {
      title: "Decide an uncertain dispatch",
      message:
        `Task ${inspection.taskId} (phase ${inspection.phase}) on pane ${inspection.paneId}, revision ` +
        `${String(inspection.revision)}, digest ${inspection.digest}: confirm that ${outcome}.\n\n` +
        `What the worker's pane shows now:\n${shown}\n\n` +
        "Read the pane yourself before answering: an answer of \"delivered\" records a submission you did not see.",
    },
  };
}

// readBriefInspection shapes the whole brief_inspection answer, and reports whether
// it is usable at all: a decision is never taken on a partial reading.
function readBriefInspection(value: unknown): BriefInspection {
  return {
    taskId: previewText(value, "taskId"),
    title: previewText(value, "title"),
    phase: previewText(value, "phase"),
    blocker: previewText(value, "blocker"),
    paneId: previewText(value, "paneId"),
    revision: previewNumber(value, "revision"),
    digest: previewText(value, "digest"),
    terminal: previewText(value, "terminal"),
  };
}

// confirmThen is the gate itself: without an interactive UI nothing happens, and
// a failed or declined confirmation returns before act() is ever called.
export async function confirmThen<T>(
  gate: UIGate,
  request: ConfirmRequest,
  act: () => Promise<T>,
): Promise<GateOutcome<T>> {
  if (!gate.hasUI) {
    return {
      ok: false,
      reason: "this session has no interactive UI, so the user could not confirm and nothing was done",
    };
  }
  let approved: unknown;
  try {
    approved = await gate.confirm(request.title, request.message);
  } catch (error) {
    return { ok: false, reason: `the confirmation failed (${String(error)}), so nothing was done` };
  }
  if (approved !== true) {
    return { ok: false, reason: "the user did not confirm, so nothing was done" };
  }
  return { ok: true, value: await act() };
}
// The review sends exactly the persisted report revision the user approved,
// along with their disposition. A later report/task edit invalidates this token.
export function recordReviewWithConsent<T>(
  gate: UIGate,
  preview: ReportPreview,
  disposition: string,
  send: (params: { taskId: string; disposition: string; expectedRevision: number }) => Promise<T>,
): Promise<GateOutcome<T>> {
  const request = {
    title: "Record review disposition",
    message:
      `Confirm that you reviewed the worker's final report for task ${preview.taskId} and chose this disposition:\n` +
      `${disposition}\nStored report (revision ${preview.revision}):\n` +
      `Summary: ${JSON.stringify(preview.summary)}\nBranch: ${JSON.stringify(preview.branch)}\n` +
      `Evidence: ${JSON.stringify(preview.evidence)}\nChecks: ${JSON.stringify(preview.checks)}\n` +
      `Risk: ${JSON.stringify(preview.risk)}\n` +
      "This closes the task and may start queued dependent work. Decline if you have not reviewed the report.",
  };
  return confirmThen(gate, request, () => send({
    taskId: preview.taskId, disposition, expectedRevision: preview.revision,
  }));
}


// ---------------------------------------------------------------------------
// the post-turn session refresh
// ---------------------------------------------------------------------------
//
// OMP materializes a session lazily: the JSONL appears with the first assistant
// message, so the identity a freshly started process announces legitimately
// reports onDisk false — and a resume hint built from an unmaterialized session
// can never work. `agent_end` is the first point at which the runtime has
// persisted the turn, so the extension re-announces there.

// REFRESH_TIMEOUT_MS bounds one refresh. The turn is already complete when it
// runs, and OMP awaits a turn's post-prompt handlers, so a wedged bridge must not
// hold the runtime for the full bridge timeout.
const REFRESH_TIMEOUT_MS = 5_000;

// MATERIALIZE_TRIES and MATERIALIZE_WAIT_MS bound the wait for a session the
// runtime says is not on disk yet. Persistence is queued in emission order, so the
// write for this very turn may still be in flight when the hook runs: waiting a
// few milliseconds once is what makes the refresh see the materialized session,
// and it costs nothing on every later turn.
const MATERIALIZE_TRIES = 5;
const MATERIALIZE_WAIT_MS = 40;

// refreshIdentity is what one post-turn refresh announces, or null while the
// runtime reports the identity the load-time handshake already recorded. Every
// field counts, including materialization: a session that only just reached the
// disk is exactly the fact worth announcing.
export function refreshIdentity(identity: SessionIdentity, announced: SessionIdentity | null): SessionIdentity | null {
  if (announced === null) return null;
  if (identity.id === announced.id && identity.file === announced.file && identity.onDisk === announced.onDisk) {
    return null;
  }
  return identity;
}

// refreshReady re-announces the current session identity, once.
//
// It never fails a completed turn: a bridge failure warns and reports that
// nothing was announced, leaving the coordinator's recorded handshake — and with
// it the no-resume state — untouched. It is exported because it is the one place
// the refresh happens, which is what the load check drives with a recording send.
export async function refreshReady(
  identity: SessionIdentity,
  announced: SessionIdentity | null,
  send: (identity: SessionIdentity) => Promise<void>,
  warn: (message: string) => void,
): Promise<SessionIdentity | null> {
  const refresh = refreshIdentity(identity, announced);
  if (refresh === null) return null;
  try {
    await send(refresh);
    return refresh;
  } catch (error) {
    warn(`Orchestrator session refresh failed: ${String(error)}`);
    return null;
  }
}

// afterTurnRefresh performs one post-turn refresh: wait (bounded) while the runtime
// says the session is not on disk yet, then announce the identity it reports now —
// once, and only when that identity differs from the handshake already recorded.
//
// It reports a failure through warn and returns null instead of throwing, so the
// agent_end hook wired to it can never fail an otherwise completed turn. It is
// exported, and it is the function that hook calls, so the load check can drive the
// failure path with a rejecting send.
export async function afterTurnRefresh(
  identity: () => SessionIdentity,
  announced: SessionIdentity | null,
  send: (identity: SessionIdentity, signal: AbortSignal) => Promise<void>,
  warn: (message: string) => void,
): Promise<SessionIdentity | null> {
  let current = identity();
  for (let attempt = 0; !current.onDisk && attempt < MATERIALIZE_TRIES; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, MATERIALIZE_WAIT_MS));
    current = identity();
  }
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), REFRESH_TIMEOUT_MS);
  try {
    return await refreshReady(current, announced, (next) => send(next, controller.signal), warn);
  } finally {
    clearTimeout(timer);
  }
}

// warnOnce reports at most one warning per process: a refresh that keeps failing
// stays visible without repeating on every turn.
export function warnOnce(warn: (message: string) => void): (message: string) => void {
  let warned = false;
  return (message: string) => {
    if (warned) return;
    warned = true;
    warn(message);
  };
}

// toolResult and toolRefusal are the two shapes these tools answer with: the
// bridge's value, or the reason an action was not taken.
function toolResult(value: unknown) {
  return { content: [{ type: "text", text: JSON.stringify(value) }], details: value };
}

function toolRefusal(reason: string) {
  return { content: [{ type: "text", text: reason }], isError: true };
}

// callBridge runs one versioned bridge command. The Herdr binary path and the
// plugin state directory are handed to every child explicitly: an ordinary
// Herdr-managed OMP pane is not guaranteed to inherit plugin-command
// environment, so the extension cannot rely on process.env carrying them.
async function callBridge(
  method: string,
  params: unknown,
  identity: SessionIdentity,
  signal?: AbortSignal,
): Promise<unknown> {
  const request = JSON.stringify({
    version: BRIDGE_PROTOCOL,
    method,
    ompSessionID: identity.id,
    ompSessionFile: identity.file,
    ompSessionOnDisk: identity.onDisk,
    params: params ?? {},
  });

  return await new Promise<unknown>((resolve, reject) => {
    let settled = false;
    let cancelTimeout: () => void = () => {};
    const finish = (settle: () => void): void => {
      if (settled) return;
      settled = true;
      cancelTimeout();
      if (signal) signal.removeEventListener("abort", onAbort);
      settle();
    };

    const child = spawn(PLUGIN_EXECUTABLE, ["bridge", method], {
      windowsHide: true,
      stdio: ["pipe", "pipe", "pipe"],
      env: {
        ...process.env,
        HERDR_PLUGIN_STATE_DIR: PLUGIN_STATE_DIR,
        HERDR_ORCH_HERDR_BIN_PATH: HERDR_BINARY,
      },
    });

    // Decode each stream once rather than per chunk: a multi-byte character
    // split across two pipe reads must not be replaced by U+FFFD.
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");

    const out: string[] = [];
    const err: string[] = [];
    let outBytes = 0;
    let errBytes = 0;

    const overflow = (stream: "stdout" | "stderr", bytes: number): void => {
      child.kill();
      finish(() =>
        reject(
          new BridgeError({
            code: "bridge_output_too_large",
            message: `${method}: ${stream} exceeded ${MAX_OUTPUT_BYTES} bytes (${bytes})`,
          }),
        ),
      );
    };

    const onAbort = (): void => {
      child.kill();
      finish(() => reject(new BridgeError({ code: "aborted", message: `${method}: cancelled` })));
    };

    child.stdout.on("data", (chunk: string) => {
      outBytes += Buffer.byteLength(chunk, "utf8");
      if (outBytes > MAX_OUTPUT_BYTES) {
        overflow("stdout", outBytes);
        return;
      }
      out.push(chunk);
    });
    child.stderr.on("data", (chunk: string) => {
      errBytes += Buffer.byteLength(chunk, "utf8");
      if (errBytes > MAX_OUTPUT_BYTES) {
        overflow("stderr", errBytes);
        return;
      }
      err.push(chunk);
    });
    child.on("error", (error: unknown) => {
      finish(() =>
        reject(
          new BridgeError({
            code: "bridge_spawn_failed",
            message: `${method}: cannot run ${PLUGIN_EXECUTABLE}: ${String(error)}`,
          }),
        ),
      );
    });
    child.on("close", (code: number | null) => {
      const stdout = out.join("");
      const stderr = err.join("");
      let response: unknown;
      try {
        response = JSON.parse(stdout);
      } catch {
        const detail = stderr.split(/\r?\n/, 1)[0].slice(0, DETAIL_LIMIT);
        finish(() =>
          reject(
            new BridgeError({
              code: "bridge_protocol",
              message: `${method}: exit ${String(code)} without a JSON answer${detail ? `: ${detail}` : ""}`,
            }),
          ),
        );
        return;
      }
      const body: Record<string, unknown> =
        typeof response === "object" && response !== null ? (response as Record<string, unknown>) : {};
      if (body.ok === true) {
        finish(() => resolve(body.result));
        return;
      }
      const failure = readErrorBody(body.error);
      finish(() =>
        reject(
          new BridgeError(
            failure ?? {
              code: "bridge_protocol",
              message: `${method}: unrecognized bridge answer ${stdout
                .split(/\r?\n/, 1)[0]
                .slice(0, DETAIL_LIMIT)}`,
            },
          ),
        ),
      );
    });

    // The bridge may reject the request and exit before reading all of stdin.
    child.stdin.on("error", () => {});
    if (signal) {
      if (signal.aborted) {
        cancelTimeout = () => {};
        onAbort();
      } else {
        signal.addEventListener("abort", onAbort, { once: true });
      }
    }
    const timeout = setTimeout(() => {
      child.kill();
      finish(() =>
        reject(
          new BridgeError({
            code: "bridge_timeout",
            message: `${method}: no answer within ${BRIDGE_TIMEOUT_MS} ms`,
          }),
        ),
      );
    }, BRIDGE_TIMEOUT_MS);
    cancelTimeout = () => clearTimeout(timeout);
    if (settled) {
      // The promise already settled (an aborted signal before the child was
      // ready); the timer above must not keep the runtime alive.
      clearTimeout(timeout);
      return;
    }
    child.stdin.end(request);
  });
}

export default function (pi: ExtensionAPI) {
  // Only Herdr-managed pane processes belong to this plugin. Every other OMP
  // session keeps its normal tool set untouched.
  if (process.env.HERDR_ENV !== "1" || !process.env.HERDR_PANE_ID) return;

  const z = pi.zod;
  // A primary decides and relays; a worker reports. The tool sets are disjoint,
  // and the bridge enforces the same split on every call, so an active tool is
  // never the only thing standing between a role and the other role's actions.
  const primaryNames = [
    "orch_dispatch",
    "orch_answer",
    "orch_approve_dialog",
    "orch_review",
    "orch_settle_input",
    "orch_adjudicate_dispatch",
    "orch_cleanup",
  ];
  const workerNames = ["orch_progress", "orch_question", "orch_report"];
  const ownNames = [...primaryNames, ...workerNames];
  let role: Role | null = null;

  // activate asks the bridge to prove this process's binding, then activates
  // exactly the verified role's tools. It is idempotent: only this plugin's
  // tool names are filtered, so other OMP tools and other extensions' tools are
  // preserved, and nothing is written when the active set already matches.
  async function activate(identity: SessionIdentity): Promise<Role | null> {
    const binding = readResolveBinding(await callBridge("resolve", {}, identity));
    const nextRole = binding.bound ? binding.role : null;
    const target = nextRole === "primary" ? primaryNames : nextRole === "worker" ? workerNames : [];
    const current = pi.getActiveTools();
    const desired = [
      ...new Set([...current.filter((name: string) => !ownNames.includes(name)), ...target]),
    ];
    if (desired.join("\0") !== current.join("\0")) await pi.setActiveTools(desired);
    role = nextRole;
    if (role) await announce(identity);
    return role;
  }

  pi.on("session_start", async (_event, ctx) => {
    try {
      await activate(sessionIdentity(ctx.sessionManager));
    } catch (error) {
      ctx.ui.notify(`Orchestrator binding unavailable: ${String(error)}`, "warning");
      throw error;
    }
  });

  // Herdr can restart an agent process with a new launch-time pane id, and the
  // plugin's own startup reconciliation can run after that process already
  // loaded this extension. Re-resolving before every user turn lets a verified
  // rebind restore the right tools instead of silently disabling them forever.
  // The handshake the coordinator holds, as this process last announced it.
  let announced: SessionIdentity | null = null;
  let warnRefresh: ((message: string) => void) | null = null;

  // announce records the identity surface for the bound role and remembers what
  // was announced, so the post-turn refresh can tell whether anything changed.
  async function announce(identity: SessionIdentity): Promise<void> {
    await callBridge("ready", {}, identity);
    announced = identity;
  }

  // A completed turn is where a lazily materialized session reaches the disk.
  // Re-announce then, so the coordinator can offer a resume hint for a session
  // that really exists — and never let this fail the turn: a bridge failure warns
  // once and leaves the recorded handshake, and with it the no-resume state, alone.
  pi.on("agent_end", async (_event, ctx) => {
    if (role === null) return;
    // One warning per process, whichever turn first shows the failure.
    const warn = (warnRefresh ??= warnOnce((message: string): void => ctx.ui.notify(message, "warning")));
    try {
      const refreshed = await afterTurnRefresh(
        () => sessionIdentity(ctx.sessionManager),
        announced,
        (next, signal) => callBridge("ready", {}, next, signal),
        warn,
      );
      if (refreshed !== null) announced = refreshed;
    } catch (error) {
      // The hook is the last thing between a completed turn and the user: only an
      // unexpected failure reaches here, and it must not escape.
      warn(`Orchestrator session refresh failed: ${String(error)}`);
    }
  });

  pi.on("before_agent_start", async (event, ctx) => {
    const currentRole = await activate(sessionIdentity(ctx.sessionManager));
    if (!currentRole) return;
    return {
      systemPrompt: [
        ...event.systemPrompt,
        currentRole === "primary"
          ? "You are the sole user liaison. Delegate independent bounded work with orch_dispatch, avoid editing paths reserved for workers, and keep the user in the loop: relay an ordinary worker question with orch_answer, approve a blocked worker's exact dialog keys with orch_approve_dialog only after the user confirms them, record the user's decision on a reviewed report with orch_review, and remove a reviewed checkout with orch_cleanup only after the user confirms it. A worker's report is never a merge and never completes the task: only a disposition the user recorded does. Never approve keys or remove a checkout the user has not confirmed. If sending an answer or approved keys is left uncertain (a timeout), nothing else may be sent to that worker until you inspect its pane with the user and settle it with orch_settle_input."
          : "Work only in this assigned worktree. Use orch_progress for partial progress, orch_question when you are blocked and need the primary to ask the user, and orch_report with a complete result — summary, branch, changed-file/commit evidence, checks and a stated risk — when you are finished. Only the primary records the user's disposition for your result; do not merge and do not remove the branch or the checkout.",
      ],
    };
  });

  pi.registerTool({
    name: "orch_dispatch",
    label: "Delegate task",
    defaultInactive: true,
    description: "Start a bounded, isolated coding task.",
    // Schema rule: never pass a mutable value (array or object) to `.default()`
    // — OMP's builder rejects it at construction time ("A mutable default value
    // must be specified as a factory"), which makes the whole extension fail to
    // load. Use a factory, or `.optional()`. omp/load-extension.check.ts runs
    // this schema through OMP's own builder so the mistake cannot ship.
    parameters: z.object({
      title: z.string().min(1).max(160),
      brief: z.string().min(1).max(16000),
      checks: z.array(z.string().min(1).max(512)).min(1),
      dependencies: z.array(z.string()).default(() => []),
      reservedPaths: z.array(z.string()).default(() => []),
      baseRevision: z.string().min(7),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("dispatch", params, sessionIdentity(ctx.sessionManager), signal);
      return { content: [{ type: "text", text: JSON.stringify(result) }], details: result };
    },
  });

  pi.registerTool({
    name: "orch_progress",
    label: "Report progress",
    defaultInactive: true,
    description: "Report partial progress to the primary. This never completes the task.",
    parameters: z.object({
      summary: z.string().min(1).max(2000),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("progress", params, sessionIdentity(ctx.sessionManager), signal);
      return toolResult(result);
    },
  });

  pi.registerTool({
    name: "orch_question",
    label: "Ask the primary",
    defaultInactive: true,
    description:
      "Ask the primary (and the user) a question you cannot answer yourself. The task becomes Needs input until the answer is relayed back.",
    parameters: z.object({
      text: z.string().min(1).max(4000),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("question", params, sessionIdentity(ctx.sessionManager), signal);
      return toolResult(result);
    },
  });

  pi.registerTool({
    name: "orch_report",
    label: "Report result",
    defaultInactive: true,
    description:
      "Submit a complete final result for review: summary, branch, changed-file/commit evidence, checks and a stated risk. The task becomes Ready for review; only the primary's recorded disposition completes it.",
    parameters: z.object({
      kind: z.literal("result"),
      summary: z.string().min(1).max(4000),
      branch: z.string().min(1),
      evidence: z.array(z.string().min(1).max(512)).min(1),
      checks: z.array(z.string().min(1).max(512)).min(1),
      risk: z.string().min(1).max(4000),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("report", params, sessionIdentity(ctx.sessionManager), signal);
      return toolResult(result);
    },
  });

  pi.registerTool({
    name: "orch_answer",
    label: "Answer worker",
    defaultInactive: true,
    description:
      "Relay the user's answer to a worker that asked a question. The answer goes to that task's current public pane only.",
    parameters: z.object({
      taskId: z.string().min(1),
      text: z.string().min(1).max(8000),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("answer", params, sessionIdentity(ctx.sessionManager), signal);
      return toolResult(result);
    },
  });

  pi.registerTool({
    name: "orch_approve_dialog",
    label: "Approve worker choice",
    defaultInactive: true,
    description:
      "Approve the exact keys of a blocked worker's full-screen dialog, after the user confirms those keys. Never a blanket approval, and never for a dialog the user has not seen.",
    parameters: z.object({
      taskId: z.string().min(1),
      keys: z.array(z.string().min(1).max(64)).min(1).max(16),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const identity = sessionIdentity(ctx.sessionManager);
      const preview = readDialogPreview(await callBridge("dialog_preview", { taskId: params.taskId }, identity, signal));
      const plan = dialogConfirmRequest(preview, params.keys);
      if (!plan.ok) return toolRefusal(plan.reason);
      const gated = await confirmThen(uiGate(ctx), plan.request, () =>
        callBridge(
          "approve_dialog",
          { taskId: params.taskId, observedDigest: preview.digest, keys: params.keys },
          identity,
          signal,
        ),
      );
      if (!gated.ok) return toolRefusal(gated.reason);
      return toolResult(gated.value);
    },
  });

  pi.registerTool({
    name: "orch_review",
    label: "Record decision",
    defaultInactive: true,
    description:
      "Ask the user to confirm the task and proposed disposition after reviewing the worker result. A report alone never moves a task to Done.",
    parameters: z.object({
      taskId: z.string().min(1),
      disposition: z.string().min(1).max(200),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const gate = uiGate(ctx);
      if (!gate.hasUI) return toolRefusal("this session has no interactive UI, so the user could not confirm and nothing was done");
      const identity = sessionIdentity(ctx.sessionManager);
      const preview = readReportPreview(await callBridge("report_preview", { taskId: params.taskId }, identity, signal));
      if (!preview || preview.taskId !== params.taskId) return toolRefusal("the persisted report could not be verified for this task");
      const gated = await recordReviewWithConsent(gate, preview, params.disposition, (confirmed) =>
        callBridge("review", confirmed, identity, signal),
      );
      if (!gated.ok) return toolRefusal(gated.reason);
      return toolResult(gated.value);
    },
  });

  pi.registerTool({
    name: "orch_settle_input",
    label: "Settle worker input",
    defaultInactive: true,
    description:
      "Record what the user saw in a worker's pane after an input attempt was left uncertain (a timeout or an unreadable answer): whether it arrived. Until the user confirms this, nothing else may be sent to that worker.",
    parameters: z.object({
      taskId: z.string().min(1),
      decision: z.string().min(1).max(20),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const identity = sessionIdentity(ctx.sessionManager);
      const plan = settleConfirmRequest(params.taskId, params.decision);
      if (!plan.ok) return toolRefusal(plan.reason);
      const gated = await confirmThen(uiGate(ctx), plan.request, () =>
        callBridge("settle_input", { taskId: params.taskId, decision: params.decision }, identity, signal),
      );
      if (!gated.ok) return toolRefusal(gated.reason);
      return toolResult(gated.value);
    },
  });

  pi.registerTool({
    name: "orch_adjudicate_dispatch",
    label: "Decide an uncertain dispatch",
    defaultInactive: true,
    description:
      "Resolve a brief whose outcome is uncertain: after you inspect the worker's pane, record whether it arrived. This also covers a prompt accepted by Herdr when the task could not persist its submitted phase. \"delivered\" records the submission without sending it again; \"not_delivered\" clears that blocker and retries once. Nothing else can clear that blocker.",
    parameters: z.object({
      taskId: z.string().min(1),
      decision: z.enum(["delivered", "not_delivered"]),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const identity = sessionIdentity(ctx.sessionManager);
      const inspection = readBriefInspection(
        await callBridge("brief_inspection", { taskId: params.taskId }, identity, signal),
      );
      const plan = adjudicateConfirmRequest(inspection, params.decision);
      if (!plan.ok) return toolRefusal(plan.reason);
      const gated = await confirmThen(uiGate(ctx), plan.request, () =>
        callBridge(
          "adjudicate_brief",
          {
            taskId: inspection.taskId,
            expectedRevision: inspection.revision,
            decision: params.decision,
            paneId: inspection.paneId,
            digest: inspection.digest,
          },
          identity,
          signal,
        ),
      );
      if (!gated.ok) return toolRefusal(gated.reason);
      return toolResult(gated.value);
    },
  });

  pi.registerTool({
    name: "orch_cleanup",
    label: "Remove worker checkout",
    defaultInactive: true,
    description:
      "Remove a reviewed worker's checkout after the user confirms it. The branch is kept, the removal is never forced, and a live worker or a dirty checkout is refused.",
    parameters: z.object({
      taskId: z.string().min(1),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const identity = sessionIdentity(ctx.sessionManager);
      const preview = readCleanupPreview(await callBridge("cleanup_preview", { taskId: params.taskId }, identity, signal));
      const plan = cleanupConfirmRequest(preview);
      if (!plan.ok) return toolRefusal(plan.reason);
      const gated = await confirmThen(uiGate(ctx), plan.request, () =>
        callBridge("cleanup", { taskId: params.taskId }, identity, signal),
      );
      if (!gated.ok) return toolRefusal(gated.reason);
      return toolResult(gated.value);
    },
  });
}
