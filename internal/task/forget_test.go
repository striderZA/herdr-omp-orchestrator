package task

import "testing"

func TestForgetBlockersRefusesEachInFlightKind(t *testing.T) {
	base := ForgetFacts{Project: Project{Key: "project-root"}}
	tests := []struct {
		name  string
		facts ForgetFacts
		id    string
	}{
		{
			name: "undecided task",
			facts: func() ForgetFacts {
				f := base
				f.Tasks = []Task{{ID: "task-undecided"}}
				return f
			}(),
			id: "task-undecided",
		},
		{
			name: "claimed task",
			facts: func() ForgetFacts {
				f := base
				f.Tasks = []Task{{ID: "task-claimed", Disposition: "accepted"}}
				f.ClaimedTaskIDs = []string{"task-claimed"}
				return f
			}(),
			id: "task-claimed",
		},
		{
			name: "sending note",
			facts: func() ForgetFacts {
				f := base
				f.Notes = []Inbox{{ID: "note-sending", DeliveryState: DeliverySending}}
				return f
			}(),
			id: "note-sending",
		},
		{
			name: "uncertain note",
			facts: func() ForgetFacts {
				f := base
				f.Notes = []Inbox{{ID: "note-uncertain", DeliveryState: DeliveryUncertain}}
				return f
			}(),
			id: "note-uncertain",
		},
		{
			name: "open bind intent",
			facts: func() ForgetFacts {
				f := base
				f.BindIntentOpen = true
				return f
			}(),
			id: "project-root",
		},
		{
			name: "uncertain reattach notice",
			facts: func() ForgetFacts {
				f := base
				f.Project.ReattachNotice = NoticeAttempt{PaneID: "pane-notice", State: NoticeUncertain}
				return f
			}(),
			id: "pane-notice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blockers := ForgetBlockers(tt.facts)
			if len(blockers) == 0 {
				t.Fatal("ForgetBlockers returned no refusal reason")
			}
			if !containsString(blockers[0], tt.id) {
				t.Fatalf("first refusal %q does not name %q", blockers[0], tt.id)
			}
		})
	}
}

func TestForgetBlockersAllowsDecidedHistoryAndPendingNotes(t *testing.T) {
	facts := ForgetFacts{
		Project: Project{Key: "project-root"},
		Tasks:   []Task{{ID: "task-done", Disposition: "accepted", Outcome: OutcomeIntegrated}},
		Notes: []Inbox{
			{ID: "note-pending", DeliveryState: DeliveryPending},
			{ID: "note-delivered", DeliveryState: DeliveryDelivered},
		},
		Tickets: []Ticket{{ID: "ticket-open"}},
	}
	if blockers := ForgetBlockers(facts); blockers != nil {
		t.Fatalf("ForgetBlockers = %v, want nil", blockers)
	}
}

func TestForgetFingerprintChangesWithAnyRecordedFact(t *testing.T) {
	base := ForgetFacts{
		Project: Project{Key: "project-root"},
		Tasks:   []Task{{ID: "task-a", Revision: 1}, {ID: "task-b", Revision: 1}},
		Notes:   []Inbox{{ID: "note-a", DeliveryState: DeliveryPending}},
		Tickets: []Ticket{{ID: "ticket-a", Revision: 1}},
	}
	baseFingerprint := forgetFingerprint(t, base)

	changes := []struct {
		name   string
		mutate func(*ForgetFacts)
	}{
		{"task revision", func(f *ForgetFacts) { f.Tasks[0].Revision++ }},
		{"note state", func(f *ForgetFacts) { f.Notes[0].DeliveryState = DeliverySending }},
		{"ticket revision", func(f *ForgetFacts) { f.Tickets[0].Revision++ }},
		{"claimed set", func(f *ForgetFacts) { f.ClaimedTaskIDs = []string{"task-a"} }},
		{"bind intent", func(f *ForgetFacts) { f.BindIntentOpen = true }},
		{"primary attention", func(f *ForgetFacts) { f.Project.PrimaryAttention = "primary unavailable" }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			facts := cloneForgetFacts(base)
			change.mutate(&facts)
			if got := forgetFingerprint(t, facts); got == baseFingerprint {
				t.Fatalf("fingerprint unchanged after %s", change.name)
			}
		})
	}

	reversed := cloneForgetFacts(base)
	reversed.Tasks[0], reversed.Tasks[1] = reversed.Tasks[1], reversed.Tasks[0]
	if got := forgetFingerprint(t, reversed); got != baseFingerprint {
		t.Fatalf("reversing Tasks changed fingerprint: got %s, want %s", got, baseFingerprint)
	}
}

func forgetFingerprint(t *testing.T, facts ForgetFacts) string {
	t.Helper()
	fingerprint, err := ForgetFingerprint(facts)
	if err != nil {
		t.Fatalf("ForgetFingerprint() error = %v", err)
	}
	return fingerprint
}

func cloneForgetFacts(f ForgetFacts) ForgetFacts {
	f.Tasks = append([]Task(nil), f.Tasks...)
	f.ClaimedTaskIDs = append([]string(nil), f.ClaimedTaskIDs...)
	f.Notes = append([]Inbox(nil), f.Notes...)
	f.Tickets = append([]Ticket(nil), f.Tickets...)
	return f
}

func containsString(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
