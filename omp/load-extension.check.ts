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
import extension from "./herdr-orchestrator.ts";

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
  on: (event: string) => {
    events.push(event);
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
check(
  names.join(",") === "orch_dispatch,orch_report",
  `registered tools are ${JSON.stringify(names)}, want orch_dispatch and orch_report`,
);
check(events.includes("session_start"), "the extension does not listen to session_start");
check(events.includes("before_agent_start"), "the extension does not listen to before_agent_start");

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
    risk: "",
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
    !tryParse(schema, { kind: "result", summary: "done", branch: "orch/abc", evidence: ["commit abc"], risk: "" }).ok,
    "orch_report accepted a final report without checks",
  );
  check(
    !tryParse(schema, { ...valid, kind: "partial" }).ok,
    "orch_report accepted kind partial; only kind result is final",
  );
}

if (failures.length > 0) {
  for (const failure of failures) console.error(`load check FAILED: ${failure}`);
  process.exit(1);
}
console.log(
  `load check ok: ${names.length} tools registered inactive (${names.join(", ")}); ` +
    `events ${events.join(", ")}; ${setActiveCalls.length} activation calls outside a session; ` +
    `schemas built by ${builderSpecifier}`,
);
