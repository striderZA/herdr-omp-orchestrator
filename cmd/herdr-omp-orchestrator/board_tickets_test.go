package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestBoardOptionsRouteTicketMovesThroughCoordinatorPreflight(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project := task.Project{
		Key:                `C:\work\demo-repo`,
		RepoWorkspaceID:    "w1",
		CurrentWorkspaceID: "w1",
		LaunchPaneID:       "w1:p1",
		PublicPaneID:       "w1:p1",
		AgentName:          "orch_p_fixture",
		OMPSessionID:       "primary-session",
	}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// A move now starts a planner, so this test uses a nonexistent checkout and
	// proves coordinator preflight refuses before any Herdr operation.
	client, err := herdr.NewClient(exe)
	if err != nil {
		t.Fatalf("herdr.NewClient: %v", err)
	}

	var ops board.TicketOps = boardOptions(store, client).Tickets
	if ops == nil {
		t.Fatal("the board process must wire ticket writes")
	}
	if err := ops.CreateTicket(ctx, project.Key, "Add search"); err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	tickets, err := store.Tickets(ctx, project.Key)
	if err != nil || len(tickets) != 1 || tickets[0].Title != "Add search" || tickets[0].Column != task.ColumnBacklog {
		t.Fatalf("the created ticket = %+v, %v; want one backlog ticket for the resolved project", tickets, err)
	}

	// The digest is the coordinator's own: a stale one is refused with the
	// coordinator's code, and nothing moved.
	if err := ops.MoveTicketToTodo(ctx, project.Key, tickets[0].ID, "not-the-digest"); !errors.Is(err, coord.ErrTicketChanged) {
		t.Fatalf("a stale digest = %v, want coord.ErrTicketChanged", err)
	}
	if after, _ := store.Ticket(ctx, tickets[0].ID); after.Column != task.ColumnBacklog || after.Revision != 1 {
		t.Fatalf("a refused move must write nothing: %+v", after)
	}
	if err := ops.MoveTicketToTodo(ctx, project.Key, tickets[0].ID, tickets[0].Digest()); !errors.Is(err, coord.ErrBaseRevision) {
		t.Fatalf("a move without a committed project HEAD = %v, want coord.ErrBaseRevision", err)
	}
	if after, _ := store.Ticket(ctx, tickets[0].ID); after.Column != task.ColumnBacklog || after.Revision != 1 {
		t.Fatalf("a move without a committed project HEAD must leave the ticket unchanged: %+v", after)
	}
	if tasks, _ := store.Tasks(ctx, project.Key); len(tasks) != 0 {
		t.Fatalf("a refused move must leave no planner task: %+v", tasks)
	}

	// The project is the caller's resolved one: another key is not a project.
	if err := ops.CreateTicket(ctx, `C:\elsewhere`, "x"); !errors.Is(err, coord.ErrProjectUnknown) {
		t.Fatalf("an unbound project = %v, want coord.ErrProjectUnknown", err)
	}
}

// The executable is where the coordinator's answer becomes the board's: a move
// the coordinator recorded but whose planner did not start reaches the board as a
// recorded move with its cause, while every other error stays a refusal.
func TestBoardMoveErrorKeepsARecordedMoveRecorded(t *testing.T) {
	cause := errors.New("create the worktree: herdr: not_git_worktree")
	var recorded *board.MovedTicketError
	got := boardMoveError(fmt.Errorf("move: %w", &coord.PlannerNotStartedError{TicketID: "tkt_a", TaskID: "orch_p", Err: cause}))
	if !errors.As(got, &recorded) || recorded.Err != cause {
		t.Fatalf("a recorded move whose planner did not start = %#v, want a board.MovedTicketError carrying its cause", got)
	}
	refusal := fmt.Errorf("%w: ticket tkt_a", coord.ErrTicketChanged)
	if got := boardMoveError(refusal); got != refusal {
		t.Fatalf("a refused move = %#v, want the coordinator's refusal unchanged", got)
	}
	if got := boardMoveError(nil); got != nil {
		t.Fatalf("a move that started its planner = %v, want no error", got)
	}
}

func TestBoardOptionsFallBackToAReadOnlyBoardWithoutACoordinator(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// coord.New refuses a missing Herdr client; the board must still open, read-only.
	if got := boardOptions(store, nil); got.Tickets != nil {
		t.Fatal("without a coordinator the board must be read-only, not hold a broken writer")
	}
}
