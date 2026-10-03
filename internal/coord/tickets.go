package coord

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// Ticket operations. Like DispatchFor they are keyed by a project key that the
// caller (the bridge, or the board) has already proven, never one taken from a
// request body: every operation re-reads the project, and a ticket is only ever
// reached through that project. None of them touches Herdr, a worktree or a
// task. A ticket move records a fact; it starts nothing.

var (
	// ErrTicketUnknown reports a ticket id that does not exist in the caller's
	// project. A missing id and another project's id are deliberately the same
	// answer, so an id cannot be probed across projects.
	ErrTicketUnknown = errors.New("coord: unknown ticket")
	// ErrTicketChanged reports a confirmation digest that no longer matches the
	// ticket: it was moved, decided or edited after the preview the user saw.
	ErrTicketChanged = errors.New("coord: the ticket changed after its preview")
	// ErrTicketOpen reports a decision for a ticket that still has a linked task
	// without a disposition.
	ErrTicketOpen = errors.New("coord: the ticket still has open tasks")
)

const ticketIDPrefix = "tkt_"

// TicketCreateParams is the ticket_create request body.
type TicketCreateParams struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// TicketRefParams names one ticket (ticket_move_preview).
type TicketRefParams struct {
	TicketID string `json:"ticketId"`
}

// TicketMoveParams is the ticket_move request body. Digest is the confirmation
// token the preview returned.
type TicketMoveParams struct {
	TicketID string `json:"ticketId"`
	Digest   string `json:"digest"`
}

// TicketReviewParams is the ticket_review request body: the digest the preview
// returned and the user's decision, with the same outcomes as a task decision.
type TicketReviewParams struct {
	TicketID    string `json:"ticketId"`
	Digest      string `json:"digest"`
	Outcome     string `json:"outcome"`
	Disposition string `json:"disposition"`
}

// TicketView is the durable facts of one ticket plus its derived stage and the
// tasks linked to it. Digest is the confirmation token for the facts shown.
type TicketView struct {
	TicketID    string           `json:"ticketId"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	Column      task.Column      `json:"column"`
	Stage       task.TicketStage `json:"stage"`
	Revision    int64            `json:"revision"`
	Digest      string           `json:"digest"`
	TaskIDs     []string         `json:"taskIds"`
	Outcome     string           `json:"outcome,omitempty"`
	Disposition string           `json:"disposition,omitempty"`
}

// TicketListResult is the ticket_list answer.
type TicketListResult struct {
	Tickets []TicketView `json:"tickets"`
}

// CreateTicketFor records a new ticket in the backlog of an already authorized
// project. A title is required; the body may be empty. The bounds are
// dispatch's, so a ticket can always be dispatched as a task title and brief.
func (c *Coordinator) CreateTicketFor(ctx context.Context, projectKey string, p TicketCreateParams) (TicketView, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return TicketView{}, err
	}
	title, body := strings.TrimSpace(p.Title), strings.TrimSpace(p.Body)
	switch {
	case title == "":
		return TicketView{}, fmt.Errorf("%w: a ticket title is required", ErrInvalidRequest)
	case len(title) > maxTitleLength:
		return TicketView{}, fmt.Errorf("%w: the ticket title exceeds %d characters", ErrInvalidRequest, maxTitleLength)
	case len(body) > maxBriefLength:
		return TicketView{}, fmt.Errorf("%w: the ticket body exceeds %d characters", ErrInvalidRequest, maxBriefLength)
	}

	var lastErr error
	for attempt := 0; attempt < idAttempts; attempt++ {
		id, err := c.newTicketID()
		if err != nil {
			return TicketView{}, err
		}
		ticket := task.Ticket{ID: id, ProjectKey: project.Key, Title: title, Body: body, Column: task.ColumnBacklog}
		err = c.store.CreateTicket(ctx, ticket)
		if err == nil {
			stored, readErr := c.store.Ticket(ctx, id)
			if readErr != nil {
				return TicketView{}, fmt.Errorf("read the new ticket %q: %w", id, readErr)
			}
			return c.ticketView(ctx, stored)
		}
		if !errors.Is(err, state.ErrConflict) {
			return TicketView{}, fmt.Errorf("record ticket %q: %w", id, err)
		}
		lastErr = err
	}
	return TicketView{}, fmt.Errorf("record a ticket: %w", lastErr)
}

// ListTicketsFor lists the tickets of one project, oldest first, each with its
// derived stage and linked task ids.
func (c *Coordinator) ListTicketsFor(ctx context.Context, projectKey string) (TicketListResult, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return TicketListResult{}, err
	}
	tickets, err := c.store.Tickets(ctx, project.Key)
	if err != nil {
		return TicketListResult{}, err
	}
	tasks, err := c.projectTasks(ctx, project.Key)
	if err != nil {
		return TicketListResult{}, err
	}
	result := TicketListResult{Tickets: make([]TicketView, 0, len(tickets))}
	for _, ticket := range tickets {
		result.Tickets = append(result.Tickets, viewOfTicket(ticket, tasks))
	}
	return result, nil
}

// TicketPreviewFor reads one ticket and its confirmation digest. It writes
// nothing; the user is shown what it returns before a move or a decision.
func (c *Coordinator) TicketPreviewFor(ctx context.Context, projectKey, ticketID string) (TicketView, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return TicketView{}, err
	}
	ticket, err := c.ownedTicket(ctx, project.Key, ticketID)
	if err != nil {
		return TicketView{}, err
	}
	return c.ticketView(ctx, ticket)
}

// MoveTicketFor moves a backlog ticket to todo, but only when digest is the
// confirmation token of the ticket exactly as it stands now. The move records a
// fact and starts nothing: no task, worktree, agent or Herdr call.
//
// The checks run in a fixed order so each refusal names the real reason: unknown
// ticket, blank digest, an already decided ticket, a digest that no longer
// matches, and last a ticket that is not in the backlog. Two concurrent moves
// with the same digest have one winner: the store compare-and-swaps on the
// revision the digest vouched for, and the loser gets ErrTicketChanged.
func (c *Coordinator) MoveTicketFor(ctx context.Context, projectKey, ticketID, digest string) (TicketView, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return TicketView{}, err
	}
	ticket, err := c.ownedTicket(ctx, project.Key, ticketID)
	if err != nil {
		return TicketView{}, err
	}
	digest = strings.TrimSpace(digest)
	switch {
	case digest == "":
		return TicketView{}, fmt.Errorf("%w: the confirmation digest from the ticket preview is required", ErrInvalidRequest)
	case ticket.Decided():
		return TicketView{}, ticketAlreadyDecided(ticket)
	case digest != ticket.Digest():
		return TicketView{}, fmt.Errorf("%w: ticket %s", ErrTicketChanged, ticket.ID)
	case ticket.Column != task.ColumnBacklog:
		return TicketView{}, fmt.Errorf("%w: ticket %s is already in %s", ErrInvalidRequest, ticket.ID, ticket.Column)
	}
	moved, err := c.store.UpdateTicket(ctx, ticket.ID, ticket.Revision, func(next *task.Ticket) error {
		next.Column = task.ColumnTodo
		return nil
	})
	if errors.Is(err, state.ErrConflict) {
		return TicketView{}, fmt.Errorf("%w: ticket %s was changed by another writer", ErrTicketChanged, ticket.ID)
	}
	if err != nil {
		return TicketView{}, err
	}
	return c.ticketView(ctx, moved)
}

// ReviewTicketFor records the user's decision for a ticket. It mirrors a task's
// review: the outcome is validated first, a repeat of the recorded decision is an
// idempotent success (so a lost response is safe to retry, even after the digest
// has moved on), any other decision for a decided ticket is ErrAlreadyDecided,
// and an undecided ticket needs the digest of the facts the user saw.
//
// Whether a linked task is still open is decided by the store, inside the
// transaction that writes the decision, never by a read here: a dispatch landing
// between a read and the write would otherwise slip through.
func (c *Coordinator) ReviewTicketFor(ctx context.Context, projectKey string, p TicketReviewParams) (TicketView, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return TicketView{}, err
	}
	ticket, err := c.ownedTicket(ctx, project.Key, p.TicketID)
	if err != nil {
		return TicketView{}, err
	}
	kind, note, err := decisionOf(p.Outcome, p.Disposition)
	if err != nil {
		return TicketView{}, err
	}
	if ticket.Decided() {
		if sameTicketDecision(ticket, kind, note) {
			return c.ticketView(ctx, ticket)
		}
		return TicketView{}, ticketAlreadyDecided(ticket)
	}
	digest := strings.TrimSpace(p.Digest)
	switch {
	case digest == "":
		return TicketView{}, fmt.Errorf("%w: the confirmation digest from the ticket preview is required", ErrInvalidRequest)
	case digest != ticket.Digest():
		return TicketView{}, fmt.Errorf("%w: ticket %s", ErrTicketChanged, ticket.ID)
	}
	decided, err := c.store.DecideTicket(ctx, ticket.ID, ticket.Revision, kind, note)
	switch {
	case errors.Is(err, state.ErrTicketHasOpenTasks):
		return TicketView{}, fmt.Errorf("%w: ticket %s", ErrTicketOpen, ticket.ID)
	case errors.Is(err, state.ErrConflict):
		// Another writer landed first. If it recorded this same decision the
		// request has succeeded; a different one stands; anything else changed
		// the ticket under the digest.
		current, readErr := c.store.Ticket(ctx, ticket.ID)
		if readErr == nil && current.Decided() {
			if sameTicketDecision(current, kind, note) {
				return c.ticketView(ctx, current)
			}
			return TicketView{}, ticketAlreadyDecided(current)
		}
		return TicketView{}, fmt.Errorf("%w: ticket %s was changed by another writer", ErrTicketChanged, ticket.ID)
	case err != nil:
		return TicketView{}, err
	}
	return c.ticketView(ctx, decided)
}

// ownedTicket reads a ticket of the given project. A ticket that does not exist
// and a ticket of another project are the same ErrTicketUnknown.
func (c *Coordinator) ownedTicket(ctx context.Context, projectKey, id string) (task.Ticket, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return task.Ticket{}, fmt.Errorf("%w: no ticket id was given", ErrInvalidRequest)
	}
	ticket, err := c.store.Ticket(ctx, id)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Ticket{}, fmt.Errorf("%w: %s", ErrTicketUnknown, id)
	case err != nil:
		return task.Ticket{}, err
	case ticket.ProjectKey != projectKey:
		return task.Ticket{}, fmt.Errorf("%w: %s", ErrTicketUnknown, id)
	}
	return ticket, nil
}

func (c *Coordinator) ticketView(ctx context.Context, ticket task.Ticket) (TicketView, error) {
	tasks, err := c.projectTasks(ctx, ticket.ProjectKey)
	if err != nil {
		return TicketView{}, err
	}
	return viewOfTicket(ticket, tasks), nil
}

// viewOfTicket projects a ticket and the project's tasks onto the answer. The
// stage is derived here and never stored; linked task ids keep the store's
// oldest-first order.
func viewOfTicket(ticket task.Ticket, tasks []task.Task) TicketView {
	linked := task.LinkedTasks(tasks, ticket.ID)
	ids := make([]string, 0, len(linked))
	for _, x := range linked {
		ids = append(ids, x.ID)
	}
	return TicketView{
		TicketID:    ticket.ID,
		Title:       ticket.Title,
		Body:        ticket.Body,
		Column:      ticket.Column,
		Stage:       task.TicketStageOf(ticket, linked),
		Revision:    ticket.Revision,
		Digest:      ticket.Digest(),
		TaskIDs:     ids,
		Outcome:     ticket.Outcome,
		Disposition: ticket.Disposition,
	}
}

func sameTicketDecision(ticket task.Ticket, kind, note string) bool {
	return ticket.Outcome == kind && ticket.Disposition == note
}

// ticketDecidedError is ErrAlreadyDecided for a ticket. It matches the sentinel,
// so errors.Is and the bridge's already_decided code are unchanged, but its text
// names the ticket: the sentinel's own wording is about a worker report, which
// is not what a refused ticket move, decision or dispatch is about.
type ticketDecidedError struct{ id, outcome, note string }

func (e ticketDecidedError) Error() string {
	return fmt.Sprintf("coord: ticket %s is already decided: %q (%s)", e.id, e.note, e.outcome)
}

func (e ticketDecidedError) Is(target error) bool { return target == ErrAlreadyDecided }

func ticketAlreadyDecided(ticket task.Ticket) error {
	return ticketDecidedError{id: ticket.ID, outcome: ticket.Outcome, note: ticket.Disposition}
}

// newTicketID returns a fresh ticket id: tkt_ and 12 hexadecimal characters.
// Titles are never used as identities.
func (c *Coordinator) newTicketID() (string, error) {
	random := make([]byte, taskIDBytes)
	if err := c.random(random); err != nil {
		return "", fmt.Errorf("generate a ticket identity: %w", err)
	}
	return ticketIDPrefix + hex.EncodeToString(random), nil
}

// checkTicketForDispatch is the coordinator's read of "can this ticket take new
// work": it exists in the project and is not decided. Dispatch runs it before it
// writes anything and again when the store's gate refuses, so both paths give
// the same answer.
func (c *Coordinator) checkTicketForDispatch(ctx context.Context, projectKey, ticketID string) error {
	ticket, err := c.ownedTicket(ctx, projectKey, ticketID)
	if err != nil {
		return err
	}
	if ticket.Decided() {
		return ticketAlreadyDecided(ticket)
	}
	return nil
}
