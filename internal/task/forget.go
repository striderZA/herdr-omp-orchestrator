package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ForgetFacts is the durable project state relevant to planning a forget.
type ForgetFacts struct {
	Project        Project
	Tasks          []Task
	ClaimedTaskIDs []string // tasks with a provisioning slot or a step claim
	Notes          []Inbox
	Tickets        []Ticket
	BindIntentOpen bool // a bind_intents row exists for Project.Key
}

// ForgetBlockers returns refusal reasons in a stable order, or nil when the
// project may be forgotten.
func ForgetBlockers(f ForgetFacts) []string {
	var blockers []string

	undecided := make([]Task, 0)
	for _, task := range f.Tasks {
		if task.Disposition == "" {
			undecided = append(undecided, task)
		}
	}
	sort.Slice(undecided, func(i, j int) bool { return undecided[i].ID < undecided[j].ID })
	for _, task := range undecided {
		blockers = append(blockers, "task "+task.ID+" has no outcome: record one first, such as abandoned")
	}

	claimed := append([]string(nil), f.ClaimedTaskIDs...)
	sort.Strings(claimed)
	for _, id := range claimed {
		blockers = append(blockers, "task "+id+" is claimed and must finish first")
	}

	for _, state := range []string{DeliverySending, DeliveryUncertain} {
		notes := make([]Inbox, 0)
		for _, note := range f.Notes {
			if note.State() == state {
				notes = append(notes, note)
			}
		}
		sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
		for _, note := range notes {
			blockers = append(blockers, "note "+note.ID+" is "+state)
		}
	}

	if f.BindIntentOpen {
		blockers = append(blockers, "bind intent for project "+f.Project.Key+" is open")
	}
	if f.Project.ReattachNotice.State == NoticeUncertain {
		blockers = append(blockers, "reattach notice for pane "+f.Project.ReattachNotice.PaneID+" is uncertain")
	}
	return blockers
}

// ForgetFingerprint returns the lowercase SHA-256 of the canonical relevant
// facts. Record lists are sets for this comparison, so input order is ignored.
func ForgetFingerprint(f ForgetFacts) (string, error) {
	type revisionFact struct {
		ID       string
		Revision int64
	}
	type noteFact struct {
		ID            string
		DeliveryState string
	}
	type canonicalFacts struct {
		Project        Project
		Tasks          []revisionFact
		Notes          []noteFact
		Tickets        []revisionFact
		ClaimedTaskIDs []string
		BindIntentOpen bool
	}

	tasks := make([]revisionFact, 0, len(f.Tasks))
	for _, task := range f.Tasks {
		tasks = append(tasks, revisionFact{ID: task.ID, Revision: task.Revision})
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].ID != tasks[j].ID {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].Revision < tasks[j].Revision
	})

	notes := make([]noteFact, 0, len(f.Notes))
	for _, note := range f.Notes {
		notes = append(notes, noteFact{ID: note.ID, DeliveryState: note.State()})
	}
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].ID != notes[j].ID {
			return notes[i].ID < notes[j].ID
		}
		return notes[i].DeliveryState < notes[j].DeliveryState
	})

	tickets := make([]revisionFact, 0, len(f.Tickets))
	for _, ticket := range f.Tickets {
		tickets = append(tickets, revisionFact{ID: ticket.ID, Revision: ticket.Revision})
	}
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].ID != tickets[j].ID {
			return tickets[i].ID < tickets[j].ID
		}
		return tickets[i].Revision < tickets[j].Revision
	})

	claimed := append([]string{}, f.ClaimedTaskIDs...)
	sort.Strings(claimed)
	encoded, err := json.Marshal(canonicalFacts{
		Project: f.Project, Tasks: tasks, Notes: notes, Tickets: tickets,
		ClaimedTaskIDs: claimed, BindIntentOpen: f.BindIntentOpen,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
