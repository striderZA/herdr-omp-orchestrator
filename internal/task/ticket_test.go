package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"testing"
)

func openTask(id, ticketID string) Task {
	return Task{ID: id, TicketID: ticketID, Phase: PromptSubmitted}
}

func closedTask(id, ticketID string) Task {
	x := openTask(id, ticketID)
	x.Report = &Report{Kind: "result", Summary: "done"}
	x.Outcome = OutcomeIntegrated
	x.Disposition = OutcomeIntegrated
	return x
}

func taskWithRole(t *testing.T, x Task, role string) Task {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"Role": role})
	if err != nil {
		t.Fatalf("encode task role: %v", err)
	}
	if err := json.Unmarshal(encoded, &x); err != nil {
		t.Fatalf("decode task role: %v", err)
	}
	return x
}

func TestTicketStageOf(t *testing.T) {
	backlog := Ticket{ID: "tkt_a", Column: ColumnBacklog}
	todo := Ticket{ID: "tkt_a", Column: ColumnTodo}
	decidedBacklog := Ticket{ID: "tkt_a", Column: ColumnBacklog, Outcome: OutcomeAbandoned, Disposition: OutcomeAbandoned}
	planner := taskWithRole(t, openTask("p1", "tkt_a"), "plan")
	planReady := planner
	planReady.Report = &Report{Kind: "result", Summary: "implementation plan"}
	acceptedPlanner := taskWithRole(t, closedTask("p2", "tkt_a"), "plan")
	rejectedPlanner := acceptedPlanner
	rejectedPlanner.Outcome, rejectedPlanner.Disposition = OutcomeRejected, OutcomeRejected
	for _, tc := range []struct {
		name   string
		ticket Ticket
		linked []Task
		want   TicketStage
	}{
		{"backlog without tasks", backlog, nil, TicketBacklog},
		{"todo without tasks", todo, nil, TicketTodo},
		{"todo with one open task", todo, []Task{openTask("o1", "tkt_a")}, TicketInProgress},
		{"todo with a closed and an open task", todo, []Task{closedTask("c1", "tkt_a"), openTask("o1", "tkt_a")}, TicketInProgress},
		{"todo with every implementation task closed", todo, []Task{closedTask("c1", "tkt_a"), closedTask("c2", "tkt_a")}, TicketReadyForReview},
		{"todo with an active planner", todo, []Task{planner}, TicketStage("planning")},
		{"todo with a planner report awaiting review", todo, []Task{planReady}, TicketStage("plan_ready")},
		{"accepted plan with no implementation tasks", todo, []Task{acceptedPlanner}, TicketTodo},
		{"accepted plan and open implementation task", todo, []Task{acceptedPlanner, openTask("o1", "tkt_a")}, TicketInProgress},
		{"plan report takes precedence while implementation is open", todo, []Task{planReady, openTask("o1", "tkt_a")}, TicketStage("plan_ready")},
		{"rejected plan with no implementation tasks", todo, []Task{rejectedPlanner}, TicketTodo},
		{"decided backlog ticket with an open task", decidedBacklog, []Task{openTask("o1", "tkt_a")}, TicketDone},
		{"backlog ticket with an open task", backlog, []Task{openTask("o1", "tkt_a")}, TicketInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TicketStageOf(tc.ticket, tc.linked); got != tc.want {
				t.Fatalf("TicketStageOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTicketDecidedReadsTheDispositionFact(t *testing.T) {
	if (Ticket{}).Decided() {
		t.Fatal("a ticket with no disposition is not decided")
	}
	if !(Ticket{Outcome: OutcomeKept, Disposition: OutcomeKept}).Decided() {
		t.Fatal("a ticket with a disposition is decided")
	}
}

func TestTicketDigest(t *testing.T) {
	base := Ticket{ID: "tkt_a", Revision: 3, Title: "title", Body: "body"}
	if base.Digest() != (Ticket{ID: "tkt_a", Revision: 3, Title: "title", Body: "body"}).Digest() {
		t.Fatal("equal ID, Revision and Title must give equal digests")
	}
	for name, changed := range map[string]Ticket{
		"id":       {ID: "tkt_b", Revision: 3, Title: "title"},
		"revision": {ID: "tkt_a", Revision: 4, Title: "title"},
		"title":    {ID: "tkt_a", Revision: 3, Title: "other"},
	} {
		if changed.Digest() == base.Digest() {
			t.Fatalf("changing the %s must change the digest", name)
		}
	}
	if (Ticket{ID: "tkt_a", Revision: 3, Title: "title", Body: "another body"}).Digest() != base.Digest() {
		t.Fatal("the body is not part of the digest")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(base.Digest()) {
		t.Fatalf("digest %q is not lowercase hex SHA-256", base.Digest())
	}
}

func TestTicketDigestIsTheHashOfAJSONArray(t *testing.T) {
	sum := sha256.Sum256([]byte(`["tkt_a",3,"a \"quoted\" title"]`))
	want := hex.EncodeToString(sum[:])
	if got := (Ticket{ID: "tkt_a", Revision: 3, Title: `a "quoted" title`}).Digest(); got != want {
		t.Fatalf("Digest = %s, want the SHA-256 of the canonical JSON array %s", got, want)
	}
}

func TestTicketDigestHasNoFieldBoundaryCollisions(t *testing.T) {
	// Raw concatenation would turn both of these into "a12b".
	if (Ticket{ID: "a", Revision: 12, Title: "b"}).Digest() == (Ticket{ID: "a1", Revision: 2, Title: "b"}).Digest() {
		t.Fatal("ids and revisions that concatenate to the same text must not collide")
	}
	// A title that merely contains the delimiter and quote characters.
	if (Ticket{ID: "a", Revision: 1, Title: `x","y`}).Digest() == (Ticket{ID: "a", Revision: 1, Title: "x"}).Digest() {
		t.Fatal("a title with quote and comma characters must not collide with a shorter title")
	}
}

func TestLinkedTasksMatchesOnlyTheExactTicketID(t *testing.T) {
	all := []Task{
		openTask("t1", ""),
		openTask("t2", "tkt_a"),
		openTask("t3", "tkt_ab"),
		openTask("t4", ""),
	}
	if got := LinkedTasks(all, ""); len(got) != 0 {
		t.Fatalf("an empty ticket id must match nothing, got %d tasks", len(got))
	}
	got := LinkedTasks(all, "tkt_a")
	if len(got) != 1 || got[0].ID != "t2" {
		t.Fatalf("tkt_a must not match tkt_ab: %+v", got)
	}
}

func TestTaskWithoutTicketIDDecodesAsUnlinked(t *testing.T) {
	var x Task
	if err := json.Unmarshal([]byte(`{"ID":"orch_x"}`), &x); err != nil {
		t.Fatal(err)
	}
	if x.ID != "orch_x" || x.TicketID != "" {
		t.Fatalf("a stored task without a TicketID must read as unlinked: %+v", x)
	}
}
