package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"herdr-omp-orchestrator/internal/task"
)

// The primary's read surface over its own project: the tasks it dispatched and
// the inbox notes recorded for it. Like the ticket operations they are keyed by a
// project key the bridge already proved, never one from a request body, they
// touch neither Herdr nor a worktree, and they write nothing: reading a note
// claims no delivery and settles none, so an `uncertain` note stays uncertain.
//
// Every answer is bounded three ways, because the bridge replaces an answer over
// its response bound with an error instead of truncating it: a row count, a
// rune bound on each free-text field, and a byte budget on the whole list that
// accounts for JSON escaping. A list that was cut says so (Truncated) and always
// reports the Total it was cut from.
const (
	// InspectDefaultLimit is the row count a list returns when it is not told one.
	InspectDefaultLimit = 50
	// InspectMaxLimit is the largest row count a list may ask for.
	InspectMaxLimit = 200

	inspectTextLimit   = 1000
	inspectDetailLimit = 2000
	inspectByteBudget  = 512 << 10
)

// TaskListParams is the task_list request body. Both fields are optional.
type TaskListParams struct {
	// Stage keeps only the tasks in one derived stage.
	Stage string `json:"stage"`
	// Limit is the most rows to return; zero means InspectDefaultLimit.
	Limit int `json:"limit"`
}

// TaskEntry is one task of the project as recorded: its derived stage and the
// facts that explain it. Activity is Herdr's own word for what the terminal is
// doing now and never decides the stage.
type TaskEntry struct {
	TaskID      string     `json:"taskId"`
	Title       string     `json:"title"`
	Stage       task.Stage `json:"stage"`
	Activity    string     `json:"activity,omitempty"`
	Blocker     string     `json:"blocker,omitempty"`
	Attention   string     `json:"attention,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
	Disposition string     `json:"disposition,omitempty"`
	Successor   string     `json:"successor,omitempty"`
	TicketID    string     `json:"ticketId,omitempty"`
}

// TaskListResult is the task_list answer. Total counts the tasks that matched the
// filter, so a cut list still says how much was left out.
type TaskListResult struct {
	Tasks     []TaskEntry `json:"tasks"`
	Total     int         `json:"total"`
	Truncated bool        `json:"truncated"`
}

// InboxListParams is the inbox_list request body. Both fields are optional.
type InboxListParams struct {
	// State keeps only the notes in one delivery state.
	State string `json:"state"`
	// Limit is the most rows to return; zero means InspectDefaultLimit.
	Limit int `json:"limit"`
}

// InboxEntry is one inbox note with the delivery fact it carries. Label is the
// reason the note exists; DeliveryState is where its journey to the primary
// stands, and DeliveryDetail names the pane that took it or why an attempt did
// not settle - the only durable account of an `uncertain` note.
type InboxEntry struct {
	NoteID         string `json:"noteId"`
	TaskID         string `json:"taskId,omitempty"`
	Kind           string `json:"kind"`
	Label          string `json:"label,omitempty"`
	Summary        string `json:"summary"`
	DeliveryState  string `json:"deliveryState"`
	DeliveryDetail string `json:"deliveryDetail,omitempty"`
}

// InboxListResult is the inbox_list answer. Notice repeats the origin label of
// the inbox prompt: the notes are text workers and the orchestrator recorded.
type InboxListResult struct {
	Notice    string       `json:"notice"`
	Notes     []InboxEntry `json:"notes"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
}

// ListTasksFor lists the tasks of one project. Tasks that are still open come
// first, oldest first; decided tasks follow, newest first. When the list has to
// be cut, it is the oldest decided history that goes, never a task that is
// waiting for someone.
func (c *Coordinator) ListTasksFor(ctx context.Context, projectKey string, p TaskListParams) (TaskListResult, error) {
	limit, err := inspectLimit(p.Limit)
	if err != nil {
		return TaskListResult{}, err
	}
	stage := strings.TrimSpace(p.Stage)
	if stage != "" && !slices.Contains(taskStages(), task.Stage(stage)) {
		return TaskListResult{}, fmt.Errorf("%w: %q is not a task stage (want one of %s)",
			ErrInvalidRequest, stage, joinStages(taskStages()))
	}
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return TaskListResult{}, err
	}
	tasks, err := c.projectTasks(ctx, project.Key)
	if err != nil {
		return TaskListResult{}, err
	}

	var open, decided []task.Task
	for _, x := range tasks {
		switch got := task.StageOf(x); {
		case stage != "" && string(got) != stage:
		case got == task.Done:
			decided = append(decided, x)
		default:
			open = append(open, x)
		}
	}
	slices.Reverse(decided)
	ordered := append(open, decided...)

	entries := make([]TaskEntry, 0, min(limit, len(ordered)))
	for _, x := range ordered {
		entries = append(entries, taskEntry(x))
	}
	kept := boundedPrefix(entries, limit)
	return TaskListResult{Tasks: entries[:kept], Total: len(ordered), Truncated: kept < len(ordered)}, nil
}

// ListInboxFor lists the inbox notes of one project with their delivery state
// and detail. Notes that have not been delivered (pending, sending, uncertain)
// come first, oldest first; delivered notes follow, newest first. Like a task
// list, a cut list loses delivered history before anything undelivered.
func (c *Coordinator) ListInboxFor(ctx context.Context, projectKey string, p InboxListParams) (InboxListResult, error) {
	limit, err := inspectLimit(p.Limit)
	if err != nil {
		return InboxListResult{}, err
	}
	want := strings.TrimSpace(p.State)
	if want != "" && !slices.Contains(deliveryStates(), want) {
		return InboxListResult{}, fmt.Errorf("%w: %q is not a delivery state (want one of %s)",
			ErrInvalidRequest, want, strings.Join(deliveryStates(), ", "))
	}
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return InboxListResult{}, err
	}
	notes, err := c.store.ProjectInbox(ctx, project.Key)
	if err != nil {
		return InboxListResult{}, fmt.Errorf("read the inbox of %q: %w", project.Key, err)
	}

	var undelivered, delivered []task.Inbox
	for _, note := range notes {
		switch got := note.State(); {
		case want != "" && got != want:
		case got == task.DeliveryDelivered:
			delivered = append(delivered, note)
		default:
			undelivered = append(undelivered, note)
		}
	}
	slices.Reverse(delivered)
	ordered := append(undelivered, delivered...)

	entries := make([]InboxEntry, 0, min(limit, len(ordered)))
	for _, note := range ordered {
		entries = append(entries, inboxEntry(note))
	}
	kept := boundedPrefix(entries, limit)
	return InboxListResult{
		Notice:    inboxOriginLabel,
		Notes:     entries[:kept],
		Total:     len(ordered),
		Truncated: kept < len(ordered),
	}, nil
}

// inspectLimit resolves a requested row count. Zero asks for the default; a
// count the list cannot honor is refused instead of being quietly changed.
func inspectLimit(requested int) (int, error) {
	switch {
	case requested == 0:
		return InspectDefaultLimit, nil
	case requested < 0 || requested > InspectMaxLimit:
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRequest, InspectMaxLimit)
	}
	return requested, nil
}

// boundedPrefix returns how many leading entries fit: at most limit rows and at
// most inspectByteBudget bytes of JSON. Each entry is measured as the bridge will
// encode it, so escaping cannot push an answer past the response bound.
func boundedPrefix[T any](entries []T, limit int) int {
	used := 0
	for i, entry := range entries {
		if i >= limit {
			return i
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return i
		}
		used += len(encoded) + 1
		if used > inspectByteBudget {
			return i
		}
	}
	return len(entries)
}

func taskEntry(x task.Task) TaskEntry {
	return TaskEntry{
		TaskID:      x.ID,
		Title:       truncateRunes(x.Title, inspectTextLimit),
		Stage:       task.StageOf(x),
		Activity:    truncateRunes(strings.TrimSpace(x.Activity), inspectTextLimit),
		Blocker:     truncateRunes(strings.TrimSpace(x.Blocker), inspectTextLimit),
		Attention:   truncateRunes(strings.TrimSpace(x.Attention), inspectTextLimit),
		Outcome:     x.Outcome,
		Disposition: truncateRunes(x.Disposition, inspectTextLimit),
		Successor:   x.Successor,
		TicketID:    x.TicketID,
	}
}

func inboxEntry(note task.Inbox) InboxEntry {
	return InboxEntry{
		NoteID:         note.ID,
		TaskID:         note.TaskID,
		Kind:           note.Kind,
		Label:          truncateRunes(note.Delivery, inspectTextLimit),
		Summary:        truncateRunes(strings.TrimSpace(note.Summary), inspectTextLimit),
		DeliveryState:  note.State(),
		DeliveryDetail: truncateRunes(note.DeliveryDetail, inspectDetailLimit),
	}
}

// taskStages lists the stages a task_list filter accepts, in board order.
func taskStages() []task.Stage {
	return []task.Stage{task.Queued, task.Running, task.NeedsInput, task.ReadyForReview, task.Done}
}

func joinStages(stages []task.Stage) string {
	names := make([]string, 0, len(stages))
	for _, s := range stages {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// deliveryStates lists the states an inbox_list filter accepts.
func deliveryStates() []string {
	return []string{task.DeliveryPending, task.DeliverySending, task.DeliveryDelivered, task.DeliveryUncertain}
}
