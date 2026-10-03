package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestBoardOptionsWireTicketWritesThroughTheCoordinator(t *testing.T) {
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
	// Ticket writes never call Herdr, so any executable path is a valid client.
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
	if err := ops.MoveTicketToTodo(ctx, project.Key, tickets[0].ID, tickets[0].Digest()); err != nil {
		t.Fatalf("MoveTicketToTodo with the displayed digest: %v", err)
	}
	if after, _ := store.Ticket(ctx, tickets[0].ID); after.Column != task.ColumnTodo {
		t.Fatalf("the move must record todo: %+v", after)
	}
	if tasks, _ := store.Tasks(ctx, project.Key); len(tasks) != 0 {
		t.Fatalf("a move starts no task: %+v", tasks)
	}

	// The project is the caller's resolved one: another key is not a project.
	if err := ops.CreateTicket(ctx, `C:\elsewhere`, "x"); !errors.Is(err, coord.ErrProjectUnknown) {
		t.Fatalf("an unbound project = %v, want coord.ErrProjectUnknown", err)
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
