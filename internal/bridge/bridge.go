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
	"slices"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/coord"
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
	// The worker's own reports. They describe facts and never complete a task.
	MethodProgress = "progress"
	MethodQuestion = "question"
	// The primary's decisions and the read-only previews they are shown first.
	MethodAnswer        = "answer"
	MethodDialogPreview = "dialog_preview"
	MethodApproveDialog = "approve_dialog"
	MethodReportPreview = "report_preview"
	MethodReview        = "review"
	MethodSettleInput   = "settle_input"
	// MethodBriefInspection reads the evidence a user decides an ambiguous brief
	// submission on. The op itself mutates nothing, but it is authorized like any
	// other primary call: a session that has not confirmed its handshake may not
	// read another task's terminal.
	MethodBriefInspection = "brief_inspection"
	// MethodAdjudicateBrief records the user's decision about an ambiguous brief
	// submission.
	MethodAdjudicateBrief = "adjudicate_brief"
	MethodCleanupPreview  = "cleanup_preview"
	MethodCleanup         = "cleanup"
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
	// CodeAmbiguousOperation reports an operation whose outcome could not be
	// identified: Herdr may already have written the input, so the caller must
	// inspect the worker before anything else reaches it. It is deliberately not
	// CodeHerdrUnavailable — that code says the CLI could not be used at all, and a
	// caller that read an ambiguous answer or approved keys that way would retype
	// input that may already be in the pane.
	CodeAmbiguousOperation = "ambiguous_operation"
	CodeInternal           = "internal"
	CodeResponseTooLarge   = "response_too_large"
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
// enforcement is never conditional on the wiring. Tasks 4 and 5 fill these in,
// and CoordinatorOps builds the whole set from one coordinator.
type Ops struct {
	Dispatch func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	// Worker role.
	Report func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	// BriefInspection and AdjudicateBrief are Task 4's brief-adjudication surface:
	// the primary shows the evidence the inspection returns, the user decides, and
	// the decision is recorded against that exact evidence.
	BriefInspection func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	AdjudicateBrief func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	Progress        func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	Question        func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	// Primary role. Previews are read-only but require the same identity proof.
	Answer         func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	DialogPreview  func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	ApproveDialog  func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	ReportPreview  func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	Review         func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	SettleInput    func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	CleanupPreview func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
	Cleanup        func(ctx context.Context, principal Principal, params json.RawMessage) (any, error)
}

// Options carries what the caller observed before reading the request.
type Options struct {
	// ArgvMethod is the method named on the command line, if any. It must agree
	// with the request body, which is the authority.
	ArgvMethod string
	Ops        Ops
}

// CoordinatorOps wires one coordinator's operations into the bridge. The bridge
// authorizes a caller before any of them runs, so each operation receives the
// verified principal and decodes only that method's own body: the project, the
// task and the pane all come from persisted facts, and a caller that names one in
// params changes nothing.
//
// Every coordinator failure is answered with a stable code, so the extension and
// the plugin log classify it the same way.
func CoordinatorOps(c *coord.Coordinator) Ops {
	if c == nil {
		return UnavailableOps(errors.New("no coordinator was opened"))
	}
	return Ops{
		Dispatch: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.DispatchParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.DispatchFor(ctx, p.ProjectKey, request)
			return result, coordError(err)
		},
		Report: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.ReportParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.FinalReport(ctx, p.LaunchPaneID, request.Report())
			return result, coordError(err)
		},
		Progress: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.ProgressParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.Progress(ctx, p.LaunchPaneID, request.Summary)
			return result, coordError(err)
		},
		Question: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.QuestionParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.Question(ctx, p.LaunchPaneID, request.Text)
			return result, coordError(err)
		},
		Answer: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.AnswerParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.Answer(ctx, p.LaunchPaneID, request.TaskID, request.Text)
			return result, coordError(err)
		},
		DialogPreview: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.CleanupParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.DialogPreview(ctx, p.LaunchPaneID, request.TaskID)
			return result, coordError(err)
		},
		ApproveDialog: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.ApproveDialogParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.ApproveDialog(ctx, p.LaunchPaneID, request.TaskID, request.ObservedDigest, request.Keys)
			return result, coordError(err)
		},
		BriefInspection: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.BriefInspectionParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.BriefInspectionFor(ctx, p.ProjectKey, request.TaskID)
			return result, coordError(err)
		},
		AdjudicateBrief: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.AdjudicateBriefParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.AdjudicateBriefFor(ctx, p.ProjectKey, request.TaskID, request.ExpectedRevision,
				coord.BriefDecision(request.Decision), coord.BriefEvidence{PaneID: request.PaneID, Digest: request.Digest})
			return result, coordError(err)
		},
		ReportPreview: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.CleanupParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.ReportPreview(ctx, p.LaunchPaneID, request.TaskID)
			return result, coordError(err)
		},
		Review: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.ReviewParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.ReviewAtRevision(ctx, p.LaunchPaneID, request.TaskID, request.ExpectedRevision, request.Disposition)
			return result, coordError(err)
		},
		CleanupPreview: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.CleanupParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.CleanupPreview(ctx, p.LaunchPaneID, request.TaskID)
			return result, coordError(err)
		},
		SettleInput: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.SettleInputParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.SettleInput(ctx, p.LaunchPaneID, request.TaskID, request.Decision)
			return result, coordError(err)
		},
		Cleanup: func(ctx context.Context, p Principal, params json.RawMessage) (any, error) {
			var request coord.CleanupParams
			if err := decodeParams(params, &request); err != nil {
				return nil, err
			}
			result, err := c.Cleanup(ctx, p.LaunchPaneID, request.TaskID)
			return result, coordError(err)
		},
	}
}

// UnavailableOps answers every method with one code. A build whose coordinator
// could not be opened still authorizes its callers and then says exactly what is
// missing, instead of claiming a method is not implemented.
func UnavailableOps(cause error) Ops {
	if cause == nil {
		cause = errors.New("no reason was reported")
	}
	unavailable := func(context.Context, Principal, json.RawMessage) (any, error) {
		return nil, &Error{Code: CodeHerdrUnavailable, Message: "the coordinator is unavailable: " + cause.Error()}
	}
	return Ops{
		Dispatch:        unavailable,
		Report:          unavailable,
		Progress:        unavailable,
		Question:        unavailable,
		Answer:          unavailable,
		DialogPreview:   unavailable,
		ApproveDialog:   unavailable,
		BriefInspection: unavailable,
		AdjudicateBrief: unavailable,
		Review:          unavailable,
		ReportPreview:   unavailable,
		CleanupPreview:  unavailable,
		SettleInput:     unavailable,
		Cleanup:         unavailable,
	}
}

// decodeParams reads one method's body. A body that is not the object the method
// takes is refused with a specific code rather than being interpreted loosely.
func decodeParams(params json.RawMessage, into any) error {
	if trimmed := strings.TrimSpace(string(params)); trimmed == "" || trimmed == "null" {
		params = json.RawMessage("{}")
	}
	if err := json.Unmarshal(params, into); err != nil {
		return fail(CodeBadParams, "the request body is not the JSON object this method takes")
	}
	return nil
}

// coordError turns one coordinator failure into a stable code. It mirrors the
// executable's own mapping for its bind route, so the same failure classifies the
// same way whether it arrived through a tool call or a plugin action.
func coordError(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: coordErrorCode(err), Message: err.Error()}
}

func coordErrorCode(err error) string {
	switch {
	case errors.Is(err, coord.ErrOwnership):
		return CodeForbidden
	case errors.Is(err, coord.ErrInvalidRequest):
		return "bad_request"
	case errors.Is(err, coord.ErrReportInvalid):
		return "invalid_report"
	case errors.Is(err, coord.ErrNotReviewed):
		return "not_reviewed"
	case errors.Is(err, coord.ErrReportChanged):
		return "report_changed"
	case errors.Is(err, coord.ErrAlreadyDecided):
		return "already_decided"
	case errors.Is(err, coord.ErrWorkerLive):
		return "worker_live"
	case errors.Is(err, coord.ErrCheckoutDirty):
		return "checkout_dirty"
	case errors.Is(err, coord.ErrProvenance):
		return "provenance_unconfirmed"
	case errors.Is(err, coord.ErrDialogChanged):
		return "dialog_changed"
	case errors.Is(err, coord.ErrNotBlocked):
		return "not_blocked"
	case errors.Is(err, coord.ErrInputPending):
		return "input_pending"
	case errors.Is(err, coord.ErrBlocked):
		return "blocked"
	case errors.Is(err, coord.ErrNotAdjudicable):
		return "not_adjudicable"
	case errors.Is(err, coord.ErrEvidenceChanged):
		return "evidence_changed"
	case errors.Is(err, coord.ErrStepInFlight):
		return "step_in_flight"
	case errors.Is(err, coord.ErrAgentChanged):
		return "agent_changed"
	case errors.Is(err, coord.ErrProjectUnknown):
		return "project_unknown"
	case errors.Is(err, coord.ErrPrimaryNotLive):
		return "primary_not_live"
	case errors.Is(err, coord.ErrDifferentRepository):
		return "different_repository"
	case errors.Is(err, coord.ErrNotGitRepository):
		return "not_git_repository"
	case errors.Is(err, coord.ErrBaseRevision):
		return "bad_base_revision"
	case errors.Is(err, coord.ErrReservedConflict):
		return "reserved_conflict"
	case errors.Is(err, coord.ErrSessionUnavailable):
		return "session_unavailable"
	case errors.Is(err, coord.ErrSessionRefUnsupported):
		return "session_reference_unsupported"
	case errors.Is(err, coord.ErrReadinessTimeout):
		return "readiness_timeout"
	case errors.Is(err, coord.ErrAmbiguousStep):
		return "ambiguous_step"
	case errors.Is(err, coord.ErrPersistence):
		return "state_write_failed"
	case errors.Is(err, herdr.ErrAmbiguous):
		// Herdr may already have taken this operation's side effect — an answer or
		// approved keys written into the worker's pane — so the caller is told that
		// exactly, instead of being told the CLI was unusable.
		return CodeAmbiguousOperation
	case errors.Is(err, coord.ErrHerdrUnavailable), errors.Is(err, herdr.ErrCLI),
		errors.Is(err, herdr.ErrTimeout):
		return CodeHerdrUnavailable
	default:
		return CodeInternal
	}
}

// request is the v1 request body. Unknown fields are tolerated so a later
// version can add some without breaking v1 readers; the fields below are
// required.
type request struct {
	Version          int             `json:"version"`
	Method           string          `json:"method"`
	OMPSessionID     string          `json:"ompSessionID"`
	OMPSessionFile   string          `json:"ompSessionFile"`
	OMPSessionOnDisk bool            `json:"ompSessionOnDisk"`
	Params           json.RawMessage `json:"params"`
}

// announced is the identity the caller reports. The extension fills all of it in
// from its own session manager: the id, the session file it allocated, and whether
// that file has been materialized. Params are never authority for identity, and at
// least one identity must be present, because a session that announces nothing
// cannot be verified.
func (r request) announced() (task.Announcement, error) {
	announced, ok := task.NewAnnouncement(r.OMPSessionID, r.OMPSessionFile, r.OMPSessionOnDisk)
	if !ok {
		return task.Announcement{}, fail(CodeBadRequest, "the request carries no OMP session identity")
	}
	return announced, nil
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

	announced, err := req.announced()
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
		return h.resolve(announced)
	case MethodReady:
		return h.ready(announced)
	case MethodDispatch:
		return h.mutate(req, state.RolePrimary, announced, h.ops.Dispatch)
	case MethodReport:
		return h.mutate(req, state.RoleWorker, announced, h.ops.Report)
	case MethodProgress:
		return h.mutate(req, state.RoleWorker, announced, h.ops.Progress)
	case MethodQuestion:
		return h.mutate(req, state.RoleWorker, announced, h.ops.Question)
	case MethodAnswer:
		return h.mutate(req, state.RolePrimary, announced, h.ops.Answer)
	case MethodDialogPreview:
		return h.mutate(req, state.RolePrimary, announced, h.ops.DialogPreview)
	case MethodApproveDialog:
		return h.mutate(req, state.RolePrimary, announced, h.ops.ApproveDialog)
	case MethodReportPreview:
		return h.mutate(req, state.RolePrimary, announced, h.ops.ReportPreview)
	case MethodReview:
		return h.mutate(req, state.RolePrimary, announced, h.ops.Review)
	case MethodCleanupPreview:
		return h.mutate(req, state.RolePrimary, announced, h.ops.CleanupPreview)
	case MethodBriefInspection:
		return h.mutate(req, state.RolePrimary, announced, h.ops.BriefInspection)
	case MethodAdjudicateBrief:
		return h.mutate(req, state.RolePrimary, announced, h.ops.AdjudicateBrief)
	case MethodSettleInput:
		return h.mutate(req, state.RolePrimary, announced, h.ops.SettleInput)
	case MethodCleanup:
		return h.mutate(req, state.RolePrimary, announced, h.ops.Cleanup)
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
	req.OMPSessionFile = strings.TrimSpace(req.OMPSessionFile)
	if req.OMPSessionID == "" && req.OMPSessionFile == "" {
		return request{}, fail(CodeBadRequest, "the request carries no OMP session identity")
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
func (h *handler) resolve(announced task.Announcement) (any, error) {
	binding, err := h.store.ByOMPSession(h.ctx, announced)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// A process the coordinator has already keyed to this launch pane but
		// whose session it could not know yet is exactly a fresh worker or
		// primary: Herdr starts the agent, the extension announces its session,
		// and only then can the coordinator read it back from `agent get`.
		adopted, adoptErr := h.adoptAnnouncedSession(announced)
		switch {
		case errors.Is(adoptErr, state.ErrNotFound):
			return ResolveResult{Bound: false, Reason: ReasonNotBound, Protocol: Protocol}, nil
		case errors.Is(adoptErr, errSessionMismatch):
			attention := fmt.Sprintf("launch pane %q records %s but this process announces %s; the recorded session is kept and the orchestrator tools stay inactive until a human decides",
				h.launchPaneID, sessionOfBinding(adopted), announced.Describe())
			if noteErr := h.recordAttention(adopted, attention); noteErr != nil {
				return nil, fail(CodeStoreUnavailable, "the session mismatch could not be recorded as attention")
			}
			return ResolveResult{
				Bound:     false,
				Reason:    ReasonUnverifiedRestore,
				Attention: attention,
				Protocol:  Protocol,
			}, nil
		case adoptErr != nil:
			return nil, adoptErr
		default:
			return boundResult(adopted, true), nil
		}
	case errors.Is(err, state.ErrAmbiguous):
		return ResolveResult{
			Bound:     false,
			Reason:    ReasonAmbiguousSession,
			Attention: fmt.Sprintf("announced session %s matches more than one recorded project or task; a human must resolve which one owns it before its tools can activate", announced.Describe()),
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
		attention := fmt.Sprintf("session %s is recorded on launch pane %q but this process runs in %q, and Herdr could not verify the terminal/checkout provenance: %s. The orchestrator tools stay inactive and this is retried before the next turn",
			announced.Describe(), bindingLaunchPane(binding), h.launchPaneID, err)
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
		attention := fmt.Sprintf("session %s changed while its launch pane was being rebound; the binding will be re-verified before the next turn", announced.Describe())
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

// errSessionMismatch reports a launch pane whose record already holds a
// different OMP session than the one this process announces. The recorded fact
// is never overwritten: a human decides which process owns the pane.
var errSessionMismatch = errors.New("bridge: the recorded session differs from the announced one")

// sessionOfBinding reads the session a binding records, whatever its role.
func sessionOfBinding(b state.Binding) string {
	if b.Role == state.RolePrimary {
		return b.Project.OMPSessionID
	}
	return b.Task.OMPSessionID
}

// adoptAnnouncedSession binds an announced OMP session to the record Herdr
// already keys to this process's launch pane, and only while that record holds
// no session yet.
//
// This is the provisioning half of resolve: the coordinator records a task or
// project with its launch pane *before* it starts the agent (the pane identity
// is what makes the binding addressable), so the announced session is the first
// thing the bridge can verify against `agent get`. Adoption is deliberately
// provisional — it writes the session and nothing else. The handshake stays
// unverified until the coordinator's ConfirmReady matches `agent get`, and every
// mutating method keeps requiring that confirmation.
//
// Unknown pane reports state.ErrNotFound; a record that already holds a
// different session reports errSessionMismatch and is left untouched.
func (h *handler) adoptAnnouncedSession(announced task.Announcement) (state.Binding, error) {
	worker, err := h.store.ByLaunchPane(h.ctx, h.launchPaneID)
	switch {
	case err == nil:
		return h.adoptTaskSession(worker, announced)
	case !errors.Is(err, state.ErrNotFound):
		return state.Binding{}, fail(CodeStoreUnavailable, "the recorded bindings could not be read")
	}

	primary, err := h.store.ProjectByLaunchPane(h.ctx, h.launchPaneID)
	switch {
	case err == nil:
		return h.adoptProjectSession(primary, announced)
	case errors.Is(err, state.ErrNotFound):
		return state.Binding{}, state.ErrNotFound
	default:
		return state.Binding{}, fail(CodeStoreUnavailable, "the recorded project could not be read")
	}
}

// adoptedIdentity is what adoption writes into a record that has no session yet:
// the announced id when the runtime exposes one, otherwise the announced session
// file. Either identity is later matched by the same rule the lookup uses, so the
// choice never makes a session unfindable.
func adoptedIdentity(announced task.Announcement) string {
	if announced.ID != "" {
		return announced.ID
	}
	return announced.File
}

// recordsAnnouncement reports whether a recorded identity is one of the announced
// ones, compared in the form each kind implies.
func recordsAnnouncement(recorded string, announced task.Announcement) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return false
	}
	if announced.ID != "" && task.SameSessionReference(task.SessionKindID, announced.ID, recorded) {
		return true
	}
	return announced.File != "" && task.SameSessionReference(task.SessionKindPath, announced.File, recorded)
}

// adoptTaskSession records an announced session on the worker task bound to this
// pane, or reports the mismatch without writing.
func (h *handler) adoptTaskSession(worker task.Task, announced task.Announcement) (state.Binding, error) {
	binding := func(t task.Task) state.Binding {
		return state.Binding{Role: state.RoleWorker, Task: t}
	}
	if recorded := strings.TrimSpace(worker.OMPSessionID); recorded != "" {
		if recordsAnnouncement(recorded, announced) {
			return binding(worker), nil
		}
		return binding(worker), errSessionMismatch
	}
	identity := adoptedIdentity(announced)
	updated, err := h.store.UpdateTask(h.ctx, worker.ID, worker.Revision, func(next *task.Task) error {
		if recorded := strings.TrimSpace(next.OMPSessionID); recorded != "" {
			if recordsAnnouncement(recorded, announced) {
				return nil
			}
			return errSessionMismatch
		}
		next.OMPSessionID = identity
		return nil
	})
	if err == nil {
		return binding(updated), nil
	}
	// A compare-and-swap conflict means another call changed the task first.
	// Re-read and accept only the same announcement; a recorded session is never
	// overwritten.
	if errors.Is(err, state.ErrConflict) || errors.Is(err, errSessionMismatch) {
		current, readErr := h.store.Task(h.ctx, worker.ID)
		if readErr != nil {
			return state.Binding{}, fail(CodeStoreUnavailable, "the recorded task could not be read")
		}
		if recordsAnnouncement(current.OMPSessionID, announced) {
			return binding(current), nil
		}
		return binding(current), errSessionMismatch
	}
	return state.Binding{}, fail(CodeStoreUnavailable, "the announced session could not be recorded")
}

// adoptProjectSession is adoptTaskSession for a primary project.
func (h *handler) adoptProjectSession(primary task.Project, announced task.Announcement) (state.Binding, error) {
	binding := func(p task.Project) state.Binding {
		return state.Binding{Role: state.RolePrimary, Project: p}
	}
	if recorded := strings.TrimSpace(primary.OMPSessionID); recorded != "" {
		if recordsAnnouncement(recorded, announced) {
			return binding(primary), nil
		}
		return binding(primary), errSessionMismatch
	}
	identity := adoptedIdentity(announced)
	err := h.store.UpdateProject(h.ctx, primary.Key, func(next *task.Project) error {
		if next.LaunchPaneID != primary.LaunchPaneID {
			return errSessionMismatch
		}
		if recorded := strings.TrimSpace(next.OMPSessionID); recorded != "" {
			if recordsAnnouncement(recorded, announced) {
				return nil
			}
			return errSessionMismatch
		}
		next.OMPSessionID = identity
		return nil
	})
	if err == nil {
		primary.OMPSessionID = identity
		return binding(primary), nil
	}
	if errors.Is(err, errSessionMismatch) {
		current, readErr := h.store.Project(h.ctx, primary.Key)
		if readErr != nil {
			return state.Binding{}, fail(CodeStoreUnavailable, "the recorded project could not be read")
		}
		if recordsAnnouncement(current.OMPSessionID, announced) {
			return binding(current), nil
		}
		return binding(current), errSessionMismatch
	}
	return state.Binding{}, fail(CodeStoreUnavailable, "the announced session could not be recorded")
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
	if expected := bindingWorkspaces(binding); len(expected) > 0 && !slices.Contains(expected, pane.WorkspaceID) {
		return nil, fmt.Errorf("pane %q is in workspace %q, not the recorded workspace %s",
			h.launchPaneID, pane.WorkspaceID, quotedAlternatives(expected))
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
				// Another bridge call rebound this primary while this one verified its
				// pane. The conflict is reported as the store's own conflict, not as an
				// internal failure: resolve turns it into the recoverable unverified
				// restore (the tools stay inactive, the concurrent write stands, and the
				// failure is recorded for the next turn) instead of a dead end.
				return fmt.Errorf("%w: the primary binding changed while it was being verified", state.ErrConflict)
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
func (h *handler) ready(announced task.Announcement) (any, error) {
	binding, err := h.authorize(announced)
	if err != nil {
		return nil, err
	}
	if err := h.store.RecordReady(h.ctx, h.launchPaneID, announced, Protocol); err != nil {
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

// mutate authorizes one method and hands it to its coordinator operation: the
// caller's binding must be this process's, the role must be exactly the one the
// method requires, and the coordinator must already have confirmed this session.
// Authorization is repeated for every call, so a call is never legal because an
// earlier one was.
//
// The two read-only previews are authorized by the same function: a preview needs
// the same proof of role and identity as a mutation, and the coordinator
// operation behind it performs no write.
func (h *handler) mutate(req request, want state.Role, announced task.Announcement, op func(context.Context, Principal, json.RawMessage) (any, error)) (any, error) {
	binding, err := h.authorize(announced)
	if err != nil {
		return nil, err
	}
	if binding.Role != want {
		return nil, fail(CodeForbidden, "%s requires a %s session, and this is a %s session", req.Method, want, binding.Role)
	}
	ready, err := h.store.Ready(h.ctx, h.launchPaneID, announced)
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
func (h *handler) authorize(announced task.Announcement) (state.Binding, error) {
	binding, err := h.store.ByOMPSession(h.ctx, announced)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return state.Binding{}, fail(CodeNotBound, "no recorded project or task owns the announced session %s", announced.Describe())
	case errors.Is(err, state.ErrAmbiguous):
		return state.Binding{}, fail(CodeAmbiguousSession, "the announced session %s matches more than one recorded project or task", announced.Describe())
	case err != nil:
		return state.Binding{}, fail(CodeStoreUnavailable, "the recorded bindings could not be read")
	}
	recorded := bindingLaunchPane(binding)
	if recorded != h.launchPaneID {
		return state.Binding{}, fail(CodeLaunchPaneMismatch,
			"the announced session %s is bound to launch pane %q, not this process's launch pane %q", announced.Describe(), recorded, h.launchPaneID)
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

// bindingWorkspaces is the set of workspaces the recorded process may
// legitimately be in. A primary has only the workspace it currently runs in. A
// moved worker keeps its worktree workspace as provenance while its current
// workspace records where it was moved to, so a restore in either one is still
// the same worker; a restore anywhere else is refused.
func bindingWorkspaces(b state.Binding) []string {
	if b.Role == state.RolePrimary {
		return nonEmptyIDs(b.Project.CurrentWorkspaceID)
	}
	return nonEmptyIDs(b.Task.WorktreeWorkspaceID, b.Task.CurrentWorkspaceID)
}

// nonEmptyIDs drops empty ids — an unrecorded workspace means "unknown", not a
// workspace the pane must be in — and duplicates of one already accepted.
func nonEmptyIDs(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// quotedAlternatives renders accepted ids for a message that has to name
// everything that would have been accepted.
func quotedAlternatives(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, fmt.Sprintf("%q", id))
	}
	return strings.Join(quoted, " or ")
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
