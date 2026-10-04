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
  adjudicateConfirmRequest,
  afterTurnRefresh,
  cleanupConfirmRequest,
  confirmThen,
  dialogConfirmRequest,
  refreshIdentity,
  readReportPreview,
  readTicketPreview,
  recordReviewWithConsent,
  recordTicketMoveWithConsent,
  recordTicketReviewWithConsent,
  refreshReady,
  settleConfirmRequest,
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
  "orch_adjudicate_dispatch",
  "orch_answer",
  "orch_approve_dialog",
  "orch_cleanup",
  "orch_dispatch",
  "orch_inbox_list",
  "orch_report",
  "orch_review",
  "orch_settle_input",
  "orch_task_list",
  "orch_ticket_create",
  "orch_ticket_list",
  "orch_ticket_move",
  "orch_ticket_review",
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
  // A ticket id is optional: absent it, the parsed call carries none; given, it
  // survives parsing; an empty one is refused.
  const linked = tryParse(schema, { ...payload, ticketId: "tkt_abc" });
  check(linked.ok, `orch_dispatch rejected a call naming a ticket: ${linked.ok ? "" : linked.error}`);
  check(
    linked.ok && typeof linked.value === "object" && linked.value !== null && "ticketId" in linked.value &&
      linked.value.ticketId === "tkt_abc",
    "orch_dispatch dropped the ticketId it was given",
  );
  check(
    first.ok && typeof first.value === "object" && first.value !== null &&
      (!("ticketId" in first.value) || first.value.ticketId === undefined),
    "orch_dispatch invented a ticketId for a call that named none",
  );
  check(!tryParse(schema, { ...payload, ticketId: "" }).ok, "orch_dispatch accepted an empty ticketId");
  // A repeated request is answered with the existing task and `duplicate:true`
  // (issue #4). The bridge hands that answer to the model verbatim, so this
  // description is the only place the model learns what the flag means and what
  // to do about it.
  check(
    dispatch.description.includes("duplicate:true") && dispatch.description.includes("already holds"),
    "orch_dispatch no longer tells the model that a repeat returns the existing task with duplicate:true",
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
  // One schema carries all three kinds, so each kind's own required fields must
  // still bite: a union that accepted `progress` without a summary would let a
  // worker send a note the coordinator cannot record.
  check(
    tryParse(schema, { kind: "progress", summary: "ported the lexer" }).ok,
    "orch_report rejected a valid progress note",
  );
  check(
    tryParse(schema, { kind: "question", text: "Which branch should I target?" }).ok,
    "orch_report rejected a valid question",
  );
  check(
    !tryParse(schema, { kind: "progress" }).ok,
    "orch_report accepted progress without a summary",
  );
  check(
    !tryParse(schema, { kind: "question", summary: "wrong field for this kind" }).ok,
    "orch_report accepted a question carrying a progress field instead of its text",
  );
  check(
    !tryParse(schema, { kind: "partial", summary: "halfway" }).ok,
    "orch_report accepted an unknown kind",
  );
}

// --- the rest of the typed tool parameters ---------------------------------

// Every tool the two roles use has to build under OMP's builder and accept the
// call the model is told to make; a missing or too-loose schema is a load
// failure waiting for the user's machine.
const toolCalls: { name: string; params: Record<string, unknown>; bad: Record<string, unknown> | null }[] = [
  // orch_report is the one worker tool; each kind is a call the model is told to
  // make, and the bad case for a kind is that kind with its own field missing.
  { name: "orch_report", params: { kind: "progress", summary: "ported the lexer" }, bad: { kind: "progress", summary: "" } },
  {
    name: "orch_report",
    params: { kind: "question", text: "Which branch should I target?" },
    bad: { kind: "question", text: "" },
  },
  { name: "orch_answer", params: { taskId: "orch_abc", text: "Keep the old API." }, bad: { taskId: "", text: "" } },
  {
    name: "orch_approve_dialog",
    params: { taskId: "orch_abc", keys: ["1"] },
    bad: { taskId: "orch_abc", keys: [] },
  },
  { name: "orch_review", params: { taskId: "orch_abc", outcome: "integrated", disposition: "accepted" }, bad: { taskId: "orch_abc", disposition: "accepted" } },
  {
    name: "orch_settle_input",
    params: { taskId: "orch_abc", decision: "not_delivered" },
    bad: { taskId: "", decision: "not_delivered" },
  },
  {
    name: "orch_adjudicate_dispatch",
    params: { taskId: "orch_abc", decision: "delivered" },
    bad: { taskId: "orch_abc", decision: "maybe" },
  },
  { name: "orch_cleanup", params: { taskId: "orch_abc" }, bad: {} },
  // The ticket tools: creating and listing are inert facts, moving and closing a
  // ticket are user decisions. `orch_ticket_list` takes no arguments, so it has
  // no empty call to refuse.
  { name: "orch_ticket_create", params: { title: "Add search" }, bad: { title: "" } },
  { name: "orch_ticket_list", params: {}, bad: null },
  { name: "orch_ticket_move", params: { ticketId: "tkt_abc" }, bad: { ticketId: "" } },
  {
    name: "orch_ticket_review",
    params: { ticketId: "tkt_abc", outcome: "abandoned", disposition: "not needed" },
    bad: { ticketId: "tkt_abc", outcome: "merged" },
  },
  { name: "orch_ticket_review", params: { ticketId: "tkt_abc", outcome: "kept" }, bad: { ticketId: "", outcome: "kept" } },
  // The inspection tools take only optional filters, so the empty call is valid;
  // the bad case is a filter value the bridge would refuse.
  { name: "orch_task_list", params: {}, bad: { stage: "archived" } },
  { name: "orch_task_list", params: { stage: "needs_input", limit: 20 }, bad: { limit: 0 } },
  { name: "orch_inbox_list", params: {}, bad: { state: "lost" } },
  { name: "orch_inbox_list", params: { state: "uncertain", limit: 20 }, bad: { limit: 0 } },
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
    call.bad === null || !tryParse(schema, call.bad).ok,
    `${call.name} accepted an empty ${JSON.stringify(call.bad)}`,
  );
}
// Bounds and defaults the ticket schemas promise.
const ticketCreate = tools.find((tool) => tool.name === "orch_ticket_create");
if (ticketCreate) {
  const schema = requireSchema(ticketCreate);
  const minimal = tryParse(schema, { title: "x" });
  check(
    minimal.ok && typeof minimal.value === "object" && minimal.value !== null && "body" in minimal.value && minimal.value.body === "",
    "orch_ticket_create body does not default to an empty string",
  );
  check(tryParse(schema, { title: "t".repeat(160), body: "b".repeat(16000) }).ok, "orch_ticket_create refused the largest valid call");
  check(!tryParse(schema, { title: "t".repeat(161) }).ok, "orch_ticket_create accepted a title over 160 characters");
  check(!tryParse(schema, { title: "x", body: "b".repeat(16001) }).ok, "orch_ticket_create accepted a body over 16000 characters");
}
const ticketReview = tools.find((tool) => tool.name === "orch_ticket_review");
if (ticketReview) {
  const schema = requireSchema(ticketReview);
  const minimal = tryParse(schema, { ticketId: "tkt_abc", outcome: "kept" });
  check(
    minimal.ok && typeof minimal.value === "object" && minimal.value !== null && "disposition" in minimal.value &&
      minimal.value.disposition === "",
    "orch_ticket_review disposition does not default to an empty string",
  );
  check(!tryParse(schema, { ticketId: "tkt_abc", outcome: "kept", disposition: "n".repeat(201) }).ok, "orch_ticket_review accepted a note over 200 characters");
}
// Bounds the inspection schemas promise: an optional filter, and a row limit that
// is a whole number between 1 and the 200 the coordinator accepts.
for (const name of ["orch_task_list", "orch_inbox_list"]) {
  const inspect = tools.find((tool) => tool.name === name);
  if (!inspect) continue;
  const schema = requireSchema(inspect);
  check(tryParse(schema, { limit: 1 }).ok && tryParse(schema, { limit: 200 }).ok, `${name} refused a limit inside 1..200`);
  check(!tryParse(schema, { limit: 201 }).ok, `${name} accepted a limit over 200`);
  check(!tryParse(schema, { limit: -1 }).ok, `${name} accepted a negative limit`);
  check(!tryParse(schema, { limit: 2.5 }).ok, `${name} accepted a fractional limit`);
}
// A review is a user decision, not a model decision. Refusing the UI or
// declining the exact task/disposition must stop before the bridge is spawned.
async function checkReviewConsent(): Promise<void> {
  const review = tools.find((tool) => tool.name === "orch_review") as
    | (RegisteredTool & {
        execute: (
          callId: string,
          params: { taskId: string; disposition: string },
          signal: AbortSignal,
          update: undefined,
          ctx: unknown,
        ) => Promise<{ isError?: boolean; content: Array<{ text: string }> }>;
      })
    | undefined;
  if (!review) {
    failures.push("orch_review was not registered");
    return;
  }
  const identity = {
    getSessionId: () => "sess-1",
    getSessionFile: () => "C:/session.jsonl",
    isSessionOnDisk: () => true,
  };
  const params = { taskId: "orch_abc", disposition: "Accepted the reported result." };
  let prompts = 0;
  try {
    const noUI = await review.execute("review-no-ui", params, new AbortController().signal, undefined, {
      hasUI: false,
      ui: { confirm: async () => { prompts++; return true; } },
      sessionManager: identity,
    });
    check(noUI.isError === true, "orch_review proceeded without an interactive UI");
    check(prompts === 0, "orch_review asked for confirmation without an interactive UI");
  } catch (error) {
    failures.push(`orch_review reached the bridge without a UI: ${String(error)}`);
  }
  const preview = {
    taskId: params.taskId,
    revision: 7,
    stage: "ready_for_review",
    summary: "Added subtract to a.js",
    branch: "orch/abc",
    evidence: ["commit 0123456789abcdef"],
    checks: ["go test ./internal/coord ok"],
    risk: "No known risk",
  };
  check(readReportPreview(preview)?.revision === 7, "valid stored report preview was refused");
  check(readReportPreview({ ...preview, evidence: null }) === null, "preview with missing evidence was trusted");
  check(readReportPreview({ ...preview, revision: 0 }) === null, "preview with no revision was trusted");
  let request = { title: "", message: "" };
  const sent: Array<{ taskId: string; outcome: string; disposition: string; expectedRevision: number }> = [];
  const send = async (confirmed: { taskId: string; outcome: string; disposition: string; expectedRevision: number }) => {
    sent.push(confirmed);
    return { stage: "Done" };
  };
  const declined = await recordReviewWithConsent(
    { hasUI: true, confirm: async (title, message) => { request = { title, message }; return false; } },
    preview,
    "rejected",
    params.disposition,
    send,
  );
  check(!declined.ok && sent.length === 0, "declined report review reached the bridge mutation");
  for (const fact of [preview.taskId, "revision 7", preview.summary, preview.branch, "rejected", "stay queued",
    preview.evidence[0], preview.checks[0], preview.risk, params.disposition]) {
    check(request.message.includes(fact), `review confirmation did not show ${JSON.stringify(fact)}`);
  }
  // The acceptance harness (tests/acceptance_windows.ps1, Test-PrimaryReviewConfirmScreen)
  // waits for exactly this shape: the outcome first, the note on its own line, then
  // the stored report, ending with the sentence the outcome implies. A reworded
  // dialog must fail here, not in a live run; change the matcher and its helper
  // fixture together with this text.
  check(request.title === "Record review disposition", `the review dialog title changed: ${JSON.stringify(request.title)}`);
  check(
    request.message === [
      `Confirm that you reviewed the worker's final report for task ${preview.taskId} and chose this outcome: rejected`,
      `Note: ${params.disposition}`,
      "Stored report (revision 7):",
      `Summary: ${JSON.stringify(preview.summary)}`,
      `Branch: ${JSON.stringify(preview.branch)}`,
      `Evidence: ${JSON.stringify(preview.evidence)}`,
      `Checks: ${JSON.stringify(preview.checks)}`,
      `Risk: ${JSON.stringify(preview.risk)}`,
      "This closes the task and frees its slot and paths, but tasks that depend on it stay queued. Decline if you have not reviewed the report.",
    ].join("\n"),
    `the non-accepting review dialog changed shape:\n${request.message}`,
  );
  let approvedRequest = { title: "", message: "" };
  const approved = await recordReviewWithConsent(
    { hasUI: true, confirm: async (title, message) => { approvedRequest = { title, message }; return true; } },
    preview,
    "integrated",
    params.disposition,
    send,
  );
  check(approved.ok && approved.value !== null, "an approved review did not return its bridge result");
  check(
    sent.length === 1 && sent[0]?.taskId === params.taskId && sent[0]?.outcome === "integrated" &&
      sent[0]?.disposition === params.disposition && sent[0]?.expectedRevision === preview.revision,
    `the approved review sent ${JSON.stringify(sent)}, want the exact preview revision and decision`,
  );
  check(
    approvedRequest.message.startsWith(
      `Confirm that you reviewed the worker's final report for task ${preview.taskId} and chose this outcome: integrated\nNote: ${params.disposition}\nStored report (revision 7):\n`,
    ) && approvedRequest.message.endsWith(
      "This closes the task and may start queued dependent work. Decline if you have not reviewed the report.",
    ),
    `the accepting review dialog changed shape:\n${approvedRequest.message}`,
  );
}

await checkReviewConsent();

// Moving or closing a ticket is a user decision. The helpers refuse without a UI,
// stop when the user declines, show the exact facts being confirmed, and send the
// digest of precisely the ticket that was previewed.
async function checkTicketConsent(): Promise<void> {
  const preview = {
    ticketId: "tkt_abc123",
    title: "Add search",
    column: "backlog",
    stage: "backlog",
    revision: 4,
    digest: "d".repeat(64),
    taskIds: ["orch_a1"],
  };
  check(readTicketPreview(preview)?.digest === preview.digest, "a valid ticket preview was refused");
  check(readTicketPreview({ ...preview, taskIds: [] })?.taskIds.length === 0, "a ticket preview with no linked tasks was refused");
  const untrusted: Record<string, unknown> = {
    "no digest": { ...preview, digest: "" },
    "no revision": { ...preview, revision: 0 },
    "no id": { ...preview, ticketId: "" },
    "no title": { ...preview, title: "" },
    "an unknown column": { ...preview, column: "archive" },
    "an unknown stage": { ...preview, stage: "lost" },
    "task ids that are not strings": { ...preview, taskIds: [1] },
    "no task ids": { ...preview, taskIds: undefined },
    "a non-object body": "tkt_abc123",
    "a null body": null,
  };
  for (const [name, value] of Object.entries(untrusted)) {
    check(readTicketPreview(value) === null, `a ticket preview with ${name} was trusted`);
  }

  const moves: Array<{ ticketId: string; digest: string }> = [];
  const sendMove = async (confirmed: { ticketId: string; digest: string }) => {
    moves.push(confirmed);
    return { column: "todo" };
  };
  const trustedPreview = readTicketPreview(preview);
  if (trustedPreview === null) return;

  let request = { title: "", message: "" };
  const declinedMove = await recordTicketMoveWithConsent(
    { hasUI: true, confirm: async (title, message) => { request = { title, message }; return false; } },
    trustedPreview,
    sendMove,
  );
  check(!declinedMove.ok && moves.length === 0, "a declined ticket move reached the bridge mutation");
  for (const fact of [preview.ticketId, preview.title, "backlog", "todo", "revision 4", "starts nothing"]) {
    check(request.message.includes(fact), `the move confirmation did not show ${JSON.stringify(fact)}`);
  }
  // Test-PrimaryTicketConfirmScreen in the acceptance harness waits for exactly
  // these sentences; change both together.
  check(request.title === "Move ticket to todo", `the move dialog title changed: ${JSON.stringify(request.title)}`);
  check(
    request.message === [
      `Confirm moving ticket ${preview.ticketId} to todo.`,
      `Title: ${JSON.stringify(preview.title)}`,
      `Now: ${preview.column} (stage ${preview.stage}), revision ${preview.revision}`,
      "In this version a move only records the ticket as ready for work; it starts nothing. No task, worktree or agent is created, and dispatching work for the ticket is a separate step.",
    ].join("\n"),
    `the ticket move dialog changed shape:\n${request.message}`,
  );
  let prompts = 0;
  const noUiMove = await recordTicketMoveWithConsent(
    { hasUI: false, confirm: async () => { prompts++; return true; } },
    trustedPreview,
    sendMove,
  );
  check(!noUiMove.ok && prompts === 0 && moves.length === 0, "a ticket move proceeded or prompted without an interactive UI");
  const approvedMove = await recordTicketMoveWithConsent({ hasUI: true, confirm: async () => true }, trustedPreview, sendMove);
  check(approvedMove.ok, "an approved ticket move did not return its bridge result");
  check(
    moves.length === 1 && moves[0]?.ticketId === preview.ticketId && moves[0]?.digest === preview.digest,
    `the approved move sent ${JSON.stringify(moves)}, want the previewed ticket id and digest`,
  );
  // A move that could not be honored is refused before the user is asked.
  let asked = 0;
  for (const [name, impossible] of Object.entries({
    "already in todo": { ...preview, column: "todo", stage: "todo" },
    "already decided": { ...preview, stage: "done" },
  })) {
    const trusted = readTicketPreview(impossible);
    if (trusted === null) {
      failures.push(`the ${name} preview was not readable`);
      continue;
    }
    const refused = await recordTicketMoveWithConsent(
      { hasUI: true, confirm: async () => { asked++; return true; } },
      trusted,
      sendMove,
    );
    check(!refused.ok && asked === 0 && moves.length === 1, `a ticket ${name} was offered to the user or sent`);
  }

  const reviews: Array<{ ticketId: string; digest: string; outcome: string; disposition: string }> = [];
  const sendReview = async (confirmed: { ticketId: string; digest: string; outcome: string; disposition: string }) => {
    reviews.push(confirmed);
    return { stage: "done" };
  };
  const note = "Superseded by tkt_def456.";
  request = { title: "", message: "" };
  const declinedReview = await recordTicketReviewWithConsent(
    { hasUI: true, confirm: async (title, message) => { request = { title, message }; return false; } },
    trustedPreview,
    "superseded",
    note,
    sendReview,
  );
  check(!declinedReview.ok && reviews.length === 0, "a declined ticket review reached the bridge mutation");
  for (const fact of [preview.ticketId, preview.title, "superseded", note, "revision 4", "orch_a1", "cannot be closed while"]) {
    check(request.message.includes(fact), `the ticket review confirmation did not show ${JSON.stringify(fact)}`);
  }
  check(request.title === "Record ticket decision", `the ticket decision dialog title changed: ${JSON.stringify(request.title)}`);
  check(
    request.message === [
      `Confirm closing ticket ${preview.ticketId} with this outcome: superseded`,
      `Title: ${JSON.stringify(preview.title)}`,
      `Note: ${note}`,
      `Ticket: revision ${preview.revision}, stage ${preview.stage}; linked tasks: ${preview.taskIds.join(", ")}`,
      "This records your decision on the ticket only: it changes no task and merges nothing. A ticket cannot be closed while any of its linked tasks is still open.",
    ].join("\n"),
    `the ticket decision dialog changed shape:\n${request.message}`,
  );
  prompts = 0;
  const noUiReview = await recordTicketReviewWithConsent(
    { hasUI: false, confirm: async () => { prompts++; return true; } },
    trustedPreview,
    "kept",
    note,
    sendReview,
  );
  check(!noUiReview.ok && prompts === 0 && reviews.length === 0, "a ticket review proceeded or prompted without an interactive UI");
  // "rejected" differs from every other outcome used above, so a helper that sent
  // a fixed or earlier outcome could not pass.
  const approvedReview = await recordTicketReviewWithConsent(
    { hasUI: true, confirm: async () => true },
    trustedPreview,
    "rejected",
    note,
    sendReview,
  );
  check(approvedReview.ok, "an approved ticket review did not return its bridge result");
  check(
    reviews.length === 1 && reviews[0]?.ticketId === preview.ticketId && reviews[0]?.digest === preview.digest &&
      reviews[0]?.outcome === "rejected" && reviews[0]?.disposition === note,
    `the approved review sent ${JSON.stringify(reviews)}, want the previewed digest and the exact decision`,
  );

  // A done preview is not refused by the helper, by design: the coordinator is the
  // authority on whether a decision is a retry of the recorded one (it answers with
  // the same view, which is what makes a lost response safe to retry) or a
  // conflicting one (already_decided). The helper asks the user and sends, with the
  // digest of the ticket as it stands. (The preview here has no outcome; the real
  // bridge's does carry one, which this helper deliberately does not act on.)
  const donePreview = readTicketPreview({ ...preview, stage: "done", revision: 5, digest: "e".repeat(64) });
  if (donePreview === null) {
    failures.push("the done ticket preview was not readable");
  } else {
    let doneAsked = 0;
    let doneRequest = { title: "", message: "" };
    const declinedRetry = await recordTicketReviewWithConsent(
      { hasUI: true, confirm: async (title, message) => { doneAsked++; doneRequest = { title, message }; return false; } },
      donePreview,
      "rejected",
      note,
      sendReview,
    );
    check(
      !declinedRetry.ok && doneAsked === 1 && reviews.length === 1,
      "a declined retry on a done ticket must ask once and send nothing",
    );
    check(doneRequest.message.includes("stage done"), "the retry confirmation did not show that the ticket is already done");
    const retry = await recordTicketReviewWithConsent(
      { hasUI: true, confirm: async () => { doneAsked++; return true; } },
      donePreview,
      "rejected",
      note,
      sendReview,
    );
    check(doneAsked === 2, "a confirmed retry on a done ticket was not put to the user");
    check(
      retry.ok && retry.value.stage === "done",
      "a confirmed retry on a done ticket did not return the coordinator's answer",
    );
    check(
      reviews.length === 2 && reviews[1]?.ticketId === preview.ticketId && reviews[1]?.digest === donePreview.digest &&
        reviews[1]?.outcome === "rejected" && reviews[1]?.disposition === note,
      `the confirmed retry sent ${JSON.stringify(reviews[1])}, want the done preview's own digest and the same decision`,
    );
    // A different decision for the same done ticket is sent too: the coordinator
    // refuses it as already_decided; the helper never decides that from a preview.
    const conflicting = await recordTicketReviewWithConsent(
      { hasUI: true, confirm: async () => true },
      donePreview,
      "kept",
      note,
      sendReview,
    );
    check(
      conflicting.ok && reviews.length === 3 && reviews[2]?.outcome === "kept",
      "a different decision on a done ticket must reach the coordinator, which refuses it, not be guessed at by the helper",
    );
  }

  // The tools themselves refuse a session with no UI before they read or send
  // anything, exactly as orch_review does.
  for (const [name, params] of [
    ["orch_ticket_move", { ticketId: "tkt_abc123" }],
    ["orch_ticket_review", { ticketId: "tkt_abc123", outcome: "kept", disposition: "" }],
  ] as const) {
    const tool = tools.find((candidate) => candidate.name === name) as
      | (RegisteredTool & {
          execute: (
            callId: string,
            params: Record<string, unknown>,
            signal: AbortSignal,
            update: undefined,
            ctx: unknown,
          ) => Promise<{ isError?: boolean; content: Array<{ text: string }> }>;
        })
      | undefined;
    if (!tool) {
      failures.push(`${name} was not registered`);
      continue;
    }
    let toolPrompts = 0;
    try {
      const outcome = await tool.execute(`${name}-no-ui`, params, new AbortController().signal, undefined, {
        hasUI: false,
        ui: { confirm: async () => { toolPrompts++; return true; } },
        sessionManager: {
          getSessionId: () => "sess-1",
          getSessionFile: () => "C:/session.jsonl",
          isSessionOnDisk: () => true,
        },
      });
      check(outcome.isError === true, `${name} proceeded without an interactive UI`);
      check(toolPrompts === 0, `${name} asked for confirmation without an interactive UI`);
    } catch (error) {
      failures.push(`${name} reached the bridge without a UI: ${String(error)}`);
    }
  }
}

await checkTicketConsent();


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

  // A settlement is the user's own report about what they saw, so it is confirmed
  // before it is recorded, and a decision the user cannot make is refused outright.
  const delivered = settleConfirmRequest("orch_abc", "delivered");
  if (!delivered.ok) {
    failures.push(`the delivered settlement was refused: ${delivered.reason}`);
  } else {
    check(
      delivered.request.title === "Settle worker input",
      `the settlement confirmation is titled ${JSON.stringify(delivered.request.title)}`,
    );
    check(
      delivered.request.message.includes("orch_abc") && delivered.request.message.includes("DID arrive"),
      `the settlement confirmation does not say what the user is confirming: ${JSON.stringify(delivered.request.message)}`,
    );
  }
  const notDelivered = settleConfirmRequest("orch_abc", "not_delivered");
  if (!notDelivered.ok) {
    failures.push(`the not-delivered settlement was refused: ${notDelivered.reason}`);
  } else {
    check(
      notDelivered.request.message.includes("did NOT arrive"),
      `the not-delivered confirmation reads ${JSON.stringify(notDelivered.request.message)}`,
    );
  }
  check(!settleConfirmRequest("orch_abc", "maybe").ok, "the settlement accepted a decision the user cannot make");
  check(!settleConfirmRequest("", "delivered").ok, "the settlement accepted no task");

  // A brief adjudication is decided on evidence, so the confirmation must show the
  // pane, the revision, the digest and the terminal text the digest covers, and it
  // must refuse a decision the coordinator would not accept.
  const inspection = {
    taskId: "orch_abc",
    title: "one",
    phase: "agent_started",
    blocker: "task orch_abc could not submit the brief: coord: the step's outcome could not be identified",
    paneId: "w2:p1",
    revision: 7,
    digest: "a1b2c3d4",
    terminal: "worker terminal\n> waiting for instructions",
  };
  const adjudicated = adjudicateConfirmRequest(inspection, "delivered");
  if (!adjudicated.ok) {
    failures.push(`the delivered adjudication was refused: ${adjudicated.reason}`);
  } else {
    check(
      adjudicated.request.title === "Decide an uncertain dispatch",
      `the adjudication confirmation is titled ${JSON.stringify(adjudicated.request.title)}`,
    );
    for (const shown of ["orch_abc", "w2:p1", "revision 7", "a1b2c3d4", "waiting for instructions", "DID reach"]) {
      check(
        adjudicated.request.message.includes(shown),
        `the adjudication confirmation does not show ${JSON.stringify(shown)}: ${JSON.stringify(adjudicated.request.message)}`,
      );
    }
  }
  const withheld = adjudicateConfirmRequest(inspection, "not_delivered");
  if (!withheld.ok) {
    failures.push(`the not-delivered adjudication was refused: ${withheld.reason}`);
  } else {
    check(
      withheld.request.message.includes("did NOT reach"),
      `the not-delivered adjudication reads ${JSON.stringify(withheld.request.message)}`,
    );
  }
  check(!adjudicateConfirmRequest(inspection, "maybe").ok, "the adjudication accepted a decision the coordinator refuses");
  check(
    !adjudicateConfirmRequest({ ...inspection, digest: "" }, "delivered").ok,
    "the adjudication accepted evidence with no digest",
  );
  check(
    !adjudicateConfirmRequest({ ...inspection, paneId: "" }, "delivered").ok,
    "the adjudication accepted evidence with no pane",
  );
  check(!adjudicateConfirmRequest({ ...inspection, taskId: "" }, "delivered").ok, "the adjudication accepted no task");

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
