// Command herdr-omp-orchestrator is the plugin's single Go executable.
//
// Herdr runs it as a plugin action (for example `open-board`), its startup and
// event hooks run `startup`/`event`, its plugin pane runs `board`, and the OMP
// extension runs it as `bridge <method>` for every tool invocation. Every
// route reads the environment it was started with; the non-interactive ones
// answer with one JSON object, so a human reading a Herdr plugin log and the
// extension parsing stdout see the same result.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/bridge"
	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/setup"
	"herdr-omp-orchestrator/internal/state"
)

// bridgeTimeout bounds one bridge invocation in the executable itself. The
// extension imposes its own, shorter timeout; this is the backstop that keeps a
// wedged process from outliving the turn that started it.
const bridgeTimeout = 2 * time.Minute

// bindTimeout bounds one bind action: it may create a workspace, start an OMP
// agent and wait for the extension's readiness handshake.
const bindTimeout = 90 * time.Second

// startupTimeout bounds the startup reconciliation pass. It reads Herdr's
// snapshot and the repository's worktree listing, and may prompt a verified idle
// primary, so it is bounded well inside the timeout a failing plugin command is
// given.
const startupTimeout = 2 * time.Minute

// eventTimeout bounds one hook. A hook applies a single observation and may
// prompt a verified idle primary, which is one bounded Herdr call.
const eventTimeout = 60 * time.Second

// openBoardTimeout bounds one `open-board` action: it resolves the project and
// asks Herdr to open the board pane. The interactive board itself is not
// bounded: the reader closes it.
const openBoardTimeout = 30 * time.Second

// pluginID and boardEntrypoint are the manifest's own names
// (herdr-plugin.toml): the action opens the pane the plugin declares.
const (
	pluginID        = "user.omp-orchestrator"
	boardEntrypoint = "board"
	forgetEntrypoint = "forget"
)

// contextLimit bounds the plugin invocation context this executable accepts.
const contextLimit = 1 << 20

// Environment the bind route requires, the hook payload Herdr injects, and the
// Herdr binary the routes may read.
const (
	envStateDir      = "HERDR_PLUGIN_STATE_DIR"
	envContextJSON   = "HERDR_PLUGIN_CONTEXT_JSON"
	envHerdrBinary   = "HERDR_BIN_PATH"
	envHerdrBinaryAt = "HERDR_ORCH_HERDR_BIN_PATH"
	// envWorkspaceID is the workspace a plugin command was invoked in; the board
	// pane route uses it even when the JSON context is unavailable.
	envWorkspaceID = "HERDR_WORKSPACE_ID"
	// envPluginEvent and envPluginEventJSON are what Herdr injects into every
	// hook: the hook name and the event payload. Both are read as-is, never
	// reconstructed from argv.
	envPluginEvent     = "HERDR_PLUGIN_EVENT"
	envPluginEventJSON = "HERDR_PLUGIN_EVENT_JSON"
)

const usage = "usage: herdr-omp-orchestrator <setup [--dry-run]|bind|startup|event|board|open-board|forget|open-forget|reattach|open-reattach|bridge <method>|recover|--version>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	env := environMap(os.Environ())
	recoveryFailure := func(err error) int {
		return emit(os.Stdout, os.Stderr, failureBody("recovery_unavailable", err.Error()))
	}
	recoverySignalFailure := func(err error) {
		fmt.Fprintf(os.Stderr, "herdr-omp-orchestrator: %v\n", err)
	}
	switch os.Args[1] {
	case "setup":
		os.Exit(runSetup(env, os.Args[2:], os.Stdout, os.Stderr))
	case "bind":
		os.Exit(runBind(env, os.Stdout, os.Stderr))
	case "startup":
		os.Exit(runRecoverableRoute(
			func() (func(bool) error, error) { return startRecoveryWorker(recoveryScopeAll, "") },
			func() (int, bool) { return runStartup(env, os.Stdout, os.Stderr), true },
			recoveryFailure,
			recoverySignalFailure,
		))
	case "event":
		os.Exit(runEvent(env, os.Stdout, os.Stderr))
	case "board":
		os.Exit(runBoard(env, os.Stdout, os.Stderr))
	case "open-board":
		os.Exit(runOpenBoard(env, os.Stdout, os.Stderr))
	case "forget":
		os.Exit(runForget(env, os.Stdin, os.Stdout, os.Stderr))
	case "open-forget":
		os.Exit(runOpenForget(env, os.Stdout, os.Stderr))
	case "reattach":
		os.Exit(runReattach(env, os.Stdin, os.Stdout, os.Stderr))
	case "open-reattach":
		os.Exit(runOpenReattach(env, os.Stdout, os.Stderr))
	case "bridge":
		args := os.Args[2:]
		if scope, recoverable := bridgeRecoveryScope(args); recoverable {
			os.Exit(runRecoverableRoute(
				func() (func(bool) error, error) { return startRecoveryWorker(scope, "") },
				func() (int, bool) {
					return runBridgeRecoverable(env, os.Stdin, os.Stdout, os.Stderr, args)
				},
				recoveryFailure,
				recoverySignalFailure,
			))
		}
		os.Exit(runBridge(env, os.Stdin, os.Stdout, os.Stderr, args))
	case "recover":
		os.Exit(runRecovery(env, os.Stderr, os.Args[2:]))
	case "--version":
		os.Exit(runVersion(os.Args[2:], os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "herdr-omp-orchestrator: unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

// pluginContext is the invocation context Herdr injects into a plugin command.
// Only the fields this route needs are read; everything else is ignored.
type pluginContext struct {
	FocusedPaneID    string `json:"focused_pane_id"`
	FocusedPaneCwd   string `json:"focused_pane_cwd"`
	FocusedPaneAgent string `json:"focused_pane_agent"`
	WorkspaceCwd     string `json:"workspace_cwd"`
	WorkspaceID      string `json:"workspace_id"`
}

// runBind binds the primary OMP session to the repository the action was
// invoked in. The action answers with one JSON object like every other route,
// so the Herdr plugin log shows the same result a caller sees.
func runBind(env map[string]string, stdout, stderr io.Writer) int {
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		return emit(stdout, stderr, failureBody("state_dir_unavailable", envStateDir+" is not set"))
	}
	raw := strings.TrimSpace(env[envContextJSON])
	if raw == "" {
		return emit(stdout, stderr, failureBody("context_unavailable", envContextJSON+" is not set"))
	}
	if len(raw) > contextLimit {
		return emit(stdout, stderr, failureBody("context_too_large", fmt.Sprintf("the invocation context exceeds the %d byte bound", contextLimit)))
	}
	var invocation pluginContext
	if err := json.Unmarshal([]byte(raw), &invocation); err != nil {
		return emit(stdout, stderr, failureBody("context_unavailable", "the invocation context is not a JSON object"))
	}
	repo := strings.TrimSpace(invocation.FocusedPaneCwd)
	if repo == "" {
		repo = strings.TrimSpace(invocation.WorkspaceCwd)
	}
	if repo == "" {
		return emit(stdout, stderr, failureBody("context_unavailable", "the invocation context names no working directory to bind"))
	}

	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	defer closeCoordinator()

	ctx, cancel := context.WithTimeout(context.Background(), bindTimeout)
	defer cancel()
	project, err := coordinator.Bind(ctx, repo, invocation.WorkspaceID, invocation.FocusedPaneID)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	answer := map[string]any{"ok": true, "project": project}
	// The session hint carries the resume command only when the runtime has
	// materialized the session: an unmaterialized session is bound but not yet
	// resumable, and saying so beats advertising a command that cannot work.
	if hint, hintErr := coordinator.SessionHint(ctx, project.LaunchPaneID); hintErr == nil {
		answer["session"] = hint
	} else {
		answer["session"] = map[string]any{
			"onDisk":            false,
			"resumeUnavailable": "the announced session could not be read back: " + hintErr.Error(),
		}
	}
	return emit(stdout, stderr, answer)
}

// runStartup is Herdr's `[[startup]]` hook: it reconciles the recorded
// identities against Herdr's live snapshot and the repository's worktree
// listing, because event history is not replayed. It is repeat-safe, so a
// restarted server (which runs the hook again) cannot double-apply anything.
func runStartup(env map[string]string, stdout, stderr io.Writer) int {
	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	defer closeCoordinator()

	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	if err := coordinator.Reconcile(ctx); err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "reconciled": true})
}

// runEvent is Herdr's `[[events]]` hook: one observation from
// HERDR_PLUGIN_EVENT_JSON, applied to the facts the store already holds.
//
// A failure answers with the same envelope every other route uses and exits
// nonzero, so Herdr records it in the plugin log; the event itself is never
// interpreted through a shell or from argv.
func runEvent(env map[string]string, stdout, stderr io.Writer) int {
	eventName := strings.TrimSpace(env[envPluginEvent])
	if eventName == "" {
		return emit(stdout, stderr, failureBody("event_unavailable", envPluginEvent+" is not set"))
	}
	// The bound applies to the bytes Herdr delivered, before any trimming, so a
	// payload padded with whitespace around a small event is refused whole
	// instead of being accepted as a smaller one.
	rawPayload := env[envPluginEventJSON]
	if len(rawPayload) > coord.MaxEventPayloadBytes {
		return emit(stdout, stderr, failureBody("event_payload_too_large",
			fmt.Sprintf("the %s payload is %d bytes, larger than the %d byte bound",
				eventName, len(rawPayload), coord.MaxEventPayloadBytes)))
	}
	if strings.TrimSpace(rawPayload) == "" {
		return emit(stdout, stderr, failureBody("event_payload_unavailable", envPluginEventJSON+" is not set"))
	}

	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	defer closeCoordinator()

	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()
	if err := coordinator.HandleEvent(ctx, eventName, []byte(rawPayload)); err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "event": eventName})
}

// runBoard is the `[[panes]]` route: the board for the project the pane was
// opened in, read-only for tasks, running in this terminal until the reader
// closes it.
//
// The pane route reads the state store and the Herdr binary the invocation was
// started with; it resolves its own project from the injected workspace or
// working directory and refuses rather than guessing. Nothing is written on
// stdout on success: this terminal belongs to the board, not to a JSON envelope.
func runBoard(env map[string]string, stdout, stderr io.Writer) int {
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		return emit(stdout, stderr, failureBody("state_dir_unavailable", envStateDir+" is not set"))
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return emit(stdout, stderr, failureBody("state_write_failed", fmt.Sprintf("open the orchestrator state: %v", err)))
	}
	defer func() { _ = store.Close() }()
	client, err := herdr.NewClient(herdrBinary(env))
	if err != nil {
		return emit(stdout, stderr, failureBody("herdr_unavailable", err.Error()))
	}

	invocation := invocationContext(env)
	project, err := board.ProjectFor(context.Background(), store,
		firstNonEmpty(invocation.WorkspaceID, env[envWorkspaceID]), invocationDir(invocation))
	if err != nil {
		return emit(stdout, stderr, failureBody("project_unknown", err.Error()))
	}
	// The board owns the terminal until it is closed; no timeout applies, and
	// Bubble Tea is left with the process's own stdio.
	if err := board.Run(context.Background(), store, client, project.Key,
		boardOptions(store, client, func(ticketID string) (func(bool) error, error) {
			return startRecoveryWorker(recoveryScopeOwner, ticketID)
		})); err != nil {
		return emit(stdout, stderr, failureBody("internal", err.Error()))
	}
	return 0
}

// boardOptions gives the board its one write path. The board creates and moves
// tickets only through board.TicketOps, and in this executable that is the
// coordinator, so the same project check, digest check and decision rules apply
// as for the primary's bridge calls. The project key the board passes is the one
// runBoard resolved from the pane's own context, never one the board chose.
//
// A coordinator that cannot be built does not stop the board from opening: it is
// returned read-only, and says so when a ticket key is pressed.
func boardOptions(
	store *state.Store,
	client *herdr.Client,
	launchRecovery ...func(string) (func(bool) error, error),
) board.Options {
	coordinator, err := coord.New(store, client, coord.Options{})
	if err != nil {
		return board.Options{}
	}
	var launch func(string) (func(bool) error, error)
	if len(launchRecovery) > 0 {
		launch = launchRecovery[0]
	}
	return board.Options{Tickets: ticketWriter{coordinator: coordinator, launchRecovery: launch}}
}

// ticketWriter is board.TicketOps over the coordinator's project-keyed ticket
// operations. The board package imports nothing from coord; this is the seam.
type ticketWriter struct {
	coordinator    *coord.Coordinator
	moveTicket     func(context.Context, string, string, string) (coord.TicketView, error)
	launchRecovery func(string) (func(bool) error, error)
}

func (w ticketWriter) CreateTicket(ctx context.Context, projectKey, title string) error {
	_, err := w.coordinator.CreateTicketFor(ctx, projectKey, coord.TicketCreateParams{Title: title})
	return err
}

func (w ticketWriter) MoveTicketToTodo(ctx context.Context, projectKey, ticketID, digest string) error {
	var complete func(bool) error
	if w.launchRecovery != nil {
		var err error
		complete, err = w.launchRecovery(ticketID)
		if err != nil {
			return fmt.Errorf("start the recovery worker before moving the ticket: %w", err)
		}
	}
	var moveErr error
	if w.moveTicket != nil {
		_, moveErr = w.moveTicket(ctx, projectKey, ticketID, digest)
	} else {
		_, moveErr = w.coordinator.MoveTicketFor(ctx, projectKey, ticketID, digest)
	}
	if complete != nil {
		var notStarted *coord.PlannerNotStartedError
		moved := moveErr == nil || errors.As(moveErr, &notStarted)
		if err := complete(moved); err != nil {
			signalErr := fmt.Errorf("signal ticket-move recovery: %w", err)
			if moveErr == nil {
				return &board.MovedTicketSignalError{Err: signalErr}
			}
			if moved {
				return &board.MovedTicketSignalError{Err: errors.Join(boardMoveError(moveErr), signalErr)}
			}
			return errors.Join(boardMoveError(moveErr), signalErr)
		}
	}
	return boardMoveError(moveErr)
}

// boardMoveError is the board's view of a move's error: a move the coordinator
// recorded but whose planner did not start becomes a board.MovedTicketError
// carrying why, so the board reports the ticket as moved; any other error is the
// move's refusal and passes through unchanged.
func boardMoveError(err error) error {
	var notStarted *coord.PlannerNotStartedError
	if errors.As(err, &notStarted) {
		return &board.MovedTicketError{Err: notStarted.Err}
	}
	return err
}

// runOpenBoard is the `[[actions]]` route: it opens the board pane for the bound
// project the action was invoked from. Herdr owns pane topology, so this route
// calls `plugin pane open` through the Herdr binary the action was started with;
// the plugin id and entrypoint are the ones herdr-plugin.toml declares.
func runOpenBoard(env map[string]string, stdout, stderr io.Writer) int {
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		return emit(stdout, stderr, failureBody("state_dir_unavailable", envStateDir+" is not set"))
	}
	binary := herdrBinary(env)
	if binary == "" {
		return emit(stdout, stderr, failureBody("herdr_unavailable", envHerdrBinary+" is not set"))
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return emit(stdout, stderr, failureBody("state_write_failed", fmt.Sprintf("open the orchestrator state: %v", err)))
	}
	defer func() { _ = store.Close() }()
	client, err := herdr.NewClient(binary)
	if err != nil {
		return emit(stdout, stderr, failureBody("herdr_unavailable", err.Error()))
	}

	invocation := invocationContext(env)
	project, err := board.ProjectFor(context.Background(), store,
		firstNonEmpty(invocation.WorkspaceID, env[envWorkspaceID]), invocationDir(invocation))
	if err != nil {
		return emit(stdout, stderr, failureBody("project_unknown", err.Error()))
	}
	workspace := firstNonEmpty(project.RepoWorkspaceID, project.CurrentWorkspaceID)
	if workspace == "" {
		return emit(stdout, stderr, failureBody("project_unknown",
			fmt.Sprintf("the bound project %s records no workspace to open the board in", project.Key)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), openBoardTimeout)
	defer cancel()
	// The manifest declares this pane as an overlay, which Herdr anchors to the
	// active pane. A --workspace argument is only valid for workspace placements.
	opened, err := client.Call(ctx, "plugin", "pane", "open",
		"--plugin", pluginID, "--entrypoint", boardEntrypoint)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "project": project.Key, "workspace": workspace, "pane": opened})
}

// invocationContext reads the JSON object Herdr injects into a plugin command.
// A missing or unreadable context is never fatal here: the board resolves its
// project from the identities it does have, and refuses rather than guessing.
func invocationContext(env map[string]string) pluginContext {
	raw := strings.TrimSpace(env[envContextJSON])
	if raw == "" || len(raw) > contextLimit {
		return pluginContext{}
	}
	var invocation pluginContext
	if err := json.Unmarshal([]byte(raw), &invocation); err != nil {
		return pluginContext{}
	}
	return invocation
}

// invocationDir is the working directory the invocation named: the focused
// pane's if it has one, otherwise the workspace's.
func invocationDir(invocation pluginContext) string {
	return firstNonEmpty(invocation.FocusedPaneCwd, invocation.WorkspaceCwd)
}

// firstNonEmpty returns the first value that is not blank.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// runBridge answers one extension call. The request is read from stdin with a
// hard bound and the answer is written to stdout, so an oversized or malformed
// request is still answered in the documented envelope.
func runBridge(env map[string]string, stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	method := ""
	if len(args) > 0 {
		method = strings.TrimSpace(args[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), bridgeTimeout)
	defer cancel()

	ops, closeCoordinator, opErr := bridgeOps(env)
	defer closeCoordinator()

	out, err := bridge.Run(ctx, env, stdin, bridge.Options{ArgvMethod: method, Ops: ops})
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: bridge: %v\n", err)
		return 2
	}
	if opErr != nil {
		// `resolve` and `ready` still work: only the coordinator operations are
		// unavailable, and each of them says so instead of failing silently.
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: bridge: coordinator operations unavailable: %v\n", opErr)
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: bridge: %v\n", err)
		return 2
	}
	return 0
}

func runBridgeRecoverable(env map[string]string, stdin io.Reader, stdout, stderr io.Writer, args []string) (int, bool) {
	var output bytes.Buffer
	status := runBridge(env, stdin, &output, stderr, args)
	if _, err := stdout.Write(output.Bytes()); err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: bridge: %v\n", err)
		return 2, true
	}
	method := ""
	if len(args) > 0 {
		method = strings.TrimSpace(args[0])
	}
	return status, bridgeResponseNeedsRecovery(method, output.Bytes())
}

func bridgeResponseNeedsRecovery(method string, payload []byte) bool {
	var response struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return true
	}
	switch method {
	case bridge.MethodDispatch:
		var result struct {
			Duplicate bool `json:"duplicate"`
		}
		err := json.Unmarshal(response.Result, &result)
		if response.OK && err == nil && result.Duplicate {
			return false
		}
		return true
	case bridge.MethodTicketMove:
		return response.OK
	case bridge.MethodReview:
		// Review always runs the idempotent queue drain after an accepting or
		// repeated decision, so the recovery worker is relevant to this route.
		return true
	case bridge.MethodAdjudicateBrief:
		if !response.OK {
			return true
		}
		var result struct {
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			return true
		}
		return result.Decision == "not_delivered"
	default:
		return false
	}
}

// openCoordinator opens the coordinator over the plugin's own state directory
// and the Herdr binary the environment was started with.
func openCoordinator(env map[string]string) (*coord.Coordinator, func(), error) {
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		return nil, func() {}, fmt.Errorf("%w: %s is not set", coord.ErrInvalidRequest, envStateDir)
	}
	herdrPath := herdrBinary(env)
	client, err := herdr.NewClient(herdrPath)
	if err != nil {
		return nil, func() {}, err
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open the orchestrator state: %w", err)
	}
	coordinator, err := coord.New(store, client, coord.Options{
		// Only Bind consults the gate: it refuses, before it records or starts
		// anything, when Herdr's OMP integration or this plugin's extension is
		// missing from the profile the action runs in. The check is read-only.
		Preflight: func(ctx context.Context) error { return setup.Verify(ctx, env, herdrPath) },
	})
	if err != nil {
		_ = store.Close()
		return nil, func() {}, err
	}
	return coordinator, func() { _ = store.Close() }, nil
}

// bridgeOps wires the coordinator's operations into the bridge, which authorizes
// the caller before any of them runs. A coordinator that could not be opened is
// answered with one code for every method, so the failure names what is missing
// instead of pretending the method does not exist.
func bridgeOps(env map[string]string) (bridge.Ops, func(), error) {
	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		return bridge.UnavailableOps(err), func() {}, err
	}
	return bridge.CoordinatorOps(coordinator), closeCoordinator, nil
}

// herdrBinary resolves the Herdr executable path for this route.
func herdrBinary(env map[string]string) string {
	if path := strings.TrimSpace(env[envHerdrBinaryAt]); path != "" {
		return path
	}
	return strings.TrimSpace(env[envHerdrBinary])
}

// coordCode maps a coordinator failure onto a stable code the extension and the
// plugin log can classify.
func coordCode(err error) string {
	switch {
	case errors.Is(err, coord.ErrForgetRefused):
		return "forget_refused"
	case errors.Is(err, coord.ErrForgetPlanChanged):
		return "forget_plan_changed"
	case errors.Is(err, coord.ErrForgetNotConfirmed):
		return "forget_not_confirmed"
	case errors.Is(err, coord.ErrReattachPending):
		// Checked first: a pending reattach wraps the cause that left it pending
		// (a readiness timeout, a refused notice), and the caller must hear that
		// the replacement is recorded but unconfirmed, not only why.
		return "reattach_pending"
	case errors.Is(err, coord.ErrNotReady):
		// The cause is the setup package's own proof, so the code is the one the
		// setup action reports for the same failure: a caller sees
		// integration_missing or extension_missing, not a generic refusal.
		var coded *setup.Error
		if errors.As(err, &coded) {
			return coded.Code()
		}
		return "not_ready"
	case errors.Is(err, coord.ErrPrimaryLive):
		return "primary_live"
	case errors.Is(err, coord.ErrAlreadyBound):
		return "already_bound"
	case errors.Is(err, coord.ErrReplacementBusy):
		return "replacement_busy"
	case errors.Is(err, coord.ErrEvidenceChanged):
		return "evidence_changed"
	case errors.Is(err, coord.ErrInvalidRequest):
		return "bad_request"
	case errors.Is(err, coord.ErrNotGitRepository):
		return "not_git_repository"
	case errors.Is(err, coord.ErrProjectUnknown):
		return "project_unknown"
	case errors.Is(err, coord.ErrDifferentRepository):
		return "different_repository"
	case errors.Is(err, coord.ErrBaseRevision):
		return "bad_base_revision"
	case errors.Is(err, coord.ErrReservedConflict):
		return "reserved_conflict"
	case errors.Is(err, coord.ErrPrimaryNotLive):
		return "primary_not_live"
	case errors.Is(err, coord.ErrAgentChanged):
		return "agent_changed"
	case errors.Is(err, coord.ErrSessionUnavailable):
		return "session_unavailable"
	case errors.Is(err, coord.ErrSessionRefUnsupported):
		return "session_reference_unsupported"
	case errors.Is(err, coord.ErrReadinessTimeout):
		return "readiness_timeout"
	case errors.Is(err, coord.ErrBlocked):
		return "blocked"
	case errors.Is(err, coord.ErrAmbiguousStep):
		return "ambiguous_step"
	case errors.Is(err, coord.ErrPersistence):
		return "state_write_failed"
	case errors.Is(err, herdr.ErrAmbiguous):
		// The step's outcome could not be identified: it mirrors the bridge's own
		// code for the same failure class, so an ambiguous operation never reads as
		// an unusable CLI wherever the caller saw it.
		return "ambiguous_operation"
	case errors.Is(err, coord.ErrHerdrUnavailable), errors.Is(err, herdr.ErrCLI),
		errors.Is(err, herdr.ErrTimeout):
		return "herdr_unavailable"
	default:
		return "internal"
	}
}

// setupDryRunFlag makes setup report what it would do, without writing.
const setupDryRunFlag = "--dry-run"

// runSetup installs the plugin's OMP extension and reports the outcome. The
// executable path it embeds is this binary, resolved at runtime, so a moved or
// reinstalled plugin refreshes the embedded path instead of leaving an
// extension that points at a stale executable. With --dry-run it reports the
// action an install would take and writes nothing.
func runSetup(env map[string]string, args []string, stdout, stderr io.Writer) int {
	dryRun, err := parseSetupArgs(args)
	if err != nil {
		emit(stdout, stderr, failureBody("usage", err.Error()))
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: setup: %v\n", err)
		return emit(stdout, stderr, failureBody("executable_unavailable", "the plugin executable path could not be resolved"))
	}
	if dryRun {
		return emitSetupPlan(env, executable, stdout, stderr)
	}
	if err := setup.Install(env, executable); err != nil {
		return emit(stdout, stderr, setupFailure(err))
	}
	path, err := setup.ExtensionPath(env)
	if err != nil {
		return emit(stdout, stderr, setupFailure(err))
	}
	return emit(stdout, stderr, map[string]any{
		"ok":        true,
		"extension": path,
		"protocol":  setup.Protocol,
	})
}

// parseSetupArgs reads the arguments setup accepts: none, or exactly --dry-run.
// Anything else is refused before any path is resolved, so a mistyped flag can
// never fall through to a real install. That refusal is a usage error and exits
// 2, like the other usage refusals of this executable.
func parseSetupArgs(args []string) (bool, error) {
	switch {
	case len(args) == 0:
		return false, nil
	case len(args) == 1 && args[0] == setupDryRunFlag:
		return true, nil
	default:
		return false, fmt.Errorf("setup accepts no arguments or exactly %s; got %q; usage: herdr-omp-orchestrator setup [%s]",
			setupDryRunFlag, args, setupDryRunFlag)
	}
}

// emitSetupPlan answers what setup would do for env and writes nothing. Its
// refusals are the install's own, so a preview fails and exits the way the real
// setup would.
func emitSetupPlan(env map[string]string, executable string, stdout, stderr io.Writer) int {
	plan, err := setup.PlanInstall(env, executable)
	if err != nil {
		return emit(stdout, stderr, setupFailure(err))
	}
	return emit(stdout, stderr, map[string]any{
		"ok":        true,
		"dryRun":    true,
		"action":    string(plan.Action),
		"extension": plan.Extension,
		"protocol":  setup.Protocol,
	})
}

// setupFailure renders a setup failure as the same envelope the bridge uses, so
// both routes answer with a stable code and a bounded message. It never quotes
// file contents: only the code and the message setup produced.
func setupFailure(err error) map[string]any {
	var coded *setup.Error
	if errors.As(err, &coded) {
		return failureBody(coded.Code(), err.Error())
	}
	return failureBody("internal", err.Error())
}

func failureBody(code, message string) map[string]any {
	return map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message}}
}

// emit writes one JSON answer and returns the process exit status.
func emit(stdout, stderr io.Writer, body map[string]any) int {
	encoded, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: %v\n", err)
		return 2
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: %v\n", err)
		return 2
	}
	if ok, _ := body["ok"].(bool); !ok {
		return 1
	}
	return 0
}

// environMap turns a process environment into a map without losing entries that
// contain '=' inside their value.
func environMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			out[key] = value
		}
	}
	return out
}
