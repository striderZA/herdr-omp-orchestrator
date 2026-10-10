package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Column is the one place a ticket's own record says it sits. It is a stored
// fact, set by a human or by a primary acting for one; every later stage of a
// ticket is derived from the tasks linked to it (see TicketStageOf) and is
// never stored.
type Column string

const (
	// ColumnBacklog holds a ticket that has been written down but not started.
	ColumnBacklog Column = "backlog"
	// ColumnTodo holds a ticket a human marked ready for work; moving it starts or
	// queues the ticket's planner task.
	ColumnTodo Column = "todo"
)

// Ticket is the durable record of one piece of intended work. It exists before
// any Herdr resource, so it is keyed by its own id, not by a worktree or pane.
//
// Outcome and Disposition are the user's decision about the ticket, with the
// same meaning as on a Task (ADR 0001): a non-empty Disposition closes it.
type Ticket struct {
	ID, ProjectKey, Title, Body string
	Column                      Column
	Outcome, Disposition        string
	// Successor names the ticket that replaces this one when the decision is
	// superseded (issue #61). It is a reference, not a dependency: no linked task
	// moves to it. It stays empty for every other decision and for a legacy row.
	Successor                      string `json:",omitempty"`
	Revision, CreatedAt, UpdatedAt int64
}

// TicketStage is where a ticket appears on the board. It is a projection of the
// ticket's Column, its decision and its linked tasks, never a stored fact, and
// like Stage it is never derived from terminal activity.
type TicketStage string

const (
	TicketBacklog        TicketStage = "backlog"
	TicketTodo           TicketStage = "todo"
	TicketPlanning       TicketStage = "planning"
	TicketPlanReady      TicketStage = "plan_ready"
	TicketInProgress     TicketStage = "in_progress"
	TicketReadyForReview TicketStage = "ready_for_review"
	TicketDone           TicketStage = "done"
)

// Decided reports whether the user has recorded a decision for the ticket.
func (t Ticket) Decided() bool { return t.Disposition != "" }

// Digest is the confirmation token a caller echoes back to move or review a
// ticket: the lowercase hex SHA-256 of the JSON array [ID, Revision, Title].
//
// Each field is delimited and escaped by the JSON encoding, so two different
// triples can never produce the same bytes the way plain concatenation would
// ("a"+12+"b" and "a1"+2+"b" are both "a12b"). The body is deliberately not
// part of it: the digest vouches for what the user was shown to confirm, which
// is which ticket, at which revision, called what.
func (t Ticket) Digest() string {
	// Marshalling a string, an int64 and a string cannot fail.
	encoded, _ := json.Marshal([]any{t.ID, t.Revision, t.Title})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// LinkedTasks returns the tasks dispatched for one ticket, in the order given.
// The match is the whole ticket id, so "tkt_a" never matches "tkt_ab", and an
// empty id matches nothing (every unlinked task has an empty TicketID).
func LinkedTasks(all []Task, ticketID string) []Task {
	if ticketID == "" {
		return nil
	}
	var linked []Task
	for _, x := range all {
		if x.TicketID == ticketID {
			linked = append(linked, x)
		}
	}
	return linked
}

// TicketStageOf projects a ticket and its linked tasks onto a stage. A decision
// wins; otherwise an active planner task wins, then implementation tasks, then
// the ticket's stored column. Planner tasks never count as implementation tasks.
func TicketStageOf(t Ticket, linked []Task) TicketStage {
	if t.Decided() {
		return TicketDone
	}
	implementationTasks, openImplementations := 0, 0
	planning, planReady := false, false
	for _, x := range linked {
		if x.Role == TaskRolePlan {
			if x.Disposition != "" {
				continue
			}
			if x.Report != nil && x.Report.Kind == "result" {
				planReady = true
			} else {
				planning = true
			}
			continue
		}
		implementationTasks++
		if x.Disposition == "" {
			openImplementations++
		}
	}
	switch {
	case planning:
		return TicketPlanning
	case planReady:
		return TicketPlanReady
	case implementationTasks > 0 && openImplementations == 0:
		return TicketReadyForReview
	case openImplementations > 0:
		return TicketInProgress
	case t.Column == ColumnTodo:
		return TicketTodo
	default:
		return TicketBacklog
	}
}
