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

func TestForgetBlockersReasonsHaveStableOrder(t *testing.T) {
	facts := ForgetFacts{
		Project: Project{Key: "project-root"},
		Tasks:   []Task{{ID: "task-z"}, {ID: "task-a"}},
		Notes: []Inbox{
			{ID: "note-uncertain", DeliveryState: DeliveryUncertain},
			{ID: "note-sending", DeliveryState: DeliverySending},
		},
	}
	want := []string{
		"task task-a has no outcome: record one first, such as abandoned",
		"task task-z has no outcome: record one first, such as abandoned",
		"note note-sending is sending",
		"note note-uncertain is uncertain",
	}
	if got := ForgetBlockers(facts); !equalStrings(got, want) {
		t.Fatalf("ForgetBlockers() = %v, want %v", got, want)
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
		Project:           Project{Key: "project-root"},
		Tasks:             []Task{{ID: "task-a", Revision: 1}, {ID: "task-b", Revision: 1}},
		Notes:             []Inbox{{ID: "note-a", DeliveryState: DeliveryPending}, {ID: "note-b", DeliveryState: DeliveryDelivered}},
		Tickets:           []Ticket{{ID: "ticket-a", Revision: 1}, {ID: "ticket-b", Revision: 1}},
		ClaimedTaskIDs:    []string{"task-a", "task-b"},
		ReadySessionPanes: []string{"pane-project", "pane-task"},
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
		{"ready session panes", func(f *ForgetFacts) { f.ReadySessionPanes = append(f.ReadySessionPanes, "pane-new") }},
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

	assertFingerprintInvariant(t, base, baseFingerprint, "Tasks", func(f *ForgetFacts) {
		f.Tasks[0], f.Tasks[1] = f.Tasks[1], f.Tasks[0]
	})
	assertFingerprintInvariant(t, base, baseFingerprint, "Notes", func(f *ForgetFacts) {
		f.Notes[0], f.Notes[1] = f.Notes[1], f.Notes[0]
	})
	assertFingerprintInvariant(t, base, baseFingerprint, "Tickets", func(f *ForgetFacts) {
		f.Tickets[0], f.Tickets[1] = f.Tickets[1], f.Tickets[0]
	})
	assertFingerprintInvariant(t, base, baseFingerprint, "ClaimedTaskIDs", func(f *ForgetFacts) {
		f.ClaimedTaskIDs[0], f.ClaimedTaskIDs[1] = f.ClaimedTaskIDs[1], f.ClaimedTaskIDs[0]
	})
	assertFingerprintInvariant(t, base, baseFingerprint, "ReadySessionPanes", func(f *ForgetFacts) {
		f.ReadySessionPanes[0], f.ReadySessionPanes[1] = f.ReadySessionPanes[1], f.ReadySessionPanes[0]
	})
}

func TestForgetFingerprintIgnoresEmptyAndDuplicateReadySessionPanes(t *testing.T) {
	tests := []struct {
		name  string
		left  []string
		right []string
	}{
		{name: "all empty equals nil", left: nil, right: []string{"", ""}},
		{name: "duplicates equal one pane", left: []string{"p1", "p1"}, right: []string{"p1"}},
		{name: "empty plus pane equals pane", left: []string{"", "p1"}, right: []string{"p1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := forgetFingerprint(t, ForgetFacts{ReadySessionPanes: tt.left})
			right := forgetFingerprint(t, ForgetFacts{ReadySessionPanes: tt.right})
			if left != right {
				t.Fatalf("fingerprints differ: left %s, right %s", left, right)
			}
		})
	}
}

func assertFingerprintInvariant(t *testing.T, base ForgetFacts, want, sliceName string, reorder func(*ForgetFacts)) {
	t.Helper()
	facts := cloneForgetFacts(base)
	reorder(&facts)
	if got := forgetFingerprint(t, facts); got != want {
		t.Fatalf("reversing %s changed fingerprint: got %s, want %s", sliceName, got, want)
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
	f.ReadySessionPanes = append([]string(nil), f.ReadySessionPanes...)
	return f
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsString(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
