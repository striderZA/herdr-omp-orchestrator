// Package bridge is the versioned stdin/stdout seam between the OMP extension
// and this plugin's Go core.
//
// The extension speaks exactly one protocol version. Every request names the
// method, the caller's OMP session id and a params object; every answer is
// either {"ok":true,"result":…} or {"ok":false,"error":{"code":…,"message":…}}
// written to stdout, so the extension never has to interpret an exit status or
// a diagnostic stream.
//
// # Identity
//
// The caller's identity comes from the environment the bridge process inherited
// from the OMP pane (HERDR_ENV=1 and the launch-time HERDR_PANE_ID) plus the OMP
// session id in the request body. Nothing in params is authority: a pane id a
// caller supplies is ignored, so a session cannot promote itself by naming
// another agent's pane.
//
// # Read-only resolve, verified rebind
//
// `resolve` is the only method that runs before a role is known, and it is
// read-only apart from one verified case. It finds the project or task that
// owns the caller's OMP session and requires that record's launch pane to be
// this process's inherited pane. When Herdr restarted the agent process the
// launch pane differs; resolve then asks Herdr for that pane's authoritative
// projection and rebinds only when the session, the checkout and the workspace
// provenance all match. Anything less keeps the tools inactive, records
// attention and notifies the primary: never a silent, permanent unbind.
//
// `ready` records the announced session as unverified. Mutating methods require
// the coordinator to have confirmed that exact session against `agent get`, the
// exact role the method needs, and a matching launch pane — re-checked on every
// single call.
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// Protocol is the request/response protocol version the extension and this
// bridge agree on. A request for any other version is refused instead of being
// interpreted approximately.
const Protocol = 1

// Bounds: one request is read with a hard limit, and a response that would
// exceed the response bound is replaced by an error rather than truncated.
const (
	MaxRequestBytes  = 1 << 20
	MaxResponseBytes = 1 << 20
)

// Methods.
const (
	MethodResolve  = "resolve"
	MethodReady    = "ready"
	MethodDispatch = "dispatch"
	MethodReport   = "report"
)

// Reasons a resolve result is not bound. The extension only needs `bound`; the
// reason and attention text make the failure legible to a human.
const (
	ReasonNotBound          = "not_bound"
	ReasonAmbiguousSession  = "ambiguous_session"
	ReasonUnverifiedRestore = "unverified_restore"
)

// Stable error codes. Callers and tests classify by code, never by message.
const (
	CodeBadRequest          = "bad_request"
	CodeRequestVersion      = "bad_request_version"
	CodeRequestTooLarge     = "request_too_large"
	CodeBadParams           = "bad_params"
	CodeMethodMismatch      = "method_mismatch"
	CodeUnsupportedMethod   = "unsupported_method"
	CodeNoHerdrContext      = "no_herdr_context"
	CodeStateDirUnavailable = "state_dir_unavailable"
	CodeStoreUnavailable    = "store_unavailable"
	CodeNotBound            = "not_bound"
	CodeAmbiguousSession    = "ambiguous_session"
	CodeLaunchPaneMismatch  = "launch_pane_mismatch"
	CodeNotReady            = "not_ready"
	CodeForbidden           = "forbidden"
	CodeNotImplemented      = "not_implemented"
	CodeHerdrUnavailable    = "herdr_unavailable"
	CodeInternal            = "internal"
	CodeResponseTooLarge    = "response_too_large"
)

// Environment the bridge reads. HERDR_ORCH_HERDR_BIN_PATH is set by the
// extension on every child because an ordinary Herdr-managed OMP pane is not
// guaranteed to inherit plugin-command environment; HERDR_BIN_PATH is accepted
// as the fallback for a bridge started from a plugin command.
const (
	EnvHerdr          = "HERDR_ENV"
	EnvLaunchPaneID   = "HERDR_PANE_ID"
	EnvStateDir       = "HERDR_PLUGIN_STATE_DIR"
	EnvHerdrBinary    = "HERDR_ORCH_HERDR_BIN_PATH"
	EnvHerdrBinaryAlt = "HERDR_BIN_PATH"
)

// herdrTimeout bounds one Herdr CLI call made during restore verification.
const herdrTimeout = 15 * time.Second

// Error is a failure with a stable code. Coordinator operations wired through
// Ops return it to answer the extension with a specific code instead of the
// generic internal error.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Principal is the verified identity of the caller. Coordinator operations
// receive it instead of the raw request, so they never re-derive identity from
// caller-supplied data.
type Principal struct {
	Role                state.Role
	Project             task.Project
	Task                *task.Task
	ProjectKey          string
	TaskID              string
	LaunchPaneID        string
	PublicPaneID        string
	TerminalID          string
	WorktreePath        string
	WorktreeWorkspaceID string
}

// Ops are the coordinator operations the bridge authorizes before calling. A
// nil field means that operation is not wired yet: the bridge answers
// `not_implemented` only *after* the caller has been fully authorized, so role
// enforcement is never conditional on the wiring. Tasks 4 and 5 fill these in.
type Ops struct {
	Dispatch func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	Report   func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
}

// Options carries what the caller observed before reading the request.
type Options struct {
	// ArgvMethod is the method named on the command line, if any. It must agree
	// with the request body, which is the authority.
	ArgvMethod string
	Ops        Ops
}

// request is the v1 request body. Unknown fields are tolerated so a later
// version can add some without breaking v1 readers; the fields below are
// required.
type request struct {
	Version      int             `json:"version"`
	Method       string          `json:"method"`
	OMPSessionID string          `json:"ompSessionID"`
	Params       json.RawMessage `json:"params"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *errorBody      `json:"error,omitempty"`
}

// Handle answers one request with the caller's environment. It is the entry
// point the executable uses for `bridge <method>`; ops are supplied separately
// by HandleWith.
func Handle(ctx context.Context, env map[string]string, input io.Reader) ([]byte, error) {
	return Run(ctx, env, input, Options{})
}

// HandleWith answers one request with coordinator operations wired in.
func HandleWith(ctx context.Context, env map[string]string, input io.Reader, ops Ops) ([]byte, error) {
	return Run(ctx, env, input, Options{Ops: ops})
}

// Run answers one request, checking an optional command-line method against the
// request body.
func Run(ctx context.Context, env map[string]string, input io.Reader, opts Options) ([]byte, error) {
	result, failure := run(ctx, env, input, opts)
	return encode(result, failure)
}

// run performs the request and reports either a result value or a failure. An
// I/O failure that prevents any answer at all is reported as a failure too, so
// the extension always receives a protocol response when stdout is writable.
func run(ctx context.Context, env map[string]string, input io.Reader, opts Options) (any, error) {
	req, err := readRequest(input, opts.ArgvMethod)
	if err != nil {
		return nil, err
	}

	h := &handler{ctx: ctx, env: env, ops: opts.Ops}
	if err := h.prepare(); err != nil {
		return nil, err
	}
	defer func() { _ = h.store.Close() }()

	switch req.Method {
	case MethodResolve:
		return h.resolve(req.OMPSessionID)
	case MethodReady:
		return h.ready(req.OMPSessionID)
	case MethodDispatch:
		return h.mutate(req, state.RolePrimary, h.ops.Dispatch)
	case MethodReport:
		return h.mutate(req, state.RoleWorker, h.ops.Report)
	default:
		return nil, fail(CodeUnsupportedMethod, "unknown bridge method %q", req.Method)
	}
}

func encode(result any, failure error) ([]byte, error) {
	out := response{OK: failure == nil}
	if failure != nil {
		var coded *Error
		if errors.As(failure, &coded) {
			out.Error = &errorBody{Code: coded.Code, Message: coded.Message}
		} else {
			out.Error = &errorBody{Code: CodeInternal, Message: failure.Error()}
		}
	} else {
		encoded, err := json.Marshal(result)
		if err != nil {
			out.OK = false
			out.Result = nil
			out.Error = &errorBody{Code: CodeInternal, Message: "the bridge could not encode its result"}
		} else {
			out.Result = encoded
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes {
		data, err = json.Marshal(response{
			OK:    false,
			Error: &errorBody{Code: CodeResponseTooLarge, Message: fmt.Sprintf("the answer exceeds the %d byte bound", MaxResponseBytes)},
		})
		if err != nil {
			return nil, err
		}
	}
	return append(data, '\n'), nil
}

// readRequest bounds and decodes one request. The method and version are
// validated here so no handler has to.
func readRequest(input io.Reader, argvMethod string) (request, error) {
	if input == nil {
		return request{}, fail(CodeBadRequest, "no request was provided")
	}
	data, err := io.ReadAll(io.LimitReader(input, MaxRequestBytes+1))
	if err != nil {
		return request{}, fail(CodeBadRequest, "the request could not be read")
	}
	if len(data) > MaxRequestBytes {
		return request{}, fail(CodeRequestTooLarge, "the request exceeds the %d byte bound", MaxRequestBytes)
	}
	var req request
	if err := json.Unmarshal(data, &req); err != nil {
		return request{}, fail(CodeBadRequest, "the request is not a JSON object")
	}
	if req.Version != Protocol {
		return request{}, fail(CodeRequestVersion, "request version %d is not supported (this bridge speaks version %d)", req.Version, Protocol)
	}
	req.Method = strings.TrimSpace(req.Method)
	if req.Method == "" {
		return request{}, fail(CodeBadRequest, "the request names no method")
	}
	req.OMPSessionID = strings.TrimSpace(req.OMPSessionID)
	if req.OMPSessionID == "" {
		return request{}, fail(CodeBadRequest, "the request carries no OMP session id")
	}
	if len(req.Params) > 0 {
		trimmed := strings.TrimSpace(string(req.Params))
		if trimmed != "null" && !strings.HasPrefix(trimmed, "{") {
			return request{}, fail(CodeBadParams, "params must be a JSON object")
		}
	}
	if argvMethod != "" && argvMethod != req.Method {
		return request{}, fail(CodeMethodMismatch, "the command line asked for %q while the request body asked for %q", argvMethod, req.Method)
	}
	return req, nil
}

// handler carries one request's verified context.
type handler struct {
	ctx   context.Context
	env   map[string]string
	ops   Ops
	store *state.Store

	launchPaneID string
}

// prepare validates the inherited Herdr context and opens the store.
func (h *handler) prepare() error {
	if strings.TrimSpace(h.env[EnvHerdr]) != "1" {
		return fail(CodeNoHerdrContext, "this process is not a Herdr-managed pane (HERDR_ENV is not 1)")
	}
	h.launchPaneID = strings.TrimSpace(h.env[EnvLaunchPaneID])
	if h.launchPaneID == "" {
		return fail(CodeNoHerdrContext, "this process inherited no launch-time HERDR_PANE_ID")
	}
	dir := strings.TrimSpace(h.env[EnvStateDir])
	if dir == "" {
		return fail(CodeStateDirUnavailable, "%s is not set", EnvStateDir)
	}
	store, err := state.Open(dir)
	if err != nil {
		return fail(CodeStoreUnavailable, "the orchestrator state is unavailable")
	}
	h.store = store
	return nil
}

// ResolveResult is what resolve returns: either a bound role with its recorded
// identities, or an explicit reason it could not be proven. It is an exported
// wire contract because the generated extension and the tests read these names.
type ResolveResult struct {
	Bound        bool   `json:"bound"`
	Reason       string `json:"reason,omitempty"`
	Attention    string `json:"attention,omitempty"`
	Role         string `json:"role,omitempty"`
	ProjectKey   string `json:"projectKey,omitempty"`
	TaskID       string `json:"taskId,omitempty"`
	LaunchPaneID string `json:"launchPaneId,omitempty"`
	PublicPaneID string `json:"publicPaneId,omitempty"`
	TerminalID   string `json:"terminalId,omitempty"`
	WorktreeID   string `json:"worktreeId,omitempty"`
	Rebound      bool   `json:"rebound"`
	Protocol     int    `json:"protocol"`
}

// ReadyResult is the answer to `ready`: the handshake was recorded, still
// unverified until the coordinator confirms it.
type ReadyResult struct {
	Recorded     bool   `json:"recorded"`
	Role         string `json:"role"`
	LaunchPaneID string `json:"launchPaneId"`
	PublicPaneID string `json:"publicPaneId"`
	Protocol     int    `json:"protocol"`
}

// ---------------------------------------------------------------------------
// resolve
// ---------------------------------------------------------------------------

// resolve finds the binding that owns this OMP session and proves it belongs to
// this process. It never trusts params. The only mutation it can perform is the
// verified launch-pane rebind below.
func (h *handler) resolve(ompSessionID string) (any, error) {
	binding, err := h.store.ByOMPSession(h.ctx, ompSessionID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return ResolveResult{Bound: false, Reason: ReasonNotBound, Protocol: Protocol}, nil
	case errors.Is(err, state.ErrAmbiguous):
		return ResolveResult{
			Bound:     false,
			Reason:    ReasonAmbiguousSession,
			Attention: fmt.Sprintf("OMP session %q matches more than one recorded project or task; a human must resolve which one owns it before its tools can activate", ompSessionID),
			Protocol:  Protocol,
		}, nil
	case err != nil:
		return nil, fail(CodeStoreUnavailable, "the recorded bindings could not be read")
	}

	if bindingLaunchPane(binding) == h.launchPaneID {
		return boundResult(binding, false), nil
	}

	pane, err := h.verifyRestore(binding)
	if err != nil {
		attention := fmt.Sprintf("OMP session %q is recorded on launch pane %q but this process runs in %q, and Herdr could not verify the terminal/checkout provenance: %s. The orchestrator tools stay inactive and this is retried before the next turn",
			ompSessionID, bindingLaunchPane(binding), h.launchPaneID, err)
		if noteErr := h.recordAttention(binding, attention); noteErr != nil {
			return nil, fail(CodeStoreUnavailable, "the failed restore could not be recorded as attention")
		}
		return ResolveResult{
			Bound:     false,
			Reason:    ReasonUnverifiedRestore,
			Attention: attention,
			Protocol:  Protocol,
		}, nil
	}

	rebound, err := h.rebind(binding, pane)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return nil, coded
		}
		if !errors.Is(err, state.ErrConflict) {
			return nil, fail(CodeInternal, "the verified rebind could not be stored")
		}
		attention := fmt.Sprintf("OMP session %q changed while its launch pane was being rebound; the binding will be re-verified before the next turn", ompSessionID)
		if noteErr := h.recordAttention(binding, attention); noteErr != nil {
			return nil, fail(CodeStoreUnavailable, "the failed restore could not be recorded as attention")
		}
		return ResolveResult{
			Bound:     false,
			Reason:    ReasonUnverifiedRestore,
			Attention: attention,
			Protocol:  Protocol,
		}, nil
	}
	return boundResult(rebound, true), nil
}

// verifyRestore asks Herdr for this process's own pane and requires the
// authoritative projection to match the recorded session's provenance: the pane
// must run OMP, its working directory must be the recorded checkout, and its
// workspace must be one of the recorded workspaces.
func (h *handler) verifyRestore(binding state.Binding) (*herdr.PaneInfo, error) {
	binary := strings.TrimSpace(h.env[EnvHerdrBinary])
	if binary == "" {
		binary = strings.TrimSpace(h.env[EnvHerdrBinaryAlt])
	}
	if binary == "" {
		return nil, fail(CodeHerdrUnavailable, "no Herdr binary path was provided to the bridge")
	}
	client, err := herdr.NewClient(binary)
	if err != nil {
		return nil, fail(CodeHerdrUnavailable, "the Herdr CLI could not be used: %v", err)
	}

	ctx, cancel := context.WithTimeout(h.ctx, herdrTimeout)
	defer cancel()
	pane, err := client.PaneGet(ctx, h.launchPaneID)
	if err != nil {
		return nil, fmt.Errorf("Herdr could not read pane %q: %w", h.launchPaneID, err)
	}

	if !strings.EqualFold(strings.TrimSpace(pane.Agent), "omp") {
		return nil, fmt.Errorf("pane %q is not running OMP (%s reports agent %q)", h.launchPaneID, "Herdr", pane.Agent)
	}
	if checkout := bindingCheckout(binding); !samePath(pane.Cwd, checkout) {
		return nil, fmt.Errorf("pane %q works in %q, not the recorded checkout %q", h.launchPaneID, pane.Cwd, checkout)
	}
	if workspace := bindingWorkspace(binding); workspace != "" && pane.WorkspaceID != workspace {
		return nil, fmt.Errorf("pane %q is in workspace %q, not the recorded %q", h.launchPaneID, pane.WorkspaceID, workspace)
	}
	return pane, nil
}

// rebind stores the verified launch pane, public pane, terminal and workspace
// for an existing binding, keeping the worktree provenance it was dispatched
// with. It is a compare-and-swap: a binding that changed underneath is reported
// as a conflict and re-verified on the next turn.
func (h *handler) rebind(binding state.Binding, pane *herdr.PaneInfo) (state.Binding, error) {
	if binding.Role == state.RolePrimary {
		project := binding.Project
		err := h.store.UpdateProject(h.ctx, project.Key, func(next *task.Project) error {
			if next.LaunchPaneID != project.LaunchPaneID || next.PublicPaneID != project.PublicPaneID {
				return fail(CodeInternal, "the primary binding changed while it was being verified")
			}
			next.LaunchPaneID = h.launchPaneID
			next.PublicPaneID = pane.PaneID
			next.TerminalID = pane.TerminalID
			next.CurrentWorkspaceID = pane.WorkspaceID
			return nil
		})
		if err != nil {
			return state.Binding{}, err
		}
		project.LaunchPaneID = h.launchPaneID
		project.PublicPaneID = pane.PaneID
		project.TerminalID = pane.TerminalID
		project.CurrentWorkspaceID = pane.WorkspaceID
		return state.Binding{Role: state.RolePrimary, Project: project}, nil
	}

	t := binding.Task
	updated, err := h.store.UpdateTask(h.ctx, t.ID, t.Revision, func(next *task.Task) error {
		next.LaunchPaneID = h.launchPaneID
		next.PublicPaneID = pane.PaneID
		next.TerminalID = pane.TerminalID
		next.CurrentWorkspaceID = pane.WorkspaceID
		return nil
	})
	if err != nil {
		return state.Binding{}, err
	}
	return state.Binding{Role: state.RoleWorker, Task: updated}, nil
}

// recordAttention makes a failed restore durable and visible: the task carries
// the attention and the primary gets one deduplicated inbox note. Writing the
// same failure twice changes nothing. A write that fails is reported, because
// "the tools stayed inactive" must never be the only trace of a failed restore.
// A lost compare-and-swap race is not a failure: the inbox note below still
// carries the same message.
func (h *handler) recordAttention(binding state.Binding, message string) error {
	if binding.Role == state.RoleWorker {
		t := binding.Task
		if t.Attention != message {
			_, err := h.store.UpdateTask(h.ctx, t.ID, t.Revision, func(next *task.Task) error {
				next.Attention = message
				return nil
			})
			if err != nil && !errors.Is(err, state.ErrConflict) {
				return err
			}
		}
	}
	projectKey := binding.Project.Key
	if binding.Role == state.RoleWorker {
		projectKey = binding.Task.ProjectKey
	}
	if projectKey == "" {
		return nil
	}
	note := task.Inbox{
		ID:         attentionNoteID(bindingTaskID(binding), message),
		ProjectKey: projectKey,
		TaskID:     bindingTaskID(binding),
		Kind:       "attention",
		Summary:    message,
		Delivery:   "unverified restore",
	}
	if _, err := h.store.AddInbox(h.ctx, note); err != nil {
		return err
	}
	return nil
}

// attentionNoteID deduplicates the same failure while still notifying the
// primary about a different one.
func attentionNoteID(taskID, message string) string {
	sum := sha256.Sum256([]byte(message))
	return "attention:" + taskID + ":" + hex.EncodeToString(sum[:4])
}

// ---------------------------------------------------------------------------
// ready
// ---------------------------------------------------------------------------

// ready records the announced OMP session for a binding this process owns. The
// handshake stays unverified: only the coordinator's comparison with
// `agent get` may confirm it, and only then does any mutation become legal.
func (h *handler) ready(ompSessionID string) (any, error) {
	binding, err := h.authorize(ompSessionID)
	if err != nil {
		return nil, err
	}
	if err := h.store.RecordReady(h.ctx, h.launchPaneID, ompSessionID, Protocol); err != nil {
		return nil, fail(CodeStoreUnavailable, "the readiness handshake could not be recorded")
	}
	return ReadyResult{
		Recorded:     true,
		Role:         string(binding.Role),
		LaunchPaneID: h.launchPaneID,
		PublicPaneID: bindingPublicPane(binding),
		Protocol:     Protocol,
	}, nil
}

// ---------------------------------------------------------------------------
// mutating methods
// ---------------------------------------------------------------------------

// mutate authorizes one mutating method and hands it to its coordinator
// operation: the caller's binding must be this process's, the role must be
// exactly the one the method requires, and the coordinator must already have
// confirmed this session. Authorization is repeated for every call.
func (h *handler) mutate(req request, want state.Role, op func(context.Context, Principal, json.RawMessage) (any, error)) (any, error) {
	binding, err := h.authorize(req.OMPSessionID)
	if err != nil {
		return nil, err
	}
	if binding.Role != want {
		return nil, fail(CodeForbidden, "%s requires a %s session, and this is a %s session", req.Method, want, binding.Role)
	}
	ready, err := h.store.Ready(h.ctx, h.launchPaneID, req.OMPSessionID)
	if err != nil {
		return nil, fail(CodeStoreUnavailable, "the readiness handshake could not be read")
	}
	if !ready {
		return nil, fail(CodeNotReady, "%s is refused until the coordinator has confirmed this OMP session against `agent get`", req.Method)
	}
	if op == nil {
		return nil, fail(CodeNotImplemented, "%s is authorized but its coordinator operation is not wired into this build", req.Method)
	}

	params := req.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	value, err := op(h.ctx, principalOf(binding, h.launchPaneID), params)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return nil, coded
		}
		return nil, fail(CodeInternal, "%s failed", req.Method)
	}
	return value, nil
}

// authorize resolves the session and requires the recorded binding to be this
// process's own launch pane. It deliberately re-reads the store: a binding that
// was rebound or replaced since the last call is honored, not cached.
func (h *handler) authorize(ompSessionID string) (state.Binding, error) {
	binding, err := h.store.ByOMPSession(h.ctx, ompSessionID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return state.Binding{}, fail(CodeNotBound, "no recorded project or task owns OMP session %q", ompSessionID)
	case errors.Is(err, state.ErrAmbiguous):
		return state.Binding{}, fail(CodeAmbiguousSession, "OMP session %q matches more than one recorded project or task", ompSessionID)
	case err != nil:
		return state.Binding{}, fail(CodeStoreUnavailable, "the recorded bindings could not be read")
	}
	recorded := bindingLaunchPane(binding)
	if recorded != h.launchPaneID {
		return state.Binding{}, fail(CodeLaunchPaneMismatch,
			"OMP session %q is bound to launch pane %q, not this process's launch pane %q", ompSessionID, recorded, h.launchPaneID)
	}
	return binding, nil
}

// ---------------------------------------------------------------------------
// binding accessors
// ---------------------------------------------------------------------------

func bindingLaunchPane(b state.Binding) string {
	if b.Role == state.RolePrimary {
		return b.Project.LaunchPaneID
	}
	return b.Task.LaunchPaneID
}

func bindingPublicPane(b state.Binding) string {
	if b.Role == state.RolePrimary {
		return b.Project.PublicPaneID
	}
	return b.Task.PublicPaneID
}

func bindingTaskID(b state.Binding) string {
	if b.Role == state.RolePrimary {
		return ""
	}
	return b.Task.ID
}

// bindingCheckout is the recorded working directory this binding must be
// running in: the worktree checkout for a worker, the canonical repository root
// for a primary.
func bindingCheckout(b state.Binding) string {
	if b.Role == state.RolePrimary {
		return b.Project.Key
	}
	return b.Task.WorktreePath
}

// bindingWorkspace is the workspace the recorded process is expected to be in.
// A moved worker keeps its worktree workspace as provenance, so the current
// workspace is accepted too.
func bindingWorkspace(b state.Binding) string {
	if b.Role == state.RolePrimary {
		return b.Project.CurrentWorkspaceID
	}
	return b.Task.WorktreeWorkspaceID
}

func boundResult(b state.Binding, rebound bool) ResolveResult {
	result := ResolveResult{
		Bound:        true,
		Role:         string(b.Role),
		LaunchPaneID: bindingLaunchPane(b),
		PublicPaneID: bindingPublicPane(b),
		Rebound:      rebound,
		Protocol:     Protocol,
	}
	if b.Role == state.RolePrimary {
		result.ProjectKey = b.Project.Key
		result.TerminalID = b.Project.TerminalID
		return result
	}
	result.ProjectKey = b.Task.ProjectKey
	result.TaskID = b.Task.ID
	result.TerminalID = b.Task.TerminalID
	result.WorktreeID = b.Task.WorktreeWorkspaceID
	return result
}

func principalOf(b state.Binding, launchPaneID string) Principal {
	p := Principal{
		Role:         b.Role,
		LaunchPaneID: launchPaneID,
		PublicPaneID: bindingPublicPane(b),
	}
	if b.Role == state.RolePrimary {
		p.Project = b.Project
		p.ProjectKey = b.Project.Key
		p.TerminalID = b.Project.TerminalID
		if p.LaunchPaneID == "" {
			p.LaunchPaneID = b.Project.LaunchPaneID
		}
		return p
	}
	t := b.Task
	p.Task = &t
	p.ProjectKey = t.ProjectKey
	p.TaskID = t.ID
	p.TerminalID = t.TerminalID
	p.WorktreePath = t.WorktreePath
	p.WorktreeWorkspaceID = t.WorktreeWorkspaceID
	if p.LaunchPaneID == "" {
		p.LaunchPaneID = t.LaunchPaneID
	}
	return p
}

// samePath compares two recorded paths the way the platform does: Windows
// paths are case-insensitive and both spellings separate with / and \.
func samePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
