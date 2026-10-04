// Package task defines the durable facts of one dispatched unit of work and the
// projection from those facts onto a board column.
//
// Facts and activity are deliberately separate: Herdr activity only ever
// describes what a terminal is doing right now, so nothing here derives
// completion from it.
package task

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Stage is the board column a task occupies.
type Stage string

const (
	Queued         Stage = "queued"
	Running        Stage = "running"
	NeedsInput     Stage = "needs_input"
	ReadyForReview Stage = "ready_for_review"
	Done           Stage = "done"
)

// Phase records how far provisioning and dispatch of a task has progressed.
type Phase string

const (
	Intent          Phase = "intent"
	WorktreeCreated Phase = "worktree_created"
	AgentStarted    Phase = "agent_started"
	PromptSubmitted Phase = "prompt_submitted"
)

// Report is a worker's self-reported outcome. Only Kind "result" is final. A
// final report's Checks each state how they ended; a report stored before that
// was required holds free-text checks, which read as checks without an outcome
// (see Check).
type Report struct {
	Kind, Summary, Branch string
	Evidence              []string
	Checks                []Check
	Risk                  string
}

// Task is the durable record of one unit of work.
type Task struct {
	ID, ProjectKey, Title, Brief, BaseRevision, Branch  string
	Checks, Dependencies, ReservedPaths                 []string
	WorktreePath, WorktreeWorkspaceID                   string
	CurrentWorkspaceID, TerminalID                      string
	LaunchPaneID, PublicPaneID, AgentName, OMPSessionID string
	Phase                                               Phase
	Report                                              *Report
	Blocker, Attention, Disposition                     string
	// Outcome is the structured kind of the user's decision; Disposition carries
	// its free-text note. A closed task with an empty Outcome is a legacy row.
	Outcome                                        string
	Activity                                       string
	Revision, CreatedAt, UpdatedAt, ActivitySeenAt int64
	// TicketID names the ticket this task was dispatched for. It is set when the
	// task is created and never changes, so work cannot be linked to a ticket
	// after the fact; a task without one reads as unlinked.
	TicketID string
}

// The kinds of decision a primary can record. Every kind closes the task, frees
// its slot and releases its reserved paths; only an accepting kind also releases
// the tasks that depend on it.
const (
	OutcomeIntegrated = "integrated"
	OutcomeKept       = "kept"
	OutcomeRejected   = "rejected"
	OutcomeSuperseded = "superseded"
	OutcomeAbandoned  = "abandoned"
)

// Outcomes lists every recordable outcome in display order.
func Outcomes() []string {
	return []string{OutcomeIntegrated, OutcomeKept, OutcomeRejected, OutcomeSuperseded, OutcomeAbandoned}
}

// ValidOutcome reports whether kind is a recordable outcome.
func ValidOutcome(kind string) bool {
	for _, o := range Outcomes() {
		if o == kind {
			return true
		}
	}
	return false
}

// Accepted reports whether a closed task released its dependents. A legacy row
// (disposition text, no outcome) predates the contract and keeps the behaviour
// it was recorded under, so it counts as accepted.
func (x Task) Accepted() bool {
	if x.Disposition == "" {
		return false
	}
	return x.Outcome == "" || x.Outcome == OutcomeIntegrated || x.Outcome == OutcomeKept
}

// Project is the durable record of one primary checkout the plugin dispatches from.
type Project struct {
	Key, RepoWorkspaceID, CurrentWorkspaceID, TerminalID string
	LaunchPaneID, PublicPaneID, AgentName, OMPSessionID  string
	// PrimaryAttention is why the plugin cannot currently vouch for the project's
	// primary, or empty while it can. It is a recorded observation, not a lifecycle
	// state: reconciliation and the paths that probe the primary write it when the
	// primary is absent or unproven and clear it once the primary is verified live
	// again, and the board only reads it. Writing it changes no task and moves none
	// between stages; it is separate from Task.Attention and from the inbox notes
	// that some of those paths queue for the primary. A project row written before
	// this field existed decodes to the empty string, which reads as "no attention".
	PrimaryAttention string
	// Replaced is the primaries an explicit reattach replaced, oldest first and
	// bounded to the most recent MaxReplacedPrimaries. The project row only ever
	// holds the primary it currently records, so without this list the session a
	// reattach displaced would vanish silently; with it, which session held the
	// project, in which pane and why the plugin could not vouch for it any more is
	// a durable fact. A project row written before this field existed decodes to
	// none.
	Replaced []ReplacedPrimary
	// ReattachNotice is the notice attempt of the latest reattach, kept until the
	// replacement is confirmed. It is what stops a later reattach from sending a
	// second notice into a session that may already have received one. A project
	// row written before this field existed decodes to none.
	ReattachNotice NoticeAttempt
}

// MaxReplacedPrimaries bounds Project.Replaced.
const MaxReplacedPrimaries = 8

// ReplacedPrimary is one primary binding that an explicit reattach displaced:
// the identities the project recorded for it at that moment, why the plugin could
// not vouch for it, and when the replacement was recorded.
type ReplacedPrimary struct {
	LaunchPaneID, PublicPaneID, TerminalID, WorkspaceID string
	// OMPSessionID is the session identity the displaced binding recorded. It is
	// kept as recorded, in whatever form the binding held it.
	OMPSessionID string
	// Reason is what the plugin observed about the displaced primary when it was
	// replaced (its pane gone, a different agent in it, a session it could not
	// prove).
	Reason string
	// At is when the replacement was recorded, in Unix milliseconds.
	At int64
}

// ReattachNotice states. A notice is the one prompt a reattach sends the
// replacement session so its extension can announce itself.
const (
	// NoticeUncertain is an attempt whose outcome is unknown: it is recorded
	// before the prompt is sent, and stays so when the prompt's answer was
	// ambiguous, failed without proof that nothing was written, or the process
	// stopped mid-call. Like an uncertain inbox note it is inspected by a human and
	// never resent.
	NoticeUncertain = "uncertain"
	// NoticeSent is a notice Herdr acknowledged. The replacement took its turn, so
	// sending another one cannot make its extension announce.
	NoticeSent = "sent"
)

// NoticeAttempt records the notice a reattach sent (or may have sent) to one
// replacement session. It is keyed by the pane and session it went to, so a
// different replacement starts with no attempt. The zero value is "none".
type NoticeAttempt struct {
	PaneID  string
	Session string
	// Kind is the form Session is held in (SessionKindID or SessionKindPath), as
	// Herdr reported it, so a later comparison never has to guess it.
	Kind  string
	State string
	// At is when the attempt was recorded, in Unix milliseconds.
	At int64
}

// Inbox is one durable note addressed to a project or task.
//
// Two facts travel together and are deliberately separate:
//
//   - Delivery is the human-readable reason the note exists ("worker result",
//     "worktree removed"), written by whoever recorded it. It is a label, not a
//     state, so it never decides whether the note may be sent.
//   - DeliveryState is the transactional claim state of that note's journey to
//     the primary: pending until a flush claims it, sending while one attempt is
//     in flight, delivered once the primary acknowledged it, and uncertain when
//     an attempt's outcome could not be determined (a timeout, or the plugin
//     stopping mid-delivery). Uncertain is inspected by a human, never resent.
//
// DeliveryDetail names the pane that took the note, or the reason an attempt
// did not settle.
type Inbox struct {
	ID, ProjectKey, TaskID, Kind, Summary, Delivery string
	DeliveryState                                   string
	DeliveryDetail                                  string
}

// The delivery states one durable note moves through. A note is created pending
// and only a claimed flush may settle it.
const (
	// DeliveryPending is a note no flush has claimed yet.
	DeliveryPending = "pending"
	// DeliverySending is a note claimed by one in-flight delivery attempt.
	DeliverySending = "sending"
	// DeliveryDelivered is a note the primary accepted.
	DeliveryDelivered = "delivered"
	// DeliveryUncertain is a note whose attempt could not be resolved. It is
	// inspected (agent get/read) before anything is retried.
	DeliveryUncertain = "uncertain"
)

// State is the note's delivery state, defaulting to pending for a note that was
// never claimed.
func (i Inbox) State() string {
	if strings.TrimSpace(i.DeliveryState) == "" {
		return DeliveryPending
	}
	return i.DeliveryState
}

// StageOf projects recorded facts onto a Stage in priority order.
//
// A recorded disposition closes a task; a blocker or attention request needs a
// human; only a final Report waits for review. Herdr activity and a partial
// report never complete anything.
func StageOf(x Task) Stage {
	switch {
	case x.Disposition != "":
		return Done
	case x.Blocker != "" || x.Attention != "":
		return NeedsInput
	case x.Report != nil && x.Report.Kind == "result":
		return ReadyForReview
	case x.Phase == WorktreeCreated || x.Phase == AgentStarted || x.Phase == PromptSubmitted:
		return Running
	default:
		return Queued
	}
}

// The two native session identities an installed agent integration reports. Herdr
// labels the reference with its kind, and the kind decides how the value must be
// compared: an id is an opaque token, a path is a file the platform resolves.
const (
	SessionKindID   = "id"
	SessionKindPath = "path"
)

// SessionRef is one native session identity in the form Herdr reports it: a kind
// and the value that kind names. It is the unit the readiness handshake verifies,
// so a reference whose kind this build does not understand is refused instead of
// being compared approximately.
type SessionRef struct {
	Kind  string `json:"Kind"`
	Value string `json:"Value"`
}

// Announcement is what an agent process tells the bridge about itself: both
// native identities the runtime exposes, plus whether that session has been
// materialized to durable storage. At least one identity must be present. Both
// are recorded, so the coordinator's confirmation can be re-compared in the same
// form Herdr used rather than in a single preferred form.
//
// OnDisk is state, not identity: it never takes part in a comparison, and it
// exists because an agent runtime may allocate a session path before writing it
// (OMP materializes a session lazily, on its first assistant message), so a
// resume command built from an unmaterialized session cannot work.
type Announcement struct {
	ID     string `json:"ID"`
	File   string `json:"File"`
	OnDisk bool   `json:"OnDisk"`
}

// NewAnnouncement trims one announced triple. It reports whether either identity
// is present: an announcement with two empty strings carries nothing to verify.
func NewAnnouncement(id, file string, onDisk bool) (Announcement, bool) {
	announced := Announcement{ID: strings.TrimSpace(id), File: strings.TrimSpace(file), OnDisk: onDisk}
	return announced, announced.ID != "" || announced.File != ""
}

// Describe renders an announcement for a message a human reads.
func (a Announcement) Describe() string {
	state := "not materialized on disk"
	if a.OnDisk {
		state = "on disk"
	}
	switch {
	case a.ID != "" && a.File != "":
		return fmt.Sprintf("id %q / file %q (%s)", a.ID, a.File, state)
	case a.ID != "":
		return fmt.Sprintf("id %q (%s)", a.ID, state)
	case a.File != "":
		return fmt.Sprintf("file %q (%s)", a.File, state)
	default:
		return "no session"
	}
}

// AnnouncementOfRef is the single-identity announcement a reference describes. A
// caller that knows only Herdr's reported reference (the coordinator) uses it to
// ask whether that identity is the one that was confirmed.
func AnnouncementOfRef(ref SessionRef) (Announcement, bool) {
	normalized, ok := ref.Normalized()
	if !ok {
		return Announcement{}, false
	}
	if normalized.Kind == SessionKindID {
		return Announcement{ID: normalized.Value}, true
	}
	return Announcement{File: normalized.Value}, true
}

// NormalizeSessionKind validates a Herdr reference kind, comparing case
// insensitively because the installed build reports "PATH" against a schema that
// documents "path".
func NormalizeSessionKind(kind string) (string, bool) {
	switch {
	case strings.EqualFold(strings.TrimSpace(kind), SessionKindID):
		return SessionKindID, true
	case strings.EqualFold(strings.TrimSpace(kind), SessionKindPath):
		return SessionKindPath, true
	default:
		return "", false
	}
}

// Valid reports whether a reference carries a kind this build understands
// together with a value.
func (r SessionRef) Valid() bool {
	_, ok := NormalizeSessionKind(r.Kind)
	return ok && strings.TrimSpace(r.Value) != ""
}

// Normalized returns the reference with its kind in the canonical form.
func (r SessionRef) Normalized() (SessionRef, bool) {
	kind, ok := NormalizeSessionKind(r.Kind)
	if !ok || strings.TrimSpace(r.Value) == "" {
		return SessionRef{}, false
	}
	return SessionRef{Kind: kind, Value: strings.TrimSpace(r.Value)}, true
}

// Ref returns the announced identity for one kind, and whether the announcement
// carries it.
func (a Announcement) Ref(kind string) (SessionRef, bool) {
	switch canonical, ok := NormalizeSessionKind(kind); {
	case !ok:
		return SessionRef{}, false
	case canonical == SessionKindID:
		return SessionRef{Kind: canonical, Value: a.ID}, a.ID != ""
	default:
		return SessionRef{Kind: canonical, Value: a.File}, a.File != ""
	}
}

// Matches reports whether a reference is one of the identities the announcement
// carries, compared in the form the reference's own kind implies.
func (a Announcement) Matches(ref SessionRef) bool {
	normalized, ok := ref.Normalized()
	if !ok {
		return false
	}
	announced, ok := a.Ref(normalized.Kind)
	if !ok {
		return false
	}
	return SameSessionReference(normalized.Kind, normalized.Value, announced.Value)
}

// Proves reports whether an announcement makes one recorded session value and one
// reference Herdr reports now the same running OMP session.
//
// OMP exposes two native identities for one session — an id and the session file —
// and a record keeps whichever one Herdr reported when its handshake was
// confirmed, so the two values a caller holds may be in different forms. The
// announcement is the only thing that knows both, which makes it the only thing
// that can bridge them:
//
//  1. the recorded value has to be one of the identities this announcement carries,
//     compared in the form that identity's kind implies (an id as an id, a session
//     file as a path); and
//  2. Herdr's current reference has to be this announcement's identity for the
//     reference's own kind.
//
// Both halves are whole-value comparisons, so a value that merely appears inside
// another never matches, and an announcement that does not carry the form Herdr
// reported (or reports a kind this build cannot read) proves nothing. Callers use
// this instead of parsing an id out of a file name.
func (a Announcement) Proves(recorded string, ref SessionRef) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return false
	}
	if _, ok := ref.Normalized(); !ok {
		return false
	}
	matchesRecorded := (a.ID != "" && SameSessionReference(SessionKindID, a.ID, recorded)) ||
		(a.File != "" && SameSessionReference(SessionKindPath, a.File, recorded))
	if !matchesRecorded {
		return false
	}
	return a.Matches(ref)
}

// SameSessionReference compares two values in the form their kind implies: an id
// is trimmed and compared case-insensitively, and a path with the platform's path
// rules (Windows is case-insensitive and both separators are equivalent).
func SameSessionReference(kind, a, b string) bool {
	canonical, ok := NormalizeSessionKind(kind)
	if !ok {
		return false
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if canonical == SessionKindID {
		return strings.EqualFold(a, b)
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
