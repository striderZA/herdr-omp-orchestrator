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

// callBridge runs one versioned bridge command. The Herdr binary path and the
// plugin state directory are handed to every child explicitly: an ordinary
// Herdr-managed OMP pane is not guaranteed to inherit plugin-command
// environment, so the extension cannot rely on process.env carrying them.
async function callBridge(
  method: string,
  params: unknown,
  ompSessionID: string,
  signal?: AbortSignal,
): Promise<unknown> {
  const request = JSON.stringify({
    version: BRIDGE_PROTOCOL,
    method,
    ompSessionID,
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

    child.stdout.on("data", (chunk: Uint8Array) => {
      outBytes += chunk.length;
      if (outBytes > MAX_OUTPUT_BYTES) {
        overflow("stdout", outBytes);
        return;
      }
      out.push(String(chunk));
    });
    child.stderr.on("data", (chunk: Uint8Array) => {
      errBytes += chunk.length;
      if (errBytes > MAX_OUTPUT_BYTES) {
        overflow("stderr", errBytes);
        return;
      }
      err.push(String(chunk));
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
  const primaryNames = ["orch_dispatch"];
  const workerNames = ["orch_report"];
  const ownNames = [...primaryNames, ...workerNames];
  let role: Role | null = null;

  // activate asks the bridge to prove this process's binding, then activates
  // exactly the verified role's tools. It is idempotent: only this plugin's
  // tool names are filtered, so other OMP tools and other extensions' tools are
  // preserved, and nothing is written when the active set already matches.
  async function activate(sessionID: string): Promise<Role | null> {
    const binding = readResolveBinding(await callBridge("resolve", {}, sessionID));
    const nextRole = binding.bound ? binding.role : null;
    const target = nextRole === "primary" ? primaryNames : nextRole === "worker" ? workerNames : [];
    const current = pi.getActiveTools();
    const desired = [
      ...new Set([...current.filter((name: string) => !ownNames.includes(name)), ...target]),
    ];
    if (desired.join("\0") !== current.join("\0")) await pi.setActiveTools(desired);
    role = nextRole;
    if (role) await callBridge("ready", {}, sessionID);
    return role;
  }

  pi.on("session_start", async (_event, ctx) => {
    try {
      await activate(ctx.sessionManager.getSessionId());
    } catch (error) {
      ctx.ui.notify(`Orchestrator binding unavailable: ${String(error)}`, "warning");
      throw error;
    }
  });

  // Herdr can restart an agent process with a new launch-time pane id, and the
  // plugin's own startup reconciliation can run after that process already
  // loaded this extension. Re-resolving before every user turn lets a verified
  // rebind restore the right tools instead of silently disabling them forever.
  pi.on("before_agent_start", async (event, ctx) => {
    const currentRole = await activate(ctx.sessionManager.getSessionId());
    if (!currentRole) return;
    return {
      systemPrompt: [
        ...event.systemPrompt,
        currentRole === "primary"
          ? "You are the sole user liaison. Delegate independent bounded work with orch_dispatch, avoid editing paths reserved for workers, relay their questions to the user, and request a decision before integrating reports."
          : "Work only in this assigned worktree. Use orch_question when blocked and orch_report with changed-file/commit evidence and checks when finished. Do not merge or remove the branch.",
      ],
    };
  });

  pi.registerTool({
    name: "orch_dispatch",
    label: "Delegate task",
    defaultInactive: true,
    description: "Start a bounded, isolated coding task.",
    parameters: z.object({
      title: z.string().min(1).max(160),
      brief: z.string().min(1).max(16000),
      checks: z.array(z.string().min(1).max(512)).min(1),
      dependencies: z.array(z.string()).default([]),
      reservedPaths: z.array(z.string()).default([]),
      baseRevision: z.string().min(7),
    }),
    async execute(_callId, params, signal, _update, ctx) {
      const result = await callBridge("dispatch", params, ctx.sessionManager.getSessionId(), signal);
      return { content: [{ type: "text", text: JSON.stringify(result) }], details: result };
    },
  });

  pi.registerTool({
    name: "orch_report",
    label: "Report result",
    defaultInactive: true,
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
