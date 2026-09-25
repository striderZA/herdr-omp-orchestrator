// Package herdr is the single boundary between the orchestrator and the Herdr
// command-line interface.
//
// The client executes Herdr's argv directly (never through a shell), decodes the
// JSON envelope Herdr prints, and returns typed values. It deliberately owns no
// coordination state: it never opens SQLite, never imports internal/state, and
// never decides whether a binding should change. The coordinator (Task 6) takes
// the decoded identities, validates the binding it holds, and updates the store.
//
// # Envelope
//
// Every Herdr command answers with one JSON object: {"id":..., "result":...} on
// success or {"error":{"code":...,"message":...},"id":...} on failure. Observed
// against Herdr 0.9.1-preview: the success envelope is the only thing on stdout;
// the error envelope is written to stderr with exit status 1, while argv-level
// failures (clap usage errors) print plain text to stderr with exit status 2.
// This client therefore reads both streams and is strict about the envelope
// itself: it rejects an unknown top-level field, a missing or contradictory
// result/error, a blank error code or message, a non-object result, trailing
// data after the envelope, and empty output. Being unable to interpret a
// response fails closed; it never guesses.
//
// Unknown fields *inside* a result body are tolerated on purpose. Real Herdr
// result bodies carry fields this client does not model (scroll, tokens,
// agent_session, target_layout, ...), so rejecting them would make the client
// reject every response the server actually sends. What is not tolerated is an
// unknown result "type" for the operation requested, or an absent required
// identity: both are reported as errors rather than decoded around.
//
// # Two pane identities
//
// A pane has a launch-time identity and a current public identity. The process
// Herdr started keeps its launch-time HERDR_PANE_ID forever; a cross-workspace
// move changes the public, workspace-qualified pane id and Herdr keeps the old
// value as an alias (PaneMove reports it as previous_pane_id, and PaneGet
// resolves an alias to the current public id). PaneMove exposes both, so a
// caller can update only the public identity and never re-key the launch id.
//
// # Ambiguity
//
// A timeout after input was written is not a failure. `agent prompt` writes the
// brief and then a delayed Enter, so a caller timeout (or Herdr's own `timeout`
// code) can arrive after the brief was already submitted, and
// `agent_prompt_stalled` means the write happened but no turn was observed.
// Those are returned as *AmbiguousError (with the underlying *CLIError still
// reachable through errors.As) so the coordinator inspects authoritative state
// instead of resubmitting. `agent_blocked` is the opposite: Herdr rejects it
// before sending any input, so it stays a plain *CLIError.
//
// A response this client cannot interpret leaves the outcome equally undecided:
// for a command whose side effect may already have happened (input written or
// topology mutated), a body with an unrecognized result type, a contradicted
// identity or a stream over the bound is reported as ambiguous too. A read-only
// command reports the same body as a definite decoding failure.
//
// # Bounds
//
// Each stream is bounded to MaxOutputBytes. The bound is enforced by a writer
// that returns an error instead of truncating, so oversized output fails closed
// with *OutputLimitError rather than silently yielding a partial response.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MaxOutputBytes bounds stdout and stderr independently for one CLI invocation.
// It is deliberately identical for both streams: the response must fit, and a
// diagnostic that does not fit is still a response we cannot trust.
const MaxOutputBytes = 1 << 20

// Herdr error codes this package classifies explicitly. They are the documented
// values from the CLI reference and `herdr agent prompt --help`.
const (
	// CodeAgentBlocked is returned by `agent prompt` when the agent is already
	// blocked; no input is sent.
	CodeAgentBlocked = "agent_blocked"
	// CodeAgentPromptStalled is returned when a submission was accepted but no
	// working or blocked state was observed within Herdr's stall window.
	CodeAgentPromptStalled = "agent_prompt_stalled"
	// CodeTimeout is returned when the caller's own timeout expired.
	CodeTimeout = "timeout"
)

// Sentinel errors, so callers can classify without string matching.
var (
	// ErrInvalidArgument reports arguments rejected before any process started.
	ErrInvalidArgument = errors.New("herdr: invalid argument")
	// ErrOutputTooLarge reports a stream that exceeded MaxOutputBytes.
	ErrOutputTooLarge = errors.New("herdr: output exceeded the size limit")
	// ErrMalformedResponse reports a response this client cannot interpret.
	ErrMalformedResponse = errors.New("herdr: malformed response")
	// ErrIdentity reports a response whose identity is missing or contradicts
	// the request, so no binding may be derived from it.
	ErrIdentity = errors.New("herdr: response identity is missing or contradictory")
	// ErrTimeout reports that the caller context expired first.
	ErrTimeout = errors.New("herdr: command timed out")
	// ErrAmbiguous reports an outcome that may already have taken effect.
	ErrAmbiguous = errors.New("herdr: command outcome is ambiguous")
	// ErrCLI reports any error envelope returned by the Herdr CLI.
	ErrCLI = errors.New("herdr: CLI returned an error")
)

var agentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

var validReadSources = map[string]bool{
	"visible":          true,
	"recent":           true,
	"recent-unwrapped": true,
	"detection":        true,
}

// Client executes one Herdr binary. It holds no other state: every call is a
// fresh process, and the caller owns the context deadline.
type Client struct {
	binary string
}

// NewClient returns a client that runs binary. It does not check that the binary
// exists; a missing or unusable binary surfaces on the first call.
func NewClient(binary string) (*Client, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, fmt.Errorf("%w: herdr binary path is empty", ErrInvalidArgument)
	}
	return &Client{binary: binary}, nil
}

// Binary reports the configured executable path.
func (c *Client) Binary() string { return c.binary }

// Call runs the binary with args and returns the raw JSON "result" member of a
// successful response. args are passed verbatim as argv; they are never joined,
// quoted, or handed to a shell. Call applies no ambiguity classification, so
// prefer the typed methods for anything whose timeout could be meaningful.
func (c *Client) Call(ctx context.Context, args ...string) (json.RawMessage, error) {
	return c.call(ctx, strings.Join(args, " "), args, timeoutDefinite)
}

// ---------------------------------------------------------------------------
// process execution
// ---------------------------------------------------------------------------

const (
	streamStdout = "stdout"
	streamStderr = "stderr"
)

// callResult is the raw outcome of one invocation, kept so error values can
// carry the argv, exit status and both streams for diagnosis.
type callResult struct {
	op       string
	args     []string
	stdout   []byte
	stderr   []byte
	exitCode int
}

// run executes the binary once. It reports transport-level failures only: a
// non-zero exit status is not an error here, because the caller decides whether
// an exit status is fatal (typical commands) or the payload (nothing yet).
func (c *Client) run(ctx context.Context, op string, args []string) (*callResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context for %s", ErrInvalidArgument, op)
	}
	// The only process-execution call in this package: argv goes straight to
	// the executable. There is deliberately no sh -c / cmd /c path anywhere.
	cmd := exec.CommandContext(ctx, c.binary, args...)
	stdout := &boundedBuffer{limit: MaxOutputBytes, stream: streamStdout}
	stderr := &boundedBuffer{limit: MaxOutputBytes, stream: streamStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	res := &callResult{
		op:     op,
		args:   append([]string(nil), args...),
		stdout: stdout.Bytes(),
		stderr: stderr.Bytes(),
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, &TimeoutError{Op: op, Args: res.args, Cause: ctxErr}
	}
	// Bound violations first: a truncated stream cannot be interpreted, so
	// nothing derived from it may be reported as a result or a Herdr error.
	if stdout.err != nil {
		return res, stdout.err
	}
	if stderr.err != nil {
		return res, stderr.err
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return res, fmt.Errorf("herdr %s: start %q: %w", op, c.binary, runErr)
		}
		res.exitCode = exitErr.ExitCode()
	}
	return res, nil
}

func (c *Client) call(ctx context.Context, op string, args []string, semantics timeoutSemantics) (json.RawMessage, error) {
	res, err := c.run(ctx, op, args)
	if err != nil {
		return nil, c.classify(op, semantics, err)
	}
	raw, err := decodeCallResult(res)
	if err != nil {
		return nil, c.classify(op, semantics, err)
	}
	return raw, nil
}

// classify turns a failure into the classification callers act on. Ambiguity is
// decided here, once, from the command's semantics: every typed method whose
// side effect may already have happened runs its whole response interpretation
// through this function, so an uninterpretable response is classified exactly
// like a timeout instead of surfacing as a definite failure.
func (c *Client) classify(op string, semantics timeoutSemantics, err error) error {
	if err == nil || semantics == timeoutDefinite {
		return err
	}
	// Ambiguity is decided once. A typed method re-runs classification over an
	// error the transport path already classified, so an existing
	// *AmbiguousError must not be wrapped again.
	var ambiguous *AmbiguousError
	if errors.As(err, &ambiguous) {
		return err
	}
	if mayHaveTakenEffect(err, semantics) {
		reason := ambiguityReason(semantics)
		if isUninterpretableResponse(err) {
			reason = ambiguityReasonUninterpretable
		}
		return &AmbiguousError{
			Op:     op,
			Reason: reason,
			Err:    err,
		}
	}
	return err
}

// decodeCallResult interprets one finished invocation.
func decodeCallResult(res *callResult) (json.RawMessage, error) {
	env, envErr := envelopeFromStreams(res)
	if res.exitCode != 0 {
		switch {
		case envErr == nil && env.Error != nil:
			return nil, &CLIError{
				Op:       res.op,
				Args:     res.args,
				Code:     env.Error.Code,
				Message:  env.Error.Message,
				ExitCode: res.exitCode,
				Stdout:   string(res.stdout),
				Stderr:   string(res.stderr),
			}
		case envErr == nil:
			// A success envelope on a non-zero exit status is contradictory.
			return nil, fmt.Errorf("%w: %s: exit status %d with a success envelope", ErrMalformedResponse, res.op, res.exitCode)
		case sawJSONObject(res):
			// A JSON body this client cannot interpret (an error envelope
			// without its code, say) must not be downgraded to a generic
			// failure: the caller would lose the classification.
			return nil, envErr
		default:
			// Plain-text CLI failure (a usage error, a crash): keep the exit
			// status and both streams so the caller can read what happened.
			return nil, &CLIError{
				Op:       res.op,
				Args:     res.args,
				ExitCode: res.exitCode,
				Message:  firstLine(string(res.stderr)),
				Stdout:   string(res.stdout),
				Stderr:   string(res.stderr),
			}
		}
	}
	if envErr != nil {
		return nil, envErr
	}
	if env.Error != nil {
		return nil, fmt.Errorf("%w: %s: exit status 0 with %q error envelope", ErrMalformedResponse, res.op, env.Error.Code)
	}
	return env.Result, nil
}

// sawJSONObject reports whether either stream starts with a JSON object, which
// means the command tried to speak the envelope protocol.
func sawJSONObject(res *callResult) bool {
	return bytes.HasPrefix(bytes.TrimSpace(res.stdout), []byte("{")) ||
		bytes.HasPrefix(bytes.TrimSpace(res.stderr), []byte("{"))
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// envelope is Herdr's response envelope. Unknown top-level fields are rejected
// by the decoder: an unrecognized key on the envelope means this client cannot
// tell whether the response is complete.
type envelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *errorBody      `json:"error"`
}

// envelopeFromStreams looks for the envelope on stdout and then stderr, which is
// where the installed CLI writes it for failing commands.
func envelopeFromStreams(res *callResult) (*envelope, error) {
	var firstErr error
	for _, s := range []struct {
		name string
		data []byte
	}{{streamStdout, res.stdout}, {streamStderr, res.stderr}} {
		env, err := decodeEnvelope(s.data, s.name)
		if err == nil {
			return env, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

func decodeEnvelope(raw []byte, stream string) (*envelope, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrMalformedResponse, stream)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrMalformedResponse, stream, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the JSON envelope on %s", ErrMalformedResponse, stream)
	}
	hasResult := len(env.Result) > 0 && !bytes.Equal(bytes.TrimSpace(env.Result), []byte("null"))
	hasError := env.Error != nil
	switch {
	case hasResult && hasError:
		return nil, fmt.Errorf("%w: %s carries both result and error", ErrMalformedResponse, stream)
	case !hasResult && !hasError:
		return nil, fmt.Errorf("%w: %s carries neither result nor error", ErrMalformedResponse, stream)
	}
	if hasError {
		if strings.TrimSpace(env.Error.Code) == "" || strings.TrimSpace(env.Error.Message) == "" {
			return nil, fmt.Errorf("%w: %s: error envelope is missing code or message", ErrMalformedResponse, stream)
		}
	}
	if hasResult && !bytes.HasPrefix(bytes.TrimSpace(env.Result), []byte("{")) {
		return nil, fmt.Errorf("%w: %s: result is not a JSON object", ErrMalformedResponse, stream)
	}
	return &env, nil
}

// boundedBuffer fails closed once limit bytes were written. os/exec reports the
// writer error from Run/Wait, so an oversized stream never yields truncated
// data to a decoder.
type boundedBuffer struct {
	buf    bytes.Buffer
	limit  int
	stream string
	err    error
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.buf.Len()+len(p) > b.limit {
		b.err = &OutputLimitError{Stream: b.stream, Limit: b.limit}
		return 0, b.err
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// ambiguity
// ---------------------------------------------------------------------------

// timeoutSemantics records what a timeout means for one command.
type timeoutSemantics int

const (
	// timeoutDefinite: the command changes nothing observable, so a timeout did
	// not alter Herdr state.
	timeoutDefinite timeoutSemantics = iota
	// timeoutAfterInput: the command writes input into a terminal, so a timeout
	// does not prove the input was not delivered.
	timeoutAfterInput
	// timeoutAfterMutation: the command creates or deletes topology, so a
	// timeout does not prove nothing happened.
	timeoutAfterMutation
)

func ambiguityReason(s timeoutSemantics) string {
	if s == timeoutAfterInput {
		return "timeout after submission: the input may already have been delivered; inspect the pane and the recorded step before retrying"
	}
	return "timeout during a mutating command: the change may already have been applied; inspect the matching Herdr state before retrying"
}

// ambiguityReasonUninterpretable is used when the response itself could not be
// interpreted. No timeout was observed, but the effect is just as undecided.
const ambiguityReasonUninterpretable = "uninterpretable response: the command's effect could not be determined; inspect the matching Herdr state before retrying"

// isUninterpretableResponse reports whether err means "this response cannot be
// interpreted": a rejected envelope or decode failure, an absent or
// contradictory identity, or a stream that exceeded MaxOutputBytes. Such an
// error says nothing about whether the command ran.
func isUninterpretableResponse(err error) bool {
	return errors.Is(err, ErrMalformedResponse) ||
		errors.Is(err, ErrIdentity) ||
		errors.Is(err, ErrOutputTooLarge)
}

// mayHaveTakenEffect reports whether err leaves the outcome of the command
// undecided for the given semantics.
func mayHaveTakenEffect(err error, semantics timeoutSemantics) bool {
	if errors.Is(err, ErrTimeout) {
		return true
	}
	// A response that cannot be interpreted is a failure to observe, not proof
	// that nothing happened: the side effect may already have been applied.
	if isUninterpretableResponse(err) {
		return true
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	if cliErr.Code == CodeTimeout {
		return true
	}
	return semantics == timeoutAfterInput && cliErr.Code == CodeAgentPromptStalled
}

// AmbiguousError reports a command whose effect could not be determined. It is
// never proof that nothing happened: the coordinator must inspect authoritative
// Herdr state before retrying.
type AmbiguousError struct {
	Op     string
	PaneID string
	Reason string
	// Err is the underlying failure (a *TimeoutError or *CLIError) or nil. It
	// stays reachable through errors.As so callers can still read the code.
	Err error
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("herdr %s: ambiguous outcome: %s (cause: %v)", e.Op, e.Reason, e.Err)
}

func (e *AmbiguousError) Unwrap() error { return e.Err }

func (e *AmbiguousError) Is(target error) bool { return target == ErrAmbiguous }

// Code reports the Herdr error code that triggered the ambiguity, when known.
func (e *AmbiguousError) Code() string {
	var cliErr *CLIError
	if errors.As(e.Err, &cliErr) {
		return cliErr.Code
	}
	return ""
}

// TimeoutError reports that the caller context expired before the CLI finished.
type TimeoutError struct {
	Op    string
	Args  []string
	Cause error // context.DeadlineExceeded or context.Canceled
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("herdr %s: %v after argv %q", e.Op, e.Cause, e.Args)
}

func (e *TimeoutError) Unwrap() error { return e.Cause }

func (e *TimeoutError) Is(target error) bool { return target == ErrTimeout }

// CLIError is a failure reported by the Herdr CLI. Code is empty when the CLI
// failed without printing an error envelope (for example a usage error).
type CLIError struct {
	Op       string
	Args     []string
	Code     string
	Message  string
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *CLIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("herdr %s failed with exit status %d: %q", e.Op, e.ExitCode, firstLine(e.Stderr))
	}
	return fmt.Sprintf("herdr %s failed: %s: %s (exit status %d)", e.Op, e.Code, e.Message, e.ExitCode)
}

func (e *CLIError) Is(target error) bool { return target == ErrCLI }

// IsCode reports whether the CLI answered with the given Herdr error code.
func (e *CLIError) IsCode(code string) bool { return e.Code == code }

// IdentityError reports a response whose required identity or provenance is
// absent, or contradicts another value in the same response or the request.
type IdentityError struct {
	Op     string
	Field  string
	Detail string
}

func (e *IdentityError) Error() string {
	return fmt.Sprintf("herdr %s: identity %s: %s", e.Op, e.Field, e.Detail)
}

func (e *IdentityError) Is(target error) bool { return target == ErrIdentity }

// OutputLimitError reports a stream that exceeded MaxOutputBytes. The stream is
// reported whole, never truncated.
type OutputLimitError struct {
	Stream string
	Limit  int
}

func (e *OutputLimitError) Error() string {
	return fmt.Sprintf("herdr: %s exceeded the %d byte limit", e.Stream, e.Limit)
}

func (e *OutputLimitError) Is(target error) bool { return target == ErrOutputTooLarge }

// ---------------------------------------------------------------------------
// decoded Herdr objects
// ---------------------------------------------------------------------------

// SessionRef is Herdr's AgentSessionInfo: the native session reference an
// installed integration reports for a pane. Against Herdr 0.9.1-preview the OMP
// integration reports kind "PATH" with the OMP session *file* path; other builds
// report kind "id" with the session id. The reference is a typed pair, so this
// client decodes it as one: it never parses a session id out of a file name.
type SessionRef struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

// Identity validates a reference and returns its kind and value. The kinds Herdr
// defines are accepted case-insensitively (the installed build reports "PATH"
// against a schema that documents "path"); an unknown kind or an empty value is a
// typed refusal, never a silently empty session.
func (r *SessionRef) Identity() (kind, value string, err error) {
	if r == nil {
		return "", "", &IdentityError{Op: "agent get", Field: "agent_session",
			Detail: "the pane reports no native session reference"}
	}
	canonical, ok := normalizeSessionKind(r.Kind)
	if !ok {
		return "", "", &IdentityError{Op: "agent get", Field: "agent_session.kind",
			Detail: fmt.Sprintf("unsupported session reference kind %q", r.Kind)}
	}
	trimmed := strings.TrimSpace(r.Value)
	if trimmed == "" {
		return "", "", &IdentityError{Op: "agent get", Field: "agent_session.value",
			Detail: fmt.Sprintf("a %s session reference carries no value", canonical)}
	}
	return canonical, trimmed, nil
}

// SessionKindID and SessionKindPath are the reference kinds Herdr documents.
const (
	SessionKindID   = "id"
	SessionKindPath = "path"
)

func normalizeSessionKind(kind string) (string, bool) {
	switch {
	case strings.EqualFold(strings.TrimSpace(kind), SessionKindID):
		return SessionKindID, true
	case strings.EqualFold(strings.TrimSpace(kind), SessionKindPath):
		return SessionKindPath, true
	default:
		return "", false
	}
}

// PaneInfo is the pane projection this client needs. Ids come from Herdr's
// response fields, never from labels.
type PaneInfo struct {
	PaneID       string `json:"pane_id"`
	TerminalID   string `json:"terminal_id"`
	WorkspaceID  string `json:"workspace_id"`
	TabID        string `json:"tab_id"`
	Label        string `json:"label"`
	Title        string `json:"title"`
	Agent        string `json:"agent"`
	DisplayAgent string `json:"display_agent"`
	AgentStatus  string `json:"agent_status"`
	Cwd          string `json:"cwd"`
	Focused      bool   `json:"focused"`
	Revision     uint64 `json:"revision"`
}

// AgentInfo is the live-agent projection. Name is Herdr's live agent name, which
// follows the terminal across a move and is not an identity for the store.
type AgentInfo struct {
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	DisplayAgent   string `json:"display_agent"`
	AgentStatus    string `json:"agent_status"`
	PaneID         string `json:"pane_id"`
	TerminalID     string `json:"terminal_id"`
	WorkspaceID    string `json:"workspace_id"`
	TabID          string `json:"tab_id"`
	Cwd            string `json:"cwd"`
	Focused        bool   `json:"focused"`
	Revision       uint64 `json:"revision"`
	StateChangeSeq uint64 `json:"state_change_seq"`
}

// WorkspaceWorktreeInfo is the checkout provenance Herdr attaches to a workspace
// created from a worktree.
type WorkspaceWorktreeInfo struct {
	CheckoutPath     string `json:"checkout_path"`
	RepoKey          string `json:"repo_key"`
	RepoName         string `json:"repo_name"`
	RepoRoot         string `json:"repo_root"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

// WorkspaceInfo is the workspace projection. Worktree is nil for a plain
// workspace.
type WorkspaceInfo struct {
	WorkspaceID string                 `json:"workspace_id"`
	Label       string                 `json:"label"`
	ActiveTabID string                 `json:"active_tab_id"`
	AgentStatus string                 `json:"agent_status"`
	Number      int                    `json:"number"`
	PaneCount   int                    `json:"pane_count"`
	TabCount    int                    `json:"tab_count"`
	Worktree    *WorkspaceWorktreeInfo `json:"worktree"`
}

// TabInfo is the tab projection.
type TabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	PaneCount   int    `json:"pane_count"`
}

// WorktreeInfo is one entry of `worktree list` or the created worktree.
type WorktreeInfo struct {
	Path             string  `json:"path"`
	Label            string  `json:"label"`
	Branch           *string `json:"branch"`
	OpenWorkspaceID  *string `json:"open_workspace_id"`
	IsBare           bool    `json:"is_bare"`
	IsDetached       bool    `json:"is_detached"`
	IsPrunable       bool    `json:"is_prunable"`
	IsLinkedWorktree bool    `json:"is_linked_worktree"`
}

// WorktreeSource is the repository a worktree listing came from.
type WorktreeSource struct {
	RepoKey            string `json:"repo_key"`
	RepoName           string `json:"repo_name"`
	RepoRoot           string `json:"repo_root"`
	SourceCheckoutPath string `json:"source_checkout_path"`
	SourceWorkspaceID  string `json:"source_workspace_id"`
}

// WorkspaceCreated is the result of `workspace create`.
type WorkspaceCreated struct {
	WorkspaceID string
	TabID       string
	RootPaneID  string
}

// WorktreeCreated is the result of `worktree create`, including the checkout
// provenance the caller needs before recording a worktree binding.
type WorktreeCreated struct {
	WorkspaceID      string
	TabID            string
	RootPaneID       string
	WorktreePath     string
	RepoRoot         string
	Branch           string
	IsLinkedWorktree bool
}

// WorktreeList is the result of `worktree list`.
type WorktreeList struct {
	Source    WorktreeSource
	Worktrees []WorktreeInfo
}

// WorktreeRemoved is the result of `worktree remove`.
type WorktreeRemoved struct {
	WorkspaceID string
	Path        string
	Forced      bool
}

// PaneMoved is the result of `pane move`. PreviousPaneID is the launch-time
// identity (Herdr keeps it as an alias); PaneID is the current public identity
// that later commands must target, and the only one that may be re-bound.
// ClosedWorkspaceID and ClosedTabID report a source workspace or tab the move
// closed — moving the last pane out of a workspace closes it — and are empty
// when the move closed nothing.
type PaneMoved struct {
	Changed             bool
	PreviousPaneID      string
	PreviousWorkspaceID string
	PreviousTabID       string
	ClosedWorkspaceID   string
	ClosedTabID         string
	PaneID              string
	WorkspaceID         string
	TabID               string
	TerminalID          string
}

// AgentStarted is the result of `agent start`.
type AgentStarted struct {
	Name   string
	PaneID string
	Agent  AgentInfo
	Argv   []string
}

// Snapshot is the result of `api snapshot`, used to inspect authoritative state
// (for example after an ambiguous timeout).
type Snapshot struct {
	Version            string
	Protocol           int
	FocusedPaneID      string
	FocusedTabID       string
	FocusedWorkspaceID string
	Agents             []AgentInfo
	Panes              []PaneInfo
}

// AgentReadOptions selects what `agent read` returns. Lines is the number of
// rendered rows (0 lets Herdr use its default); ANSI keeps escape sequences.
type AgentReadOptions struct {
	Source string
	Lines  int
	ANSI   bool
}

// PaneMoveOptions describes one `pane move`. Exactly one destination must be
// set. Label and TabLabel apply to the created layout; Split is required with
// TabID and must be "right" or "down".
type PaneMoveOptions struct {
	TabID        string
	TargetPaneID string
	Split        string
	Label        string
	TabLabel     string
	NewTab       bool
	NewWorkspace bool
}

// ---------------------------------------------------------------------------
// typed calls
// ---------------------------------------------------------------------------

// WorkspaceCreate creates a workspace for cwd without stealing focus.
func (c *Client) WorkspaceCreate(ctx context.Context, cwd, label string) (created *WorkspaceCreated, err error) {
	const op = "workspace create"
	if strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("%w: %s: cwd is empty", ErrInvalidArgument, op)
	}
	args := []string{"workspace", "create", "--cwd", cwd}
	if label != "" {
		args = append(args, "--label", label)
	}
	args = append(args, "--no-focus")

	// The workspace may already exist: every failure from here on, including a
	// response this client cannot interpret, is classified by the semantics.
	defer func() { err = c.classify(op, timeoutAfterMutation, err) }()

	raw, err := c.call(ctx, op, args, timeoutAfterMutation)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type      string        `json:"type"`
		Workspace WorkspaceInfo `json:"workspace"`
		Tab       TabInfo       `json:"tab"`
		RootPane  PaneInfo      `json:"root_pane"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "workspace_created", res.Type); err != nil {
		return nil, err
	}
	if err := validateWorkspace(op, res.Workspace); err != nil {
		return nil, err
	}
	if err := validateTab(op, res.Tab); err != nil {
		return nil, err
	}
	if err := validatePane(op, res.RootPane); err != nil {
		return nil, err
	}
	if res.RootPane.WorkspaceID != res.Workspace.WorkspaceID {
		return nil, &IdentityError{
			Op:     op,
			Field:  "root_pane.workspace_id",
			Detail: fmt.Sprintf("root pane belongs to workspace %q but the created workspace is %q", res.RootPane.WorkspaceID, res.Workspace.WorkspaceID),
		}
	}
	return &WorkspaceCreated{
		WorkspaceID: res.Workspace.WorkspaceID,
		TabID:       res.Tab.TabID,
		RootPaneID:  res.RootPane.PaneID,
	}, nil
}

// WorktreeCreate creates a Git worktree checkout for branch from base and opens
// it as a grouped workspace, without stealing focus. The response must carry
// workspace, root pane and worktree provenance or it is rejected.
func (c *Client) WorktreeCreate(ctx context.Context, root, branch, base string) (created *WorktreeCreated, err error) {
	const op = "worktree create"
	for _, arg := range []struct{ name, value string }{
		{"cwd", root}, {"branch", branch}, {"base", base},
	} {
		if strings.TrimSpace(arg.value) == "" {
			return nil, fmt.Errorf("%w: %s: %s is empty", ErrInvalidArgument, op, arg.name)
		}
	}
	args := []string{"worktree", "create", "--cwd", root, "--branch", branch, "--base", base, "--no-focus"}

	// The checkout may already exist: every failure from here on, including a
	// response this client cannot interpret, is classified by the semantics.
	defer func() { err = c.classify(op, timeoutAfterMutation, err) }()

	raw, err := c.call(ctx, op, args, timeoutAfterMutation)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type      string        `json:"type"`
		Workspace WorkspaceInfo `json:"workspace"`
		Tab       TabInfo       `json:"tab"`
		RootPane  PaneInfo      `json:"root_pane"`
		Worktree  WorktreeInfo  `json:"worktree"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "worktree_created", res.Type); err != nil {
		return nil, err
	}
	if err := validateWorkspace(op, res.Workspace); err != nil {
		return nil, err
	}
	if err := validateTab(op, res.Tab); err != nil {
		return nil, err
	}
	if err := validatePane(op, res.RootPane); err != nil {
		return nil, err
	}
	if res.RootPane.WorkspaceID != res.Workspace.WorkspaceID {
		return nil, &IdentityError{
			Op:     op,
			Field:  "root_pane.workspace_id",
			Detail: fmt.Sprintf("root pane belongs to workspace %q but the created workspace is %q", res.RootPane.WorkspaceID, res.Workspace.WorkspaceID),
		}
	}
	if res.Workspace.Worktree == nil {
		return nil, &IdentityError{Op: op, Field: "workspace.worktree", Detail: "created workspace carries no worktree provenance"}
	}
	if strings.TrimSpace(res.Workspace.Worktree.CheckoutPath) == "" || strings.TrimSpace(res.Workspace.Worktree.RepoRoot) == "" {
		return nil, &IdentityError{Op: op, Field: "workspace.worktree.checkout_path", Detail: "created workspace has no checkout path or repo root"}
	}
	if strings.TrimSpace(res.Worktree.Path) == "" {
		return nil, &IdentityError{Op: op, Field: "worktree.path", Detail: "response carries no worktree path"}
	}
	if !res.Worktree.IsLinkedWorktree {
		// `worktree create` runs `git worktree add`, which always produces a
		// linked checkout. A response saying otherwise describes the primary
		// repository, which the orchestrator must never bind as its isolated
		// worktree.
		return nil, &IdentityError{Op: op, Field: "worktree.is_linked_worktree", Detail: "response does not describe a linked worktree checkout"}
	}
	if res.Worktree.OpenWorkspaceID != nil && *res.Worktree.OpenWorkspaceID != res.Workspace.WorkspaceID {
		return nil, &IdentityError{
			Op:     op,
			Field:  "worktree.open_workspace_id",
			Detail: fmt.Sprintf("worktree is open in workspace %q but the created workspace is %q", *res.Worktree.OpenWorkspaceID, res.Workspace.WorkspaceID),
		}
	}
	branchName := ""
	if res.Worktree.Branch != nil {
		branchName = *res.Worktree.Branch
	}
	return &WorktreeCreated{
		WorkspaceID:      res.Workspace.WorkspaceID,
		TabID:            res.Tab.TabID,
		RootPaneID:       res.RootPane.PaneID,
		WorktreePath:     res.Worktree.Path,
		RepoRoot:         res.Workspace.Worktree.RepoRoot,
		Branch:           branchName,
		IsLinkedWorktree: res.Worktree.IsLinkedWorktree,
	}, nil
}

// WorktreeList lists the worktrees of the repository containing cwd.
func (c *Client) WorktreeList(ctx context.Context, cwd string) (*WorktreeList, error) {
	const op = "worktree list"
	if strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("%w: %s: cwd is empty", ErrInvalidArgument, op)
	}
	raw, err := c.call(ctx, op, []string{"worktree", "list", "--cwd", cwd}, timeoutDefinite)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type      string         `json:"type"`
		Source    WorktreeSource `json:"source"`
		Worktrees []WorktreeInfo `json:"worktrees"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "worktree_list", res.Type); err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.Source.RepoRoot) == "" {
		return nil, &IdentityError{Op: op, Field: "source.repo_root", Detail: "listing carries no repository root"}
	}
	for i, wt := range res.Worktrees {
		if strings.TrimSpace(wt.Path) == "" {
			return nil, &IdentityError{Op: op, Field: "worktrees.path", Detail: fmt.Sprintf("worktree %d has no path", i)}
		}
	}
	return &WorktreeList{Source: res.Source, Worktrees: res.Worktrees}, nil
}

// WorktreeRemove deletes the checkout of the worktree workspace. It never
// deletes the branch. force must only be used after confirming the checkout is
// not carrying work that matters.
func (c *Client) WorktreeRemove(ctx context.Context, workspaceID string, force bool) (removed *WorktreeRemoved, err error) {
	const op = "worktree remove"
	if strings.TrimSpace(workspaceID) == "" {
		return nil, fmt.Errorf("%w: %s: workspace id is empty", ErrInvalidArgument, op)
	}
	args := []string{"worktree", "remove", "--workspace", workspaceID}
	if force {
		args = append(args, "--force")
	}

	// The checkout may already be gone: every failure from here on, including a
	// response this client cannot interpret, is classified by the semantics.
	defer func() { err = c.classify(op, timeoutAfterMutation, err) }()

	raw, err := c.call(ctx, op, args, timeoutAfterMutation)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type        string `json:"type"`
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		Forced      bool   `json:"forced"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "worktree_removed", res.Type); err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.WorkspaceID) == "" {
		return nil, &IdentityError{Op: op, Field: "workspace_id", Detail: "response carries no workspace id"}
	}
	if res.WorkspaceID != workspaceID {
		return nil, &IdentityError{
			Op:     op,
			Field:  "workspace_id",
			Detail: fmt.Sprintf("response is about workspace %q, not the requested %q", res.WorkspaceID, workspaceID),
		}
	}
	return &WorktreeRemoved{WorkspaceID: res.WorkspaceID, Path: res.Path, Forced: res.Forced}, nil
}

// PaneGet reads one pane. paneID may be a launch-time id that Herdr still keeps
// as an alias after a move; the returned PaneInfo then carries the current
// public id, so callers must use the response rather than assume they match.
func (c *Client) PaneGet(ctx context.Context, paneID string) (*PaneInfo, error) {
	const op = "pane get"
	if strings.TrimSpace(paneID) == "" {
		return nil, fmt.Errorf("%w: %s: pane id is empty", ErrInvalidArgument, op)
	}
	raw, err := c.call(ctx, op, []string{"pane", "get", paneID}, timeoutDefinite)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type string   `json:"type"`
		Pane PaneInfo `json:"pane"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "pane_info", res.Type); err != nil {
		return nil, err
	}
	if err := validatePane(op, res.Pane); err != nil {
		return nil, err
	}
	return &res.Pane, nil
}

// PaneMove moves a pane to a tab or a new layout. The result exposes the
// launch-time identity and the current public identity separately; only the
// public one may be re-bound.
func (c *Client) PaneMove(ctx context.Context, paneID string, opts PaneMoveOptions) (moved *PaneMoved, err error) {
	const op = "pane move"
	if strings.TrimSpace(paneID) == "" {
		return nil, fmt.Errorf("%w: %s: pane id is empty", ErrInvalidArgument, op)
	}
	targets := 0
	if opts.TabID != "" {
		targets++
	}
	if opts.NewTab {
		targets++
	}
	if opts.NewWorkspace {
		targets++
	}
	if targets != 1 {
		return nil, fmt.Errorf("%w: %s: exactly one of tab id, new tab or new workspace is required", ErrInvalidArgument, op)
	}
	args := []string{"pane", "move", paneID}
	switch {
	case opts.TabID != "":
		if opts.Split != "right" && opts.Split != "down" {
			return nil, fmt.Errorf("%w: %s: split must be right or down", ErrInvalidArgument, op)
		}
		args = append(args, "--tab", opts.TabID, "--split", opts.Split)
		if opts.TargetPaneID != "" {
			args = append(args, "--target-pane", opts.TargetPaneID)
		}
	case opts.NewTab:
		args = append(args, "--new-tab")
		if opts.Label != "" {
			args = append(args, "--label", opts.Label)
		}
	case opts.NewWorkspace:
		args = append(args, "--new-workspace")
		if opts.Label != "" {
			args = append(args, "--label", opts.Label)
		}
		if opts.TabLabel != "" {
			args = append(args, "--tab-label", opts.TabLabel)
		}
	}
	args = append(args, "--no-focus")

	// The pane may already have moved: every failure from here on, including a
	// response this client cannot interpret, is classified by the semantics.
	defer func() { err = c.classify(op, timeoutAfterMutation, err) }()

	raw, err := c.call(ctx, op, args, timeoutAfterMutation)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type       string `json:"type"`
		MoveResult struct {
			Changed             bool     `json:"changed"`
			PreviousPaneID      string   `json:"previous_pane_id"`
			PreviousWorkspaceID string   `json:"previous_workspace_id"`
			PreviousTabID       string   `json:"previous_tab_id"`
			ClosedWorkspaceID   string   `json:"closed_workspace_id"`
			ClosedTabID         string   `json:"closed_tab_id"`
			Pane                PaneInfo `json:"pane"`
		} `json:"move_result"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "pane_move", res.Type); err != nil {
		return nil, err
	}
	move := res.MoveResult
	if strings.TrimSpace(move.PreviousPaneID) == "" {
		return nil, &IdentityError{Op: op, Field: "move_result.previous_pane_id", Detail: "response does not say which pane was moved"}
	}
	if err := validatePane(op, move.Pane); err != nil {
		return nil, err
	}
	return &PaneMoved{
		Changed:             move.Changed,
		PreviousPaneID:      move.PreviousPaneID,
		PreviousWorkspaceID: move.PreviousWorkspaceID,
		PreviousTabID:       move.PreviousTabID,
		ClosedWorkspaceID:   move.ClosedWorkspaceID,
		ClosedTabID:         move.ClosedTabID,
		PaneID:              move.Pane.PaneID,
		WorkspaceID:         move.Pane.WorkspaceID,
		TabID:               move.Pane.TabID,
		TerminalID:          move.Pane.TerminalID,
	}, nil
}

// AgentStart activates an available shell pane with kind omp under a live agent
// name. The name must match [a-z][a-z0-9_-]{0,31} and top-level topology must
// already exist.
func (c *Client) AgentStart(ctx context.Context, name, paneID string) (started *AgentStarted, err error) {
	const op = "agent start"
	if !agentNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: %s: %q is not a legal agent name", ErrInvalidArgument, op, name)
	}
	if strings.TrimSpace(paneID) == "" {
		return nil, fmt.Errorf("%w: %s: pane id is empty", ErrInvalidArgument, op)
	}
	args := []string{"agent", "start", name, "--kind", "omp", "--pane", paneID}

	// The agent may already be running: every failure from here on, including a
	// response this client cannot interpret, is classified by the semantics.
	defer func() { err = c.classify(op, timeoutAfterMutation, err) }()

	raw, err := c.call(ctx, op, args, timeoutAfterMutation)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type  string    `json:"type"`
		Agent AgentInfo `json:"agent"`
		Argv  []string  `json:"argv"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "agent_started", res.Type); err != nil {
		return nil, err
	}
	if err := validateAgent(op, res.Agent); err != nil {
		return nil, err
	}
	if res.Agent.PaneID != paneID {
		return nil, &IdentityError{
			Op:     op,
			Field:  "agent.pane_id",
			Detail: fmt.Sprintf("agent started on pane %q, not the requested %q", res.Agent.PaneID, paneID),
		}
	}
	return &AgentStarted{Name: name, PaneID: res.Agent.PaneID, Agent: res.Agent, Argv: res.Argv}, nil
}

// AgentPrompt submits brief to the agent on the public pane id paneID as one
// ordered write plus Enter. A timeout here is ambiguous: the brief may already
// have been submitted. Never resubmit on an ambiguous result; inspect state
// first.
func (c *Client) AgentPrompt(ctx context.Context, paneID, brief string) (prompted *AgentInfo, err error) {
	const op = "agent prompt"
	if strings.TrimSpace(paneID) == "" {
		return nil, fmt.Errorf("%w: %s: pane id is empty", ErrInvalidArgument, op)
	}
	if strings.TrimSpace(brief) == "" {
		return nil, fmt.Errorf("%w: %s: brief is empty", ErrInvalidArgument, op)
	}
	// brief is one argv element, whatever it contains: quotes and newlines are
	// data, not shell syntax.
	args := []string{"agent", "prompt", paneID, brief}

	// The brief may already have been submitted: every failure from here on,
	// including a response this client cannot interpret, is classified by the
	// semantics, and every ambiguous outcome keeps the pane it concerns.
	defer func() {
		err = c.classify(op, timeoutAfterInput, err)
		var ambiguous *AmbiguousError
		if errors.As(err, &ambiguous) {
			ambiguous.PaneID = paneID
		}
	}()

	raw, err := c.call(ctx, op, args, timeoutAfterInput)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type  string    `json:"type"`
		Agent AgentInfo `json:"agent"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "agent_prompted", res.Type); err != nil {
		return nil, err
	}
	if err := validateAgent(op, res.Agent); err != nil {
		return nil, err
	}
	return &res.Agent, nil
}

// AgentGet reads the agent behind a live agent name or pane id.
func (c *Client) AgentGet(ctx context.Context, target string) (*AgentInfo, error) {
	const op = "agent get"
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("%w: %s: target is empty", ErrInvalidArgument, op)
	}
	raw, err := c.call(ctx, op, []string{"agent", "get", target}, timeoutDefinite)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type  string    `json:"type"`
		Agent AgentInfo `json:"agent"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "agent_info", res.Type); err != nil {
		return nil, err
	}
	if err := validateAgent(op, res.Agent); err != nil {
		return nil, err
	}
	return &res.Agent, nil
}

// AgentRead reads the agent's resolved terminal stream. Herdr prints terminal
// text for this command, not JSON, so the result is returned verbatim and is
// still bounded by MaxOutputBytes.
func (c *Client) AgentRead(ctx context.Context, target string, opts AgentReadOptions) (string, error) {
	const op = "agent read"
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("%w: %s: target is empty", ErrInvalidArgument, op)
	}
	if opts.Source != "" && !validReadSources[opts.Source] {
		return "", fmt.Errorf("%w: %s: unknown source %q", ErrInvalidArgument, op, opts.Source)
	}
	if opts.Lines < 0 {
		return "", fmt.Errorf("%w: %s: lines must not be negative", ErrInvalidArgument, op)
	}
	args := []string{"agent", "read", target}
	if opts.Source != "" {
		args = append(args, "--source", opts.Source)
	}
	if opts.Lines > 0 {
		args = append(args, "--lines", strconv.Itoa(opts.Lines))
	}
	if opts.ANSI {
		args = append(args, "--ansi")
	}

	res, err := c.run(ctx, op, args)
	if err != nil {
		return "", c.classify(op, timeoutDefinite, err)
	}
	if res.exitCode != 0 {
		_, err := decodeCallResult(res)
		if err == nil {
			err = fmt.Errorf("%w: %s: exit status %d with a success envelope", ErrMalformedResponse, op, res.exitCode)
		}
		return "", c.classify(op, timeoutDefinite, err)
	}
	return string(res.stdout), nil
}

// AgentSendKeys sends logical terminal keys. Herdr validates every key before
// writing any bytes, so a rejection here means nothing was written.
func (c *Client) AgentSendKeys(ctx context.Context, target string, keys ...string) (err error) {
	const op = "agent send-keys"
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w: %s: target is empty", ErrInvalidArgument, op)
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: %s: at least one key is required", ErrInvalidArgument, op)
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: %s: empty key", ErrInvalidArgument, op)
		}
	}
	args := append([]string{"agent", "send-keys", target}, keys...)

	// The keys may already have been delivered: every failure from here on,
	// including a response this client cannot interpret, is classified by the
	// semantics.
	defer func() { err = c.classify(op, timeoutAfterInput, err) }()

	raw, err := c.call(ctx, op, args, timeoutAfterInput)
	if err != nil {
		return err
	}
	var res struct {
		Type  string     `json:"type"`
		Agent *AgentInfo `json:"agent"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return err
	}
	// The success result type for send-keys was not captured (that would have
	// required sending keys into a live agent), so only the shapes the API
	// schema can produce are accepted. Anything else fails closed.
	switch res.Type {
	case "ok":
		return nil
	case "agent_info":
		if res.Agent == nil {
			return fmt.Errorf("%w: %s: agent_info result carries no agent", ErrMalformedResponse, op)
		}
		return validateAgent(op, *res.Agent)
	default:
		return fmt.Errorf("%w: %s: unrecognized result type %q", ErrMalformedResponse, op, res.Type)
	}
}

// Snapshot reads the authoritative live state. After an ambiguous timeout this
// is what a caller inspects before deciding anything.
func (c *Client) Snapshot(ctx context.Context) (*Snapshot, error) {
	const op = "api snapshot"
	raw, err := c.call(ctx, op, []string{"api", "snapshot"}, timeoutDefinite)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type     string `json:"type"`
		Snapshot struct {
			Version            string      `json:"version"`
			Protocol           int         `json:"protocol"`
			FocusedPaneID      string      `json:"focused_pane_id"`
			FocusedTabID       string      `json:"focused_tab_id"`
			FocusedWorkspaceID string      `json:"focused_workspace_id"`
			Agents             []AgentInfo `json:"agents"`
			Panes              []PaneInfo  `json:"panes"`
		} `json:"snapshot"`
	}
	if err := decodeResult(op, raw, &res); err != nil {
		return nil, err
	}
	if err := checkType(op, "session_snapshot", res.Type); err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.Snapshot.Version) == "" {
		return nil, &IdentityError{Op: op, Field: "snapshot.version", Detail: "snapshot carries no server version"}
	}
	for i, agent := range res.Snapshot.Agents {
		if err := validateAgent(op, agent); err != nil {
			return nil, prefixIdentityError(op, fmt.Sprintf("snapshot.agents[%d]", i), err)
		}
	}
	for i, pane := range res.Snapshot.Panes {
		if err := validatePane(op, pane); err != nil {
			return nil, prefixIdentityError(op, fmt.Sprintf("snapshot.panes[%d]", i), err)
		}
	}
	return &Snapshot{
		Version:            res.Snapshot.Version,
		Protocol:           res.Snapshot.Protocol,
		FocusedPaneID:      res.Snapshot.FocusedPaneID,
		FocusedTabID:       res.Snapshot.FocusedTabID,
		FocusedWorkspaceID: res.Snapshot.FocusedWorkspaceID,
		Agents:             res.Snapshot.Agents,
		Panes:              res.Snapshot.Panes,
	}, nil
}

// ---------------------------------------------------------------------------
// decoding helpers
// ---------------------------------------------------------------------------

// decodeResult unmarshals a result body. Unknown fields inside the body are
// tolerated by design (Herdr adds fields within a protocol version); the
// envelope and the result type are what stay strict.
func decodeResult(op string, raw json.RawMessage, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s: decode result: %v", ErrMalformedResponse, op, err)
	}
	return nil
}

func checkType(op, want, got string) error {
	if got != want {
		return fmt.Errorf("%w: %s: expected result type %q, got %q", ErrMalformedResponse, op, want, got)
	}
	return nil
}

func requireID(op, field, value string) error {
	if strings.TrimSpace(value) == "" {
		return &IdentityError{Op: op, Field: field, Detail: "required identity is empty"}
	}
	return nil
}

// prefixIdentityError re-files an identity error under an enclosing field, so a
// bad entry inside a list says which entry it was.
func prefixIdentityError(op, prefix string, err error) error {
	var identity *IdentityError
	if errors.As(err, &identity) {
		return &IdentityError{Op: op, Field: prefix + "." + identity.Field, Detail: identity.Detail}
	}
	return err
}

func validatePane(op string, p PaneInfo) error {
	for _, f := range []struct{ field, value string }{
		{"pane_id", p.PaneID},
		{"terminal_id", p.TerminalID},
		{"workspace_id", p.WorkspaceID},
		{"tab_id", p.TabID},
	} {
		if err := requireID(op, "pane."+f.field, f.value); err != nil {
			return err
		}
	}
	return nil
}

func validateAgent(op string, a AgentInfo) error {
	for _, f := range []struct{ field, value string }{
		{"pane_id", a.PaneID},
		{"terminal_id", a.TerminalID},
		{"workspace_id", a.WorkspaceID},
		{"tab_id", a.TabID},
	} {
		if err := requireID(op, "agent."+f.field, f.value); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkspace(op string, w WorkspaceInfo) error {
	for _, f := range []struct{ field, value string }{
		{"workspace_id", w.WorkspaceID},
		{"active_tab_id", w.ActiveTabID},
	} {
		if err := requireID(op, "workspace."+f.field, f.value); err != nil {
			return err
		}
	}
	return nil
}

func validateTab(op string, tab TabInfo) error {
	for _, f := range []struct{ field, value string }{
		{"tab_id", tab.TabID},
		{"workspace_id", tab.WorkspaceID},
	} {
		if err := requireID(op, "tab."+f.field, f.value); err != nil {
			return err
		}
	}
	return nil
}
