# Herdr OMP Orchestrator

A Windows-only Herdr plugin that connects Oh My Pi (OMP) primary and worker agents to a durable, human-supervised task orchestrator.

## Start here

- [Product intent and contract](docs/intent.md) — goal, scope, ownership, safety invariants, and the requirement-to-test map.
- [Repository and development rules](AGENTS.md) — architecture, conventions, and commands.
- [Herdr compatibility facts](docs/herdr-facts.md) — the external Herdr behavior this plugin depends on and how it was verified.
- [Disposition contract (ADR 0001)](docs/adr/0001-disposition-contract.md) — how task decisions close tasks and release dependents.

## Development

The project uses Go 1.27.1. From the repository root:

```sh
go test ./...
go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator
```

On Windows, the manifest build uses `CGO_ENABLED=0`. The PowerShell helper and full acceptance commands, plus the optional OMP extension load check, are documented in [AGENTS.md](AGENTS.md). The full Windows acceptance suite needs real Herdr and OMP sessions; it creates an isolated named session and temporary worktree root.
