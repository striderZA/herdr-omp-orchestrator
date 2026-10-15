# Herdr OMP Orchestrator

A Windows-only Herdr plugin that connects Oh My Pi (OMP) primary and worker agents to a durable, human-supervised task orchestrator.

## Start here

- [Product intent and contract](docs/intent.md) — goal, scope, ownership, safety invariants, and the requirement-to-test map.
- [Repository and development rules](AGENTS.md) — architecture, conventions, and commands.
- [Herdr compatibility facts](docs/herdr-facts.md) — the external Herdr behavior this plugin depends on and how it was verified.
- [Disposition contract (ADR 0001)](docs/adr/0001-disposition-contract.md) — how task decisions close tasks and release dependents.
- [Successors and re-points (ADR 0005)](docs/adr/0005-successors-and-repoint.md) — how a superseded decision names its successor and how queued dependents are re-pointed to it.
- [Forgetting a project (ADR 0006)](docs/adr/0006-forget-a-project.md) — how to remove one project's plugin records without removing its repository, worktrees, branches, or Herdr panes.

## Install

This plugin runs on Windows only. Before you install it, make sure these are available in a new terminal:

- `herdr`: Herdr 0.9.0 or newer.
- `go`: Go 1.27.1, the version `go.mod` names, to build the plugin executable.
- Oh My Pi (OMP), which the plugin extends.

From the repository root, run:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install.ps1
```

The script builds `herdr-omp-orchestrator.exe`, checks Herdr's OMP integration (it runs `herdr integration install omp` only when Herdr reports it missing), links this folder as a Herdr plugin, and runs the plugin's `setup` action. It stops at the first failure and prints what to fix. Running it again is safe.

The script never unlinks or uninstalls the plugin. Its writes go through Herdr's own commands and the plugin's `setup` action: the built executable, the plugin link, Herdr's OMP integration only when Herdr reports it missing, and the OMP extension file. If Herdr already links the plugin from another folder, it stops before changing anything. If that folder is gone or this checkout has moved, run `herdr plugin unlink user.omp-orchestrator`, then run the script again.

Then follow [First use](#first-use) to bind a repository and open its task board.

## First use

After the install script finishes, bind a repository and open its board. Herdr runs plugin actions without a terminal, so `bind` and `setup` write their answer, including a refusal, to Herdr's plugin log. List it with `herdr plugin log list --plugin user.omp-orchestrator`.

The plugin's actions have these titles in Herdr: **Bind this repository as an orchestrated project** (`bind`), **Open the task board** (`board`), **Reattach the primary OMP session** (`reattach`) and **Setup OMP orchestrator extension** (`setup`).

### 1. Bind a repository

1. Open a pane in the repository and `cd` to its root. Bind refuses a pane whose working directory is not the repository root, including a subfolder.
2. Run **Bind this repository as an orchestrated project**.

Bind starts the primary, an OMP session that coordinates the work. If the pane you ran it from is a plain shell that no record claims, the primary runs in that pane. If the pane already runs an agent, such as your own OMP session, bind leaves that session alone and starts the primary in a new workspace on the same checkout. Bind sends the primary no message, so it waits for your first request. Running bind again on the same repository reopens the recorded primary and never starts a second one. If that primary's pane is gone, bind refuses and names the Reattach action, because it never substitutes a new primary.

### 2. Ask the primary for work

Type requests into the primary's pane. It can read and search the repository and use the plugin's tools, but it cannot edit files or run commands, so workers make the changes. Each dispatch you ask for needs a base commit. Before the first dispatch the plugin gives the primary none, and the primary cannot run git, so pass one in your request, for example from:

```
git rev-parse --short HEAD
```

For example: "Make two independent changes, X and Y, each by its own worker. Branch both from abc1234."

A ticket's planner takes the current HEAD by itself, so it needs no commit from you.

Then:

- A dispatch that is not for a ticket shows an OMP confirmation with the exact request. Answer it in the primary's pane. A declined confirmation starts nothing.
- Each worker gets its own Git worktree on its own `orch/` branch. Three workers run at once by default; the rest queue.
- A worker's question makes its task **Needs input**. The primary asks you, and sends your answer only after you confirm its exact text.
- Worker reports and questions reach the primary as inbox notes when it is idle. Delivered notes say they were recorded by workers and the orchestrator, not by you.
- When a worker finishes, its task is **Ready for review**. The report alone never completes it. The primary asks you to record an outcome: `integrated` and `kept` accept the work and release anything waiting on it; `rejected`, `superseded` and `abandoned` do not.
- The plugin never merges or pushes, so any merge is yours. To remove a reviewed worker's checkout, ask the primary; it asks you to confirm, keeps the branch and refuses a live or dirty checkout.

### 3. Use the board

Run **Open the task board**. It opens as an overlay in the project's workspace.

- **Backlog** and **Todo** show tickets that have no implementation work and no undecided planner. Once a ticket has either, the board shows it through its tasks instead. **Queued**, **Running**, **Needs input**, **Ready for review** and **Done** hold tasks.
- Arrow keys or `j`/`k` move, and Tab or Shift+Tab switches columns. `Enter` toggles details, and `Esc` closes them, or closes the board when none are open. `/` filters, `f` focuses the selected worker's pane, `r` refreshes and `q` closes the board.
- `n` creates a ticket from a title you type; Enter saves it and Esc cancels. Creating a ticket starts nothing.
- `m` moves the selected backlog ticket to **Todo** and records its planner, which starts or queues when all worker slots are busy. From then on the board shows the ticket through that planner's task. The keypress is the only confirmation: the board shows no dialog for `m`. It needs a commit at HEAD.

### 4. Plan a ticket before coding (optional)

1. Press `n` on the board and title the ticket.
2. Select it in **Backlog** and press `m`. Its planner works in its own worktree and reports a plan. It edits nothing.
3. When the plan is reported, the primary asks you to accept it. Accepting it (outcome `integrated` or `kept`) lets the primary dispatch implementation for the ticket, which still needs a base commit as above. Nothing starts from the plan by itself.
4. When the ticket's tasks are decided, the primary asks you to close the ticket with an outcome. A ticket cannot close while a linked task is still open.

### 5. When something goes wrong

- Restarting Herdr keeps the recorded task facts and board state. At startup the plugin reconciles its records with Herdr's live state.
- If the board shows **Primary not verified**, the recorded primary is absent or cannot be verified; the text after the label gives the reason. Run **Reattach the primary OMP session** from the pane that runs the replacement OMP, at the repository root. It shows what it would record and asks you to confirm first.

### 6. Forget a project

Use **Forget this project's orchestrator records** when you want to remove this plugin's records for a repository so you can bind it again as a new project. Start from a pane in that repository and open the action. The plan is read-only; review its counts and what will stay before confirming. The overlay is titled **Forget the project**.

For an undecided task, record its outcome through the primary. If the primary is gone, the forget refusal names **Reattach the primary OMP session** as the first step. If a task is still active, wait until its work stops, then record its outcome through the primary. A task with an active claim, a `sending` or `uncertain` note, a bind in progress, or an uncertain reattach notice also blocks forgetting. `sending` and `uncertain` notes block the action; the plugin has no action that clears an `uncertain` note yet. For a bind intent, run bind again. For an uncertain reattach notice, run **Reattach the primary OMP session**.

After a confirmed forget, the plugin deletes the `ready_sessions` rows for the panes currently recorded by the project, and the project's `inbox`, `tickets`, `tasks`, and `projects` rows. It does not delete `task_history`; those rows remain as the audit trail for the forgotten work, including the project key. Pending notes are deleted with the project. Forgetting cannot be undone.
The repository, its branches and worktrees, and the Herdr workspaces and panes stay. Forgetting does not remove checkouts or close sessions.
To confirm, type the project's folder name exactly as the prompt shows it. Pressing Enter alone cancels and deletes nothing.

The primary keeps its read-only restriction until its OMP session restarts. Restart that session or close its pane to clear the restriction.

Common refusal codes. Bind and setup write theirs to the plugin log; a dispatch refusal reaches the primary through the bridge, and the board shows a ticket refusal as a notice. Forget codes appear in the forget pane's output and on its stderr envelope:

| Code | Meaning and fix |
|---|---|
| `integration_missing` | Herdr does not report its OMP integration as installed. Run `herdr integration install omp`, or run `install.ps1` again. |
| `extension_missing` | The OMP extension is not in the active profile. Run **Setup OMP orchestrator extension**, or run `install.ps1` again. |
| `not_git_repository` | The folder is not inside a Git work tree. Bind from a folder inside one. |
| `different_repository` | The pane's working directory is not the repository root. `cd` to the root in that pane and bind again. |
| `bad_base_revision` | The base is not a commit of this repository. For a dispatch, give the primary a hash of at least seven hex characters from `git rev-parse --short HEAD`; `HEAD` and branch names such as `main` are refused. A ticket move also needs a commit at HEAD, so a repository with no commits needs a first commit. |
| `forget_refused` | The plan has a blocking reason. Record undecided task outcomes through the primary (reattach it first if it is gone); if a task is active, wait for its work to stop, then record its outcome. An active claim must finish. A bind intent may clear by running bind again; an uncertain reattach notice may clear by running **Reattach the primary OMP session**. `sending` and `uncertain` notes block forgetting, and the plugin has no action that clears an `uncertain` note yet. |
| `forget_plan_changed` | The project changed after planning. Open the forget action again, review the new plan, and confirm it again. |
| `forget_not_confirmed` | The pane returns this code when the confirmation plan has no fingerprint. Typing a different folder name or pressing Enter cancels with “Cancelled. Nothing was changed.” and returns `ok: true, forgotten: false`. |

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
