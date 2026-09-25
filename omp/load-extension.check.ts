// Load check for omp/herdr-orchestrator.ts.
//
// Transpiling is not loading: this script imports the extension, runs its
// factory against a stub ExtensionAPI whose `zod` is OMP's own schema builder,
// and then exercises the recorded tool schemas. OMP builds every tool's
// `parameters` while the factory runs, so a builder-level mistake (for example
// a mutable `.default([])`) throws there and is caught here instead of on the
// user's machine, where it reads:
//
//   Warning: Failed to load extension ~/.omp/agent/extensions/herdr-orchestrator.ts:
//   Failed to load extension: ParseError: A mutable default value must be specified as a factory
//
// Rule for every tool schema in this plugin: never hand a mutable value to
// `.default(...)` — pass a factory (`.default(() => [])`) or make the field
// `.optional()`. Arrays and objects are what that rule is about; strings,
// numbers and booleans are immutable and fine.
//
// Run it with the OMP builder resolvable, e.g.
//   NODE_PATH=%USERPROFILE%\node_modules bun omp/load-extension.check.ts
// or point OMP_TYPED_BUILDER straight at the zod module file.
import extension, {
  afterTurnRefresh,
  cleanupConfirmRequest,
  confirmThen,
  dialogConfirmRequest,
  refreshIdentity,
  refreshReady,
  uiGate,
  warnOnce,
} from "./herdr-orchestrator.ts";

// OMP's builder returns a *callable* schema (ArkType-compatible validation from
// @oh-my-pi/omptype), not zod's plain object: it has .parse() and no .shape.
type ParameterSchema = { parse(value: unknown): unknown; shape?: Record<string, unknown> } & (
  | Record<string, unknown>
  | ((...args: unknown[]) => unknown)
);

interface RegisteredTool {
  name: string;
  label: string;
  description: string;
  defaultInactive: boolean;
  parameters: ParameterSchema;
}

interface ZodBuilder {
  object(shape: Record<string, unknown>): ParameterSchema;
}

interface StubAPI {
  zod: ZodBuilder;
  getActiveTools(): string[];
  setActiveTools(names: string[]): Promise<void>;
  registerTool(tool: RegisteredTool): void;
  on(event: string, handler: (...args: unknown[]) => unknown): void;
}

// ExtensionHandler is one registered event handler, kept so the check can invoke
// the hooks that run outside a turn (or at its end) by hand.
type ExtensionHandler = (...args: unknown[]) => unknown;

const failures: string[] = [];
function check(condition: boolean, message: string): void {
  if (!condition) failures.push(message);
}

// Static import cannot work here: which module is OMP's builder is a runtime
// fact of the machine's OMP installation, so the specifier is selected from the
// environment and every candidate is shape-checked below.
function readBuilder(module: unknown): ZodBuilder {
  if (typeof module !== "object" || module === null) {
    throw new Error("the builder module is not an object");
  }
  const candidates: unknown[] = [];
  if ("z" in module) candidates.push(module.z);
  if ("default" in module) candidates.push(module.default);
  candidates.push(module);
  for (const candidate of candidates) {
    if (typeof candidate !== "object" || candidate === null || !("object" in candidate)) continue;
    const build = candidate.object;
    if (typeof build === "function") {
      // Reason: OMP re-exports one builder under different names across
      // versions; the `.object(...)` check above is the runtime proof, and this
      // casts to the small subset the check actually uses.
      return candidate as ZodBuilder;
    }
  }
  throw new Error("no schema builder with .object() was found in the module");
}

const builderSpecifier = process.env.OMP_TYPED_BUILDER || "@oh-my-pi/omptype/zod";
let z: ZodBuilder;
try {
  z = readBuilder(await import(builderSpecifier));
} catch (error) {
  console.error(`load check cannot import OMP's schema builder ${builderSpecifier}: ${String(error)}`);
  process.exit(3);
}

// The extension only registers its tools inside a Herdr pane process.
process.env.HERDR_ENV = "1";
process.env.HERDR_PANE_ID = "w1:p2";

const tools: RegisteredTool[] = [];
const events: string[] = [];
const handlers = new Map<string, ExtensionHandler>();
const active = new Set(["read", "write"]);
const setActiveCalls: string[][] = [];

const stub: StubAPI = {
  zod: z,
  getActiveTools: () => [...active],
  setActiveTools: async (names: string[]) => {
    setActiveCalls.push([...names]);
    active.clear();
    for (const name of names) active.add(name);
  },
  registerTool: (tool: RegisteredTool) => {
    tools.push(tool);
  },
  on: (event: string, handler: ExtensionHandler) => {
    events.push(event);
    handlers.set(event, handler);
  },
};

// Reason: OMP's ExtensionAPI is erased at runtime and far larger than the
// surface the extension touches; this stub implements that surface and the
// schemas it records are exercised immediately below.
const load = extension as unknown as (api: StubAPI) => void;
try {
  load(stub);
} catch (error) {
  // OMP surfaces this as "Failed to load extension" and registers none of the
  // tools, so the check fails the same way with the cause attached.
  console.error(`load check FAILED: the extension factory threw at load: ${String(error)}`);
  if (error instanceof Error && error.stack) console.error(error.stack);
  process.exit(1);
}

// --- registrations ----------------------------------------------------------

const names = tools.map((tool) => tool.name).sort();
const wantNames = [
  "orch_answer",
  "orch_approve_dialog",
  "orch_cleanup",
  "orch_dispatch",
  "orch_progress",
  "orch_question",
  "orch_report",
  "orch_review",
];
check(
  names.join(",") === wantNames.join(","),
  `registered tools are ${JSON.stringify(names)}, want ${JSON.stringify(wantNames)}`,
);
check(events.includes("session_start"), "the extension does not listen to session_start");
check(events.includes("before_agent_start"), "the extension does not listen to before_agent_start");
// OMP materializes a session lazily: the file appears with the first assistant
// message, so the identity announced at load time can still report onDisk false.
// The extension has to re-announce after a turn, or no resume hint can ever be
// offered for a session that is really on disk.
check(events.includes("agent_end"), "the extension does not listen to agent_end");

for (const tool of tools) {
  check(tool.defaultInactive === true, `${tool.name} must be registered inactive`);
  check(tool.label.length > 0, `${tool.name} has no label`);
  check(tool.description.length > 0, `${tool.name} has no description`);
  check(
    (typeof tool.parameters === "object" && tool.parameters !== null) || typeof tool.parameters === "function",
    `${tool.name} has no parameters schema`,
  );
  check(
    typeof (tool.parameters as ParameterSchema | undefined)?.parse === "function",
    `${tool.name} parameters schema was not built by OMP's builder (no .parse)`,
  );
}

function requireSchema(tool: RegisteredTool): ParameterSchema {
  const schema = tool.parameters;
  if (!schema || typeof schema.parse !== "function") {
    throw new Error(`${tool.name} was registered without a usable parameters schema`);
  }
  return schema;
}

// rebuildSchema re-runs one tool's schema through OMP's builder when the built
// schema still exposes its shape. OMP's omptype builder hands back a callable
// schema without a shape, and a mutable default throws while the factory itself
// builds it — which is why the factory run above (and its try/catch) is the
// primary construction guard, and this is the extra one when a shape is available.
function rebuildSchema(schema: ParameterSchema): { ok: true; rebuilt: boolean } | { ok: false; error: string } {
  const shape = schema.shape;
  if (typeof shape !== "object" || shape === null) {
    return { ok: true, rebuilt: false };
  }
  try {
    z.object(shape);
    return { ok: true, rebuilt: true };
  } catch (error) {
    return { ok: false, error: String(error) };
  }
}

// arrayField reads one array field of a parsed value without asserting a shape:
// anything else (missing, wrong type) reports null and becomes a failure below.
function arrayField(value: unknown, key: string): unknown[] | null {
  if (typeof value !== "object" || value === null || !(key in value)) return null;
  const field = value[key];
  return Array.isArray(field) ? field : null;
}

// tryParse reads a parse outcome without letting a schema rejection crash the
// check: a rejection is a failure to report, not a stack trace to decode.
type ParseOutcome = { ok: true; value: unknown } | { ok: false; error: string };
function tryParse(schema: ParameterSchema, payload: unknown): ParseOutcome {
  try {
    return { ok: true, value: schema.parse(payload) };
  } catch (error) {
    return { ok: false, error: String(error) };
  }
}

// --- orch_dispatch ----------------------------------------------------------

const dispatch = tools.find((tool) => tool.name === "orch_dispatch");
if (!dispatch) {
  failures.push("orch_dispatch was not registered");
} else {
  const schema = requireSchema(dispatch);
  // Rebuilding the schema is the work OMP repeats at registration; a mutable
  // default throws here.
  const rebuilt = rebuildSchema(schema);
  check(
    rebuilt.ok,
    `orch_dispatch parameters cannot be rebuilt by OMP's builder${rebuilt.ok ? "" : `: ${rebuilt.error}`}`,
  );
  check(
    typeof schema.parse === "function",
    "orch_dispatch parameters is not a schema OMP's builder produced",
  );

  const payload = {
    title: "Bounded task",
    brief: "Do the bounded thing.",
    checks: ["go test ./..."],
    baseRevision: "0123456",
  };
  const first = tryParse(schema, payload);
  if (!first.ok) {
    failures.push(`orch_dispatch rejected a minimal valid call: ${first.error}`);
  } else {
    const firstDeps = arrayField(first.value, "dependencies");
    if (firstDeps === null) {
      failures.push("orch_dispatch dependencies default is not an array");
    } else {
      check(firstDeps.length === 0, `orch_dispatch dependencies default = ${JSON.stringify(firstDeps)}, want []`);
      // A factory default hands every caller its own array; a shared instance
      // would leak this mutation into the next parse.
      firstDeps.push("leaked");
    }
    const firstReserved = arrayField(first.value, "reservedPaths");
    if (firstReserved === null) {
      failures.push("orch_dispatch reservedPaths default is not an array");
    } else {
      check(
        firstReserved.length === 0,
        `orch_dispatch reservedPaths default = ${JSON.stringify(firstReserved)}, want []`,
      );
    }
  }
  const second = tryParse(schema, payload);
  const secondDeps = second.ok ? arrayField(second.value, "dependencies") : null;
  check(
    secondDeps !== null && secondDeps.length === 0,
    `orch_dispatch defaults are shared between parses: the second call sees ${JSON.stringify(secondDeps)}`,
  );
  check(
    !tryParse(schema, { title: "", brief: "", checks: [], baseRevision: "x" }).ok,
    "orch_dispatch accepted an empty title/brief/checks/baseRevision",
  );
}

// --- orch_report ------------------------------------------------------------

const report = tools.find((tool) => tool.name === "orch_report");
if (!report) {
  failures.push("orch_report was not registered");
} else {
  const schema = requireSchema(report);
  const valid = {
    kind: "result",
    summary: "done",
    branch: "orch/abc",
    evidence: ["commit abc"],
    checks: ["go test ./..."],
    risk: "none: no new path is reachable",
  };
  const parsed = tryParse(schema, valid);
  if (!parsed.ok) {
    failures.push(`orch_report rejected a valid final report: ${parsed.error}`);
  } else {
    const kind =
      typeof parsed.value === "object" && parsed.value !== null && "kind" in parsed.value
        ? parsed.value.kind
        : undefined;
    check(kind === "result", `orch_report accepted kind ${JSON.stringify(kind)}`);
    const checks = arrayField(parsed.value, "checks");
    check(
      checks !== null && checks.length === 1,
      `orch_report checks round trip = ${JSON.stringify(checks)}, want the submitted checks`,
    );
  }
  check(
    !tryParse(schema, { kind: "result", summary: "done", branch: "orch/abc", evidence: ["commit abc"], risk: "none" }).ok,
    "orch_report accepted a final report without checks",
  );
  check(
    !tryParse(schema, { ...valid, kind: "partial" }).ok,
    "orch_report accepted kind partial; only kind result is final",
  );
  // The schema requires the field; a whitespace-only risk is what the
  // coordinator's own trim refuses on the way in.
  check(
    !tryParse(schema, { ...valid, risk: "" }).ok,
    "orch_report accepted a report without a stated risk",
  );
  check(
    !tryParse(schema, { ...valid, evidence: [] }).ok,
    "orch_report accepted a report without changed-file/commit evidence",
  );
}

// --- the rest of the typed tool parameters ---------------------------------

// Every tool the two roles use has to build under OMP's builder and accept the
// call the model is told to make; a missing or too-loose schema is a load
// failure waiting for the user's machine.
const toolCalls: { name: string; params: Record<string, unknown>; bad: Record<string, unknown> }[] = [
  { name: "orch_progress", params: { summary: "ported the lexer" }, bad: { summary: "" } },
  { name: "orch_question", params: { text: "Which branch should I target?" }, bad: { text: "" } },
  { name: "orch_answer", params: { taskId: "orch_abc", text: "Keep the old API." }, bad: { taskId: "", text: "" } },
  {
    name: "orch_approve_dialog",
    params: { taskId: "orch_abc", keys: ["1"] },
    bad: { taskId: "orch_abc", keys: [] },
  },
  { name: "orch_review", params: { taskId: "orch_abc", disposition: "accepted" }, bad: { taskId: "orch_abc" } },
  { name: "orch_cleanup", params: { taskId: "orch_abc" }, bad: {} },
];

for (const call of toolCalls) {
  const tool = tools.find((candidate) => candidate.name === call.name);
  if (!tool) {
    failures.push(`${call.name} was not registered`);
    continue;
  }
  const schema = requireSchema(tool);
  const rebuilt = rebuildSchema(schema);
  check(rebuilt.ok, `${call.name} parameters cannot be rebuilt by OMP's builder${rebuilt.ok ? "" : `: ${rebuilt.error}`}`);
  const parsed = tryParse(schema, call.params);
  check(parsed.ok, `${call.name} rejected the call its description tells the model to make: ${parsed.ok ? "" : parsed.error}`);
  check(
    !tryParse(schema, call.bad).ok,
    `${call.name} accepted an empty ${JSON.stringify(call.bad)}`,
  );
}

// --- the user-confirmed gates ----------------------------------------------

// A UI confirmation is the only thing that may send keys or remove a checkout.
// These checks exercise the gate itself: no UI and a declined confirmation must
// both return before the action runs, which is what makes "nothing was done"
// true rather than hopeful.
async function checkGates(): Promise<void> {
  const workerDialog = {
    taskId: "orch_abc",
    publicPaneId: "w2:p1",
    agentName: "orch_abc",
    agentStatus: "blocked",
    blocked: true,
    dialog: "  1. Allow once\n  2. Deny",
    digest: "0123456789abcdef",
  };

  const plan = dialogConfirmRequest(workerDialog, ["1"]);
  if (!plan.ok) {
    failures.push(`the dialog approval was refused for a blocked worker: ${plan.reason}`);
  } else {
    check(
      plan.request.title === "Approve worker choice",
      `the dialog confirmation is titled ${JSON.stringify(plan.request.title)}`,
    );
    check(
      plan.request.message.includes(workerDialog.dialog) && plan.request.message.includes("Keys: 1"),
      `the dialog confirmation does not name the dialog and its keys: ${JSON.stringify(plan.request.message)}`,
    );
  }
  check(
    !dialogConfirmRequest(workerDialog, []).ok,
    "the dialog approval accepted an empty key list",
  );
  check(
    !dialogConfirmRequest({ ...workerDialog, blocked: false }, ["1"]).ok,
    "the dialog approval accepted a worker that is not blocked",
  );
  check(
    !dialogConfirmRequest({ ...workerDialog, digest: "" }, ["1"]).ok,
    "the dialog approval accepted a dialog with no digest",
  );

  const checkout = {
    taskId: "orch_abc",
    branch: "orch/abc",
    path: "C:/worktrees/orch-abc",
    workspaceId: "w7",
    reviewed: true,
    clean: true,
    liveWorker: false,
    removable: true,
    detail: "removes the checkout",
  };
  const removal = cleanupConfirmRequest(checkout);
  if (!removal.ok) {
    failures.push(`the removal was refused for a reviewed clean checkout: ${removal.reason}`);
  } else {
    check(
      removal.request.title === "Remove worker checkout",
      `the removal confirmation is titled ${JSON.stringify(removal.request.title)}`,
    );
    for (const named of [checkout.taskId, checkout.branch, checkout.path]) {
      check(
        removal.request.message.includes(named),
        `the removal confirmation does not name ${named}: ${JSON.stringify(removal.request.message)}`,
      );
    }
  }
  check(!cleanupConfirmRequest({ ...checkout, reviewed: false }).ok, "the removal accepted an unreviewed task");
  check(!cleanupConfirmRequest({ ...checkout, liveWorker: true }).ok, "the removal accepted a live worker");
  check(!cleanupConfirmRequest({ ...checkout, clean: false, detail: "?? notes.txt" }).ok, "the removal accepted a dirty checkout");
  check(!cleanupConfirmRequest({ ...checkout, path: "" }).ok, "the removal accepted a task with no recorded checkout");

  // A checkout Herdr no longer holds in a workspace is still removable: the
  // bridge opens it by its recorded path first, which is the state a move leaves
  // behind. The confirmation must say so instead of naming a workspace.
  const unheld = cleanupConfirmRequest({ ...checkout, workspaceId: "", detail: "opens it by its recorded path first" });
  if (!unheld.ok) {
    failures.push(`the removal refused a checkout no workspace holds: ${unheld.reason}`);
  } else {
    check(
      unheld.request.message.includes(checkout.path) &&
        unheld.request.message.includes("opened by its recorded path first"),
      `the unheld-checkout confirmation does not explain the re-open: ${JSON.stringify(unheld.request.message)}`,
    );
  }

  // No UI: the action must not run at all.
  let acted = 0;
  const noUI = await confirmThen(
    { hasUI: false, confirm: async () => true },
    { title: "Remove worker checkout", message: "x" },
    async () => {
      acted++;
      return "removed";
    },
  );
  check(!noUI.ok, "the gate proceeded without an interactive UI");
  check(acted === 0, "the gate ran the action without an interactive UI");

  // A declined confirmation: the action must not run at all.
  acted = 0;
  const declined = await confirmThen(
    { hasUI: true, confirm: async () => false },
    { title: "Remove worker checkout", message: "x" },
    async () => {
      acted++;
      return "removed";
    },
  );
  check(!declined.ok, "the gate proceeded after the user declined");
  check(acted === 0, "the gate ran the action after the user declined");

  // A confirmation that throws is a refusal too, not a green light.
  acted = 0;
  const failed = await confirmThen(
    {
      hasUI: true,
      confirm: async () => {
        throw new Error("the dialog was abandoned");
      },
    },
    { title: "Remove worker checkout", message: "x" },
    async () => {
      acted++;
      return "removed";
    },
  );
  check(!failed.ok, "the gate proceeded after the confirmation failed");
  check(acted === 0, "the gate ran the action after the confirmation failed");

  // An answered confirmation runs the action exactly once.
  acted = 0;
  const approved = await confirmThen(
    { hasUI: true, confirm: async () => true },
    { title: "Remove worker checkout", message: "x" },
    async () => {
      acted++;
      return "removed";
    },
  );
  check(approved.ok, "the gate refused an approved confirmation");
  check(acted === 1, `the approved gate ran the action ${acted} times, want 1`);

  // The adapter reads the context it was given, never a guess.
  check(uiGate({ hasUI: false, ui: { confirm: async () => true } }).hasUI === false, "uiGate invented a UI");
  check(uiGate({ hasUI: true, ui: { confirm: async () => true } }).hasUI === true, "uiGate dropped a UI");
}

await checkGates();

// --- the post-turn session refresh -----------------------------------------

// The ready refresh is asserted through the function the agent_end hook uses, with
// the bridge send replaced by a recorder: spawning cannot be intercepted under
// bun (mock.module does not reach an already-imported module's bindings, and no
// stub executable can be handed to a .ts import), so the payload is pinned where
// it is produced and the hook's safety is asserted separately.
async function checkSessionRefresh(): Promise<void> {
  const loaded: SessionIdentity = { id: "sess-1", file: "C:\s.jsonl", onDisk: false };
  const materialized: SessionIdentity = { id: "sess-1", file: "C:\s.jsonl", onDisk: true };

  check(
    refreshIdentity(materialized, loaded) !== null,
    "the refresh saw nothing to announce although the runtime reported the session on disk",
  );
  const announced = refreshIdentity(materialized, loaded);
  check(
    announced?.onDisk === true && announced?.id === "sess-1" && announced?.file === "C:\s.jsonl",
    `the refreshed identity is ${JSON.stringify(announced)}, want the session the runtime reports now`,
  );
  check(
    refreshIdentity(loaded, loaded) === null,
    "an unchanged identity was refreshed, which would re-announce every turn",
  );

  // Nothing is announced before the load-time handshake has been recorded.
  let sentIdle = 0;
  const idle = await refreshReady(materialized, null, async () => {
    sentIdle++;
  }, () => {});
  check(idle === null && sentIdle === 0, "a refresh was sent before any handshake was recorded");

  // The ready case: exactly one send, carrying the refreshed identity.
  const sent: SessionIdentity[] = [];
  let warned: string[] = [];
  const refreshed = await refreshReady(materialized, loaded, async (identity) => {
    sent.push(identity);
  }, (message) => warned.push(message));
  check(sent.length === 1, `the refresh sent ${sent.length} ready calls, want exactly 1`);
  check(
    sent[0]?.onDisk === true && sent[0]?.id === "sess-1" && sent[0]?.file === "C:\s.jsonl",
    `the second ready carried ${JSON.stringify(sent[0])}, want the refreshed identity with onDisk true`,
  );
  check(refreshed?.onDisk === true, "the refresh did not report the identity it announced");

  // A failing bridge warns once and fails nothing: the coordinator keeps its
  // no-resume state, and the completed turn is untouched.
  const failed = await refreshReady(materialized, loaded, async () => {
    throw new Error("the bridge is unavailable");
  }, (message) => warned.push(message));
  check(failed === null, "a failed refresh reported the identity as announced");
  check(warned.length === 1, `a failed refresh warned ${warned.length} times, want once`);
  check(
    warned[0]?.includes("refresh failed"),
    `the warning does not say what failed: ${JSON.stringify(warned[0])}`,
  );

  // The whole post-turn path, driven the way the hook drives it, with a bridge
  // that refuses: the failure must not throw, must announce nothing, and must warn
  // at most once per process — the residual gap the independent review named.
  const failureWarnings: string[] = [];
  const warnFailure = warnOnce((message: string) => failureWarnings.push(message));
  const failedTurn = await afterTurnRefresh(
    () => materialized,
    loaded,
    async () => {
      throw new Error("the bridge is unavailable");
    },
    warnFailure,
  );
  check(failedTurn === null, "a failed turn reported an identity as announced");
  check(failureWarnings.length === 1, `a failed turn warned ${failureWarnings.length} times, want once`);
  check(
    failureWarnings[0]?.includes("refresh failed"),
    `the post-turn warning does not say what failed: ${JSON.stringify(failureWarnings[0])}`,
  );
  // A second failing turn stays quiet: the user heard about it once.
  await afterTurnRefresh(
    () => materialized,
    loaded,
    async () => {
      throw new Error("still unavailable");
    },
    warnFailure,
  );
  check(failureWarnings.length === 1, "the once-per-process warning repeated on a later turn");

  // The same function on the happy path: exactly one send, carrying the refreshed
  // identity, and no warning.
  const refreshedSends: SessionIdentity[] = [];
  const refreshedTurn = await afterTurnRefresh(
    () => materialized,
    loaded,
    async (next) => {
      refreshedSends.push(next);
    },
    warnFailure,
  );
  check(
    refreshedSends.length === 1 && refreshedTurn?.onDisk === true,
    `the post-turn refresh sent ${refreshedSends.length} calls for ${JSON.stringify(refreshedTurn)}`,
  );
  check(failureWarnings.length === 1, "a successful refresh warned");

  // A session the runtime has not materialized yet is still announced once, so a
  // build that keeps reporting onDisk false cannot spam the bridge either.
  const unmaterializedSends: SessionIdentity[] = [];
  await afterTurnRefresh(
    () => loaded,
    loaded,
    async (next) => {
      unmaterializedSends.push(next);
    },
    warnFailure,
  );
  check(unmaterializedSends.length === 0, "an unchanged identity was re-announced");

  // The hook itself, invoked as OMP invokes it, must be safe: this process is not
  // bound in the check (the bridge cannot run), so it does nothing at all rather
  // than throwing into a completed turn.
  const hook = handlers.get("agent_end");
  if (!hook) {
    failures.push("the agent_end hook was not registered");
  } else {
    let prompts = 0;
    const notices: string[] = [];
    const ctx = {
      hasUI: true,
      ui: {
        confirm: async () => {
          prompts++;
          return false;
        },
        notify: (message: string) => notices.push(message),
      },
      sessionManager: {
        getSessionId: () => "sess-1",
        getSessionFile: () => "C:\s.jsonl",
        isSessionOnDisk: () => true,
      },
    };
    try {
      await hook({ type: "agent_end", messages: [] }, ctx);
    } catch (error) {
      failures.push(`the agent_end hook threw into a completed turn: ${String(error)}`);
    }
    check(prompts === 0, "the agent_end hook asked the user something at the end of a turn");
  }
}

await checkSessionRefresh();

if (failures.length > 0) {
  for (const failure of failures) console.error(`load check FAILED: ${failure}`);
  process.exit(1);
}
console.log(
  `load check ok: ${names.length} tools registered inactive (${names.join(", ")}); ` +
    `events ${events.join(", ")}; ${setActiveCalls.length} activation calls outside a session; ` +
    `schemas built by ${builderSpecifier}`,
);
