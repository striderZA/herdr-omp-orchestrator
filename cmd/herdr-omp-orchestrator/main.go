// Command herdr-omp-orchestrator is the plugin's single Go executable.
//
// Herdr runs it as a plugin action (for example `setup`) and the OMP extension
// runs it as `bridge <method>` for every tool invocation. Both routes read the
// environment they were started with and answer with one JSON object, so a
// human reading a Herdr plugin log and the extension parsing stdout see the
// same result.
//
// Later tasks add the `startup`, `event` and board routes; this file
// deliberately wires only the routes that exist.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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

// contextLimit bounds the plugin invocation context this executable accepts.
const contextLimit = 1 << 20

// Environment the bind route requires, and the Herdr binary it may read.
const (
	envStateDir      = "HERDR_PLUGIN_STATE_DIR"
	envContextJSON   = "HERDR_PLUGIN_CONTEXT_JSON"
	envHerdrBinary   = "HERDR_BIN_PATH"
	envHerdrBinaryAt = "HERDR_ORCH_HERDR_BIN_PATH"
)

const usage = "usage: herdr-omp-orchestrator <setup|bind|bridge <method>>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	env := environMap(os.Environ())
	switch os.Args[1] {
	case "setup":
		os.Exit(runSetup(env, os.Stdout, os.Stderr))
	case "bind":
		os.Exit(runBind(env, os.Stdout, os.Stderr))
	case "bridge":
		os.Exit(runBridge(env, os.Stdin, os.Stdout, os.Stderr, os.Args[2:]))
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

// openCoordinator opens the coordinator over the plugin's own state directory
// and the Herdr binary the environment was started with.
func openCoordinator(env map[string]string) (*coord.Coordinator, func(), error) {
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		return nil, func() {}, fmt.Errorf("%w: %s is not set", coord.ErrInvalidRequest, envStateDir)
	}
	client, err := herdr.NewClient(herdrBinary(env))
	if err != nil {
		return nil, func() {}, err
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open the orchestrator state: %w", err)
	}
	coordinator, err := coord.New(store, client, coord.Options{})
	if err != nil {
		_ = store.Close()
		return nil, func() {}, err
	}
	return coordinator, func() { _ = store.Close() }, nil
}

// bridgeOps wires the coordinator's dispatch operation into the bridge, which
// authorizes the caller before calling it.
func bridgeOps(env map[string]string) (bridge.Ops, func(), error) {
	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		cause := err
		return bridge.Ops{
			Dispatch: func(context.Context, bridge.Principal, json.RawMessage) (any, error) {
				return nil, &bridge.Error{Code: "herdr_unavailable", Message: "the coordinator is unavailable: " + cause.Error()}
			},
		}, func() {}, err
	}
	ops := bridge.Ops{
		Dispatch: func(ctx context.Context, principal bridge.Principal, params json.RawMessage) (any, error) {
			var request coord.DispatchParams
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, &bridge.Error{Code: "bad_params", Message: "the dispatch request is not a JSON object"}
			}
			result, err := coordinator.DispatchFor(ctx, principal.ProjectKey, request)
			if err != nil {
				return nil, &bridge.Error{Code: coordCode(err), Message: err.Error()}
			}
			return result, nil
		},
	}
	return ops, closeCoordinator, nil
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
	case errors.Is(err, coord.ErrHerdrUnavailable), errors.Is(err, herdr.ErrCLI),
		errors.Is(err, herdr.ErrTimeout), errors.Is(err, herdr.ErrAmbiguous):
		return "herdr_unavailable"
	default:
		return "internal"
	}
}

// runSetup installs the plugin's OMP extension and reports the outcome. The
// executable path it embeds is this binary, resolved at runtime, so a moved or
// reinstalled plugin refreshes the embedded path instead of leaving an
// extension that points at a stale executable.
func runSetup(env map[string]string, stdout, stderr io.Writer) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: setup: %v\n", err)
		return emit(stdout, stderr, failureBody("executable_unavailable", "the plugin executable path could not be resolved"))
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
