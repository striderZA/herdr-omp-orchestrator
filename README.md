# Herdr OMP Orchestrator

> **Archived.** This project is superseded by the 1.0.0 release of [herdr-projects](https://github.com/eliasstravik/herdr-projects). Start with its [get-started guide](https://github.com/eliasstravik/herdr-projects/blob/main/docs/get-started.md).

Herdr OMP Orchestrator is a Herdr plugin that runs Oh My Pi (OMP) agents as a durable, human-supervised task orchestrator. It runs on Windows and on Linux (x86_64). macOS is refused, and arm64 is untested.

- A **primary** OMP session takes your requests and delegates them. It can read, search and use the plugin's tools, but it cannot edit files or run commands.
- **Workers** each carry out one task in their own Git worktree, on their own `orch/` branch.
- **Planners** write read-only plans for tickets.
- You decide what is accepted. Task progress comes from recorded reports and your decisions, never from terminal activity. The plugin never merges or pushes.

Steps that send input to an agent, record a decision or move a ticket ask you to confirm first. Starting work from a request needs a confirmation too, and work for a ticket starts only after you accept its plan. The one exception is inbox notes: the plugin delivers them to an idle primary without asking. The plugin tracks which session made each call for attribution within your user account. It is not a defense against other programs running as you.

## Documentation

- [Product intent and contract](docs/intent.md): goal, scope, ownership, safety invariants and the requirement-to-test map.
- [Repository and development rules](AGENTS.md): architecture, conventions and commands.
- [Herdr facts](docs/herdr-facts.md): the Herdr behavior this plugin depends on, and how it was verified.
- Decision records in [docs/adr/](docs/adr/). Start with [0001, the disposition contract](docs/adr/0001-disposition-contract.md), [0005, successors and re-points](docs/adr/0005-successors-and-repoint.md), [0006, forgetting a project](docs/adr/0006-forget-a-project.md) and [0007, settling uncertain notes](docs/adr/0007-settle-uncertain-notes.md).

## Requirements

- **Herdr 0.9.0 or newer.** The plugin checks Herdr's socket protocol on snapshot reads and refuses any value other than 22. [docs/herdr-facts.md](docs/herdr-facts.md) lists the builds it was verified on.
- **Oh My Pi (OMP)**, which the plugin extends.
- **Git** on `PATH`. Bind and tickets run Git in the repository.
- **Go 1.27.1**, the version `go.mod` names, to build the plugin executable.
- **Linux only:** `jq`, which the installer uses to read Herdr's JSON answers.

## Install

### Windows

Run this from the repository root, in a new terminal once the requirements are installed:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install.ps1
```

### Linux (x86_64)

Run this from the repository root:

```sh
./install.sh
```

Both scripts do the same four steps:

1. Build the plugin executable with `CGO_ENABLED=0`. On Linux the build goes into `build/linux-plugin/`, which holds a copy of `linux/herdr-plugin.toml`, the binary, and a link to `omp/`. Setup reads the extension source from the plugin root, so the link is needed.
2. Check Herdr's OMP integration. They run `herdr integration install omp` only when Herdr reports it missing.
3. Link the folder as the plugin `user.omp-orchestrator` and enable it if it is disabled. They stop on manifest warnings. On Linux the linked folder is `build/linux-plugin`, not the repository root.
4. Run the plugin's **Setup OMP orchestrator extension** action and wait up to two minutes for its answer.

Both need Herdr's server running, because step 4 runs through it. The scripts stop at the first failure. For the common causes they print the fix, and other failures print the raw error. Running them again is safe.

They never unlink or uninstall the plugin, and they never overwrite Herdr's integration. If Herdr already links `user.omp-orchestrator` from another folder, they stop before changing anything and name that folder. If that folder is gone or this checkout has moved, run `herdr plugin unlink user.omp-orchestrator` (Herdr's server must be running), then run the script again.

## Concepts

| Term | Meaning |
|---|---|
| Primary | The OMP session bound to a repository. It takes your requests and dispatches, reviews and closes work with the plugin's tools. It is read-only: it can read, search and use those tools, but it cannot edit files or run commands. |
| Worker | An OMP session that carries out one task in its own Git worktree, on its own `orch/` branch. |
| Planner | A read-only OMP session that writes a plan for a ticket, in its own worktree. |
| Ticket | A request you create on the board. It starts in Backlog. |
| Task | One unit of worker or planner work. Its board column comes from recorded facts, never from terminal activity. |
| Outcome | Your recorded decision on a task or ticket: `integrated`, `kept`, `rejected`, `superseded` or `abandoned`. `integrated` and `kept` accept the work. |
| Inbox note | A message from a worker or the orchestrator, delivered to the primary when it is idle. |

## First use

Herdr runs actions without a terminal, so Bind and Setup print their answer, refusals included, as the action's output. Read it with `herdr plugin log list --plugin user.omp-orchestrator`.

### The actions

| Action | ID | What it does |
|---|---|---|
| Setup OMP orchestrator extension | `setup` | Writes the plugin's OMP extension for the active profile. |
| Bind this repository as an orchestrated project | `bind` | Binds the repository of the pane you run it from, and starts its primary. |
| Open the task board | `board` | Opens the board for the project. |
| Show orchestrator status | `status` | Shows the plugin's status, read-only. |
| Reattach the primary OMP session | `reattach` | Replaces a lost primary, after you confirm. |
| Forget this project's orchestrator records | `forget` | Deletes the project's plugin records, after you confirm. |

### 1. Bind a repository

1. Open a pane in the repository and `cd` to its root.
2. Run **Bind this repository as an orchestrated project**.

Bind refuses with `integration_missing` or `extension_missing` until the install script, or **Setup OMP orchestrator extension**, has succeeded.

Bind starts the primary, an OMP session that coordinates the work:

- If the pane is a plain shell at the repository root that no record claims, the primary runs in that pane. A plain shell is one where Herdr reports no agent, or `shell`.
- Otherwise bind leaves the pane alone and starts the primary in a new workspace on the same checkout.
- Bind sends the primary no message. It waits for your first request.

The first bind refuses a pane that is not at the repository root, including a subfolder, with `different_repository`. Running bind again on the same repository reopens the recorded primary and never starts a second one. It ignores the pane you run it from. If the recorded primary's pane is gone, or now runs something else or another folder, bind refuses with `primary_not_live`. It names **Reattach the primary OMP session** and never substitutes a new primary.

### 2. Ask the primary for work

Type requests into the primary's pane. Each dispatch needs a base commit. Before its first dispatch the primary has none, and it cannot run git, so put a commit in your request. A hash of at least seven hex characters from `git rev-parse --short HEAD` works. HEAD and branch names are refused.

For example: "Make two independent changes, X and Y, each by its own worker. Branch both from abc1234."

Then:

- A dispatch that does not name a ticket shows an OMP dialog, `Dispatch work without a ticket`, with the exact request. Declining starts nothing. Work for a ticket skips that dialog. It starts only after the ticket's plan is accepted (see [Plan a ticket](#4-plan-a-ticket-optional)).
- Each worker gets its own Git worktree on its own `orch/` branch, which Herdr creates. The plugin runs up to three workers at once, and further tasks queue. This limit is fixed in this build.
- A worker's question makes its task **Needs input**. The primary asks you, and sends your answer to the worker only after you confirm its exact text in a `Send answer to worker` dialog.
- Worker reports and questions reach the primary as inbox notes once it is idle. Each batch is introduced as context recorded by workers and by the orchestrator, not by you.

### 3. Review what workers return

- When a worker finishes, its task is **Ready for review**. Its report never completes a task alone.
- Ask the primary to record an outcome. Recording asks you to confirm in a `Record review disposition` dialog.
- `integrated` and `kept` accept the work and release any task waiting on it. `rejected`, `superseded` (which must name its successor) and `abandoned` do not.
- Recording an outcome never merges anything. You merge the `orch/` branches yourself.
- To remove a reviewed worker's checkout, ask the primary. It asks you to confirm in a `Remove worker checkout` dialog. The branch is kept, and the removal is never forced. Cleanup refuses a running worker (`worker_live`) or a checkout that still holds work (`checkout_dirty`).

### 4. Plan a ticket (optional)

1. On the board, press `n` and type a title. Enter saves it and Esc cancels. Creating a ticket starts nothing.
2. Select the ticket in **Backlog** and press `m`, then `y` to confirm. Its planner starts, or queues if all three worker slots are busy. The move needs a commit at HEAD, and without one it is refused with `bad_base_revision`. You give the planner no commit hash.
3. The planner reads the repository in its own worktree and reports a plan without editing anything. The plan appears as a **Ready for review** card.
4. Ask the primary to record `integrated` or `kept` on the plan to accept it. Only then can the primary dispatch implementation for the ticket. Until a plan is accepted, that dispatch is refused with `ticket_plan_not_accepted`. The plugin never starts implementation from a plan by itself.
5. If you reject, supersede or abandon the plan, implementation stays refused. The ticket stays in **Todo**, and you close it with an outcome instead.
6. To close a ticket, ask the primary once its tasks are decided. Closing asks you to confirm in a `Record ticket decision` dialog. A ticket with an open linked task, planners included, is refused with `ticket_open`.

## The task board

Run **Open the task board** from the project's pane or workspace. The board opens over Herdr's active pane and refreshes itself every two seconds.

### Columns

| Column | Holds |
|---|---|
| Queued | Tasks waiting for a worker slot, and tasks whose worker has not started yet. |
| Running | Tasks whose worker or planner has started. |
| Needs input | Tasks that need you: a worker question, a blocker or an attention item. |
| Ready for review | Tasks with a final result and no recorded outcome. |
| Done | Tasks with a recorded outcome. |

**Backlog** and **Todo** hold tickets that have no tasks yet. A ticket you move to Todo gets its planner recorded at once. From then on the board shows the ticket through that planner's task.

### Keys

| Key | Action |
|---|---|
| ↑ ↓, `j` `k` | Move between cards. With details open, they scroll. |
| Tab, Shift+Tab | Next or previous column, with details closed. |
| Enter | Open or close the selected card's details. |
| Esc | Close the details. With none open, close the board. |
| `/` | Filter task cards: type, Enter applies, Esc cancels, Ctrl+U clears. Clear a filter with `/`, then Enter on an empty query. |
| `f` | Focus the selected worker's pane, once the plugin has verified it. Tickets have no pane. |
| `r` | Re-read now. |
| `n` | New ticket: type a title, Enter saves (a blank title is refused), Esc cancels. |
| `m` | On a Backlog ticket, move it to Todo and start its planner. Confirm with `y`. Any other key cancels. |
| `q` | Close the board, even with details open. |

The board's only writes are creating a ticket and moving a Backlog ticket to Todo. A move records the planner, and if worker slots are free it can start queued work first.

## Status overlay

Run **Show orchestrator status**. It is read-only and closes on Enter. It shows:

- `OMP integration: verified` and `Plugin extension: verified`, or the setup code and message when either cannot be verified.
- `Project: <key>`, or `Project: none resolved` (`Project: none resolved (no state recorded yet)` before any state exists).
- `Primary: no problem recorded`, or `Primary: not verified; reattach is needed`.
- `Board: can open`, which means the state folder, the Herdr path and a readable project are all set. It does not contact Herdr. Otherwise it reads `Board: cannot open (<reason>)`.

## When something goes wrong

### Restarts and uncertain notes

Restarting Herdr keeps all recorded facts and board state. At startup the plugin reconciles its records with Herdr's live state. That can change facts: a note that was being sent becomes **uncertain**, and an idle, verified primary may receive pending notes.

An uncertain note is one whose delivery the plugin cannot prove either way, so it is never resent. After you inspect the primary's pane, ask the primary to settle the note with `orch_settle_note`. It previews the note, and you confirm its decision in a `Settle inbox note` dialog. `delivered` ends the note. `not_delivered` returns it to pending for the primary's next idle turn.

### Primary not verified

The board shows **Primary not verified:** followed by a reason when the recorded primary is absent or cannot be verified, or when a reattach is still unconfirmed.

Run **Reattach the primary OMP session** from the pane that runs the replacement OMP, at the repository root. The overlay shows what it would record and asks `Type yes to reattach, anything else to cancel:`. Only `yes` or `y` writes anything. Anything else prints `Cancelled. Nothing was changed.` Reattach does not touch workers, tasks or the inbox. It refuses a primary that is still verified live (`primary_live`), and a replacement pane that is not idle (`replacement_busy`).

If a reattach was recorded but not confirmed (`reattach_pending`), send a message in the replacement session, then run **Bind this repository as an orchestrated project** to confirm it.

### Refusal codes

Bind and Setup write their codes to the plugin log. A dispatch refusal reaches the primary. The board shows a ticket refusal as a notice, such as `Cannot move <id>: ...`. Forget codes appear in the forget pane's output and its stderr envelope.

| Code | Meaning | What to do |
|---|---|---|
| `integration_missing` | Herdr does not report its OMP integration installed for this profile, or `herdr-omp-agent-state.ts` is missing from the extensions folder. | Run `herdr integration install omp` (for the profile the plugin runs in), then run Setup again. |
| `extension_missing` | `herdr-orchestrator.ts` is not in the profile's extensions folder. | Run **Setup OMP orchestrator extension**, or the install script again. |
| `extension_conflict` | A `herdr-orchestrator.ts` that this plugin did not write is already in that folder. | Move or rename it, then run Setup again. |
| `source_unavailable` | Setup cannot read `omp/herdr-orchestrator.ts` under the plugin root, or its first line is not the managed marker followed by a line feed. A checkout that converts line endings to CRLF, such as Git with `core.autocrlf=true`, causes the second case. | Install again from a checkout whose `omp/herdr-orchestrator.ts` has LF line endings. |
| `different_repository` | The pane you ran bind from is not at the repository root, or is in another workspace. | Run bind from a pane at the repository root. |
| `not_git_repository` | The folder is not inside a Git work tree. | Bind from a folder inside one. |
| `primary_not_live` | The recorded primary's pane is gone, or now runs something else. Bind never substitutes a new primary. | Start OMP at the repository root in a pane, then run **Reattach the primary OMP session** from it. |
| `already_bound` | The pane or its session already belongs to another binding. | Use a different pane. |
| `internal` | Bind has an unresolved workspace creation from an earlier attempt. | Check Herdr's workspace list. If the workspace exists, bind from its pane. Bind never creates a second one. |
| `bad_base_revision` | A dispatch base is not a commit of this repository: `HEAD`, a branch name, a hash shorter than seven characters, or text that is not hex. For a ticket move, the repository has no commit at HEAD. | Give a hash of at least seven hex characters from `git rev-parse --short HEAD`. For a ticket move, make a first commit. |
| `ticket_plan_not_accepted` | A dispatch for a ticket whose planner has no accepting outcome. | Record `integrated` or `kept` on the plan first. |
| `ticket_open` | A ticket decision while a linked task, planners included, is still open. | Decide the open tasks, then close the ticket. |
| `reserved_conflict` | The new task's reserved paths overlap an active worker's. | Wait for that worker, or reserve other paths. |
| `not_reviewed` | Review or cleanup of a task that has no final result yet. | Wait for the worker's result report. |
| `worker_live` | Cleanup while the worker agent is still running. | Close the worker's agent, then clean up again. |
| `checkout_dirty` | Cleanup of a checkout that still holds work. | Commit, move or remove that work, then clean up again. |
| `session_unavailable` | Herdr has not yet reported the pane's session. | Wait a moment and try again. |
| `replacement_busy` | The replacement pane for a reattach is not idle. | Wait for it to be idle, then confirm again. |
| `primary_live` | A reattach was refused because the recorded primary is still verified live. | Reattach replaces only a lost or unproven primary. |
| `reattach_pending` | A reattach was recorded, but the replacement is not confirmed. | Send a message in the replacement session, then run **Bind this repository as an orchestrated project**. |
| `project_unknown` | The action resolves to no bound project, or to more than one. | Bind the repository first. |
| `forget_refused` | The forget plan has blockers. | See [Forget a project](#forget-a-project). |
| `forget_plan_changed` | The project changed after the plan was shown. | Open the forget action again, review the new plan and confirm it. |
| `forget_not_confirmed` | The typed folder name did not match, or Enter cancelled. Nothing was deleted. | Open the forget action again to retry. |

## Forget a project

Use **Forget this project's orchestrator records** to remove this plugin's records for a repository, so you can bind it again as a new project. Run it from a pane or workspace of that project. The overlay, titled **Forget the project**, shows a read-only plan first: the number of tasks (and how many are decided), tickets, delivered notes and pending notes, and what stays.

Forgetting refuses while the plan has any of these blockers:

- A task has no outcome. Record one through the primary first, such as `abandoned`.
- A task is claimed and must finish first.
- A note is `sending`.
- A note is `uncertain`. Inspect the primary's pane, then have the primary settle it with `orch_settle_note`.
- A bind intent for the project is open. Run **Bind this repository as an orchestrated project**.
- A reattach notice is uncertain. Send a message in the replacement session, then run Bind (or Reattach again) to confirm it.
- The primary is not verified. Reattach it first to record outcomes or deliver notes. This line appears only alongside another blocker, so a missing primary does not block forgetting by itself.

Pending notes do not block forgetting.

After you confirm, the plugin deletes, in one transaction:

- the project's `inbox` rows (pending notes included), `tickets`, `tasks` and `projects` rows;
- the `ready_sessions` rows for the project's current launch pane and for its tasks' current launch panes.

Some `ready_sessions` rows stay: those for panes the project no longer records, such as a cleaned-up worker pane or a primary pane the project was reattached away from. `task_history` is never deleted. Its rows stay as the audit trail for the forgotten work, keeping the project key, and the database blocks changes to them.

The repository, its branches and worktrees, and the Herdr workspaces and panes stay. Forgetting does not remove checkouts or close sessions, and it cannot be undone.

To confirm, type the project's folder name as the prompt shows it, then press Enter. The match ignores case and surrounding spaces. Anything else, including Enter alone, cancels. The pane then prints `Cancelled. Nothing was changed: the typed folder name did not match, or Enter was pressed.` and the code `forget_not_confirmed`. A confirmed forget prints `Forgot <key>: <n> tasks, <n> tickets, <n> notes removed.`

The primary's OMP session stays read-only until it restarts. The overlay says to restart the session, or close its pane, to clear the restriction.

## Setup and the OMP extension

Setup is the **Setup OMP orchestrator extension** action. You can also run `herdr-omp-orchestrator.exe setup` on Windows, or `herdr-omp-orchestrator setup` on Linux, from the plugin folder. It writes one file, `herdr-orchestrator.ts`, into OMP's extensions folder for the active profile. It never writes Herdr's `herdr-omp-agent-state.ts` or OMP's configuration, and it does not create the plugin's state folder.

Where the file goes:

- The profile is the trimmed value of `OMP_PROFILE`, or of `PI_PROFILE` when `OMP_PROFILE` is not defined. An empty value or `default` selects the default profile. Any other value names a profile. The match is exact, so `Default` names a profile.
- Default profile: `$HOME/.omp/agent/extensions/herdr-orchestrator.ts`. `$HOME` falls back to `%USERPROFILE%` when it is unset or blank, on every platform. `PI_CONFIG_DIR` replaces `.omp`, and is joined under `$HOME`, so it must be relative to it. `PI_CODING_AGENT_DIR` replaces `.omp/agent` and is used as given.
- Named profile: `$HOME/.omp/profiles/<name>/agent/extensions/herdr-orchestrator.ts`, with `PI_CONFIG_DIR` replacing `.omp` as above. `PI_CODING_AGENT_DIR` does not apply.

What setup does:

- Before writing, it runs `herdr integration status`. It refuses with `integration_missing` unless Herdr reports the OMP integration installed and `herdr-omp-agent-state.ts` is in the same folder.
- The file embeds the absolute paths of the plugin executable, `HERDR_PLUGIN_STATE_DIR` and the Herdr CLI (`HERDR_BIN_PATH`). If the plugin folder moves, run setup again.
- The write is atomic: a temporary file, then a rename. An identical file is left alone, so a repeated run changes nothing.
- It never replaces a `herdr-orchestrator.ts` that does not start with the plugin's managed marker line. It refuses with `extension_conflict` instead.

`setup --dry-run` previews a setup without writing. It prints a JSON answer with `"dryRun": true` and the `action` setup would take (`create`, `refresh` or `unchanged`), or the refusal. The preview needs the environment the Setup action gets: `HERDR_PLUGIN_STATE_DIR` and `HERDR_BIN_PATH`, and `HERDR_PLUGIN_ROOT` naming a folder that contains `omp/herdr-orchestrator.ts`. If `HERDR_PLUGIN_ROOT` is unset, the working directory is used instead. Setup accepts no other arguments. Any other argument fails with the `usage` code and exit status 2. A successful setup exits 0, and a refusal exits 1.

To remove the extension, delete `herdr-orchestrator.ts` from the profile's extensions folder. Leave `herdr-omp-agent-state.ts` in place, because it belongs to Herdr. Until setup installs the extension again, bind refuses with `extension_missing`. The plugin has no uninstall action.

## Development

```sh
go test -timeout 30m ./...
CGO_ENABLED=0 go build -o herdr-omp-orchestrator ./cmd/herdr-omp-orchestrator
```

On Windows, set `$env:CGO_ENABLED = '0'` and build with `go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator`. The `-timeout 30m` is needed because `internal/coord` takes longer than Go's default ten minutes on Windows.

The Windows helper and acceptance scripts, the Linux install and smoke scripts, and the optional OMP extension load check are described in [AGENTS.md](AGENTS.md). Both acceptance paths drive real Herdr and OMP sessions in an isolated named session with a temporary worktree root. The Linux smoke path needs `install.sh` to have run first, and it never links or unlinks the plugin. It covers setup, bind, dispatch, restart and ticket move. It does not cover review, reattach or the board UI.
