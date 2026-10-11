# Herdr OMP Orchestrator

A Windows-only Herdr plugin that connects Oh My Pi (OMP) primary and worker agents to a durable, human-supervised task orchestrator.

## Start here

- [Product intent and contract](docs/intent.md) — goal, scope, ownership, safety invariants, and the requirement-to-test map.
- [Repository and development rules](AGENTS.md) — architecture, conventions, and commands.
- [Herdr compatibility facts](docs/herdr-facts.md) — the external Herdr behavior this plugin depends on and how it was verified.
- [Disposition contract (ADR 0001)](docs/adr/0001-disposition-contract.md) — how task decisions close tasks and release dependents.
- [Successors and re-points (ADR 0005)](docs/adr/0005-successors-and-repoint.md) — how a superseded decision names its successor and how queued dependents are re-pointed to it.

## What setup writes

Setup (the plugin's `setup` action, or `herdr-omp-orchestrator.exe setup` run from the plugin directory) writes one file: `herdr-orchestrator.ts`, the plugin's OMP extension. It goes in OMP's effective extensions directory for the active profile:

- **Default profile** (`default`, an empty value, or no profile selected): `$HOME/.omp/agent/extensions/herdr-orchestrator.ts`. `$HOME` falls back to `%USERPROFILE%` when it is unset or blank. `PI_CONFIG_DIR` replaces `.omp`, and `PI_CODING_AGENT_DIR` replaces `.omp/agent` with that directory.
- **Named profile** (from `OMP_PROFILE`, or from `PI_PROFILE` when `OMP_PROFILE` is not defined): `$HOME/.omp/profiles/<name>/agent/extensions/herdr-orchestrator.ts`, with `PI_CONFIG_DIR` replacing `.omp`. `PI_CODING_AGENT_DIR` does not apply to a named profile.

Before it writes, setup runs `herdr integration status` and refuses unless Herdr reports its OMP integration installed, and Herdr's own `herdr-omp-agent-state.ts` is present in that same directory. The file it writes:

- embeds three absolute paths: the plugin executable, `HERDR_PLUGIN_STATE_DIR` and the Herdr CLI (`HERDR_BIN_PATH`). If the plugin folder moves, run setup again to refresh them.
- is written atomically (a temporary file beside it, then a rename). A byte-identical file is left alone, so a repeated run changes nothing.
- never replaces an existing `herdr-orchestrator.ts` that does not begin with the plugin's managed marker line. Setup refuses with `extension_conflict` instead.

Setup never writes Herdr's own `herdr-omp-agent-state.ts`, never edits OMP configuration, and does not create the plugin state directory.

To preview a setup without writing, run `herdr-omp-orchestrator.exe setup --dry-run`. It prints a JSON answer shaped like setup's, with `"dryRun": true` and the `action` setup would take (`create`, `refresh` or `unchanged`), or the refusal setup would give, and it writes nothing. The preview needs the environment the setup action gets: `HERDR_PLUGIN_STATE_DIR` and `HERDR_BIN_PATH` set, and `HERDR_PLUGIN_ROOT` naming a directory that contains `omp/herdr-orchestrator.ts` (or, when `HERDR_PLUGIN_ROOT` is unset, a working directory that does). Setup accepts no other arguments: any other argument fails with the `usage` code and exit status 2.

To remove the extension by hand, delete `herdr-orchestrator.ts` from your profile's extensions directory. Leave `herdr-omp-agent-state.ts` in place, because it belongs to Herdr. Until setup installs the extension again, `bind` refuses with `extension_missing`. The plugin has no uninstall action.

## Development

The project uses Go 1.27.1. From the repository root:

```sh
go test ./...
go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator
```

On Windows, the manifest build uses `CGO_ENABLED=0`. The PowerShell helper and full acceptance commands, plus the optional OMP extension load check, are documented in [AGENTS.md](AGENTS.md). The full Windows acceptance suite needs real Herdr and OMP sessions; it creates an isolated named session and temporary worktree root.
