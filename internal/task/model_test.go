package task

import (
	"strings"
	"testing"
)

func TestStageSeparatesActivityFromCompletion(t *testing.T) {
	if StageOf(Task{Phase: Intent}) != Queued {
		t.Fatal("unprovisioned intent is queued, not running")
	}
	x := Task{Phase: PromptSubmitted, Activity: "done"}
	if StageOf(x) != Running {
		t.Fatalf("Herdr done is not task done: %s", StageOf(x))
	}
	x.Blocker = "need an answer"
	if StageOf(x) != NeedsInput {
		t.Fatal("blocker must be visible")
	}
	x.Blocker = ""
	x.Report = &Report{Kind: "partial", Summary: "more work remains"}
	if StageOf(x) != Running {
		t.Fatal("partial report is not final")
	}
	x.Report = &Report{Kind: "result", Summary: "finished"}
	if StageOf(x) != ReadyForReview {
		t.Fatal("report needs review")
	}
	x.Disposition = "user chose to keep branch"
	if StageOf(x) != Done {
		t.Fatal("recorded disposition closes task")
	}
}

func TestStagePriorityOrder(t *testing.T) {
	cases := []struct {
		name string
		job  Task
		want Stage
	}{
		{"empty task is queued", Task{}, Queued},
		{"worktree created is running", Task{Phase: WorktreeCreated}, Running},
		{"agent started is running", Task{Phase: AgentStarted}, Running},
		{"attention needs input", Task{Phase: PromptSubmitted, Attention: "dialog open"}, NeedsInput},
		{"blocker outranks final report", Task{Phase: PromptSubmitted, Report: &Report{Kind: "result"}, Blocker: "stuck"}, NeedsInput},
		{"disposition outranks final report", Task{Phase: PromptSubmitted, Report: &Report{Kind: "result"}, Disposition: "kept branch"}, Done},
		{"unknown report kind is not final", Task{Phase: PromptSubmitted, Report: &Report{Kind: "note"}}, Running},
		{"activity alone does not advance a task", Task{Phase: Intent, Activity: "done"}, Queued},
	}
	for _, tc := range cases {
		if got := StageOf(tc.job); got != tc.want {
			t.Errorf("%s: StageOf = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestAnnouncementProvesTheRecordedSessionInEitherForm is the shared contract for
// every caller that holds a session value recorded at dispatch time and a session
// reference Herdr reports now.
//
// OMP exposes two native identities for one session, and which one a record keeps
// depends on what Herdr reported when the handshake was confirmed: a task's
// OMPSessionID is the announced id on one build and the session file on another.
// The announcement is the only thing that knows both, so it — not a string
// comparison of two arbitrary values — has to prove that the recorded value and
// the reported reference describe the same running session. Nothing here parses an
// id out of a file name, and nothing accepts a partial value.
func TestAnnouncementProvesTheRecordedSessionInEitherForm(t *testing.T) {
	const (
		id    = "01a0d886-c907-7000-80ce-cdc6564a519d"
		other = "ffffffff-ffff-7000-80ce-ffffffffffff"
		file  = `C:\work\omp\agent\sessions\-work-orch\2026-09-25T11-59-03-837Z_01a0d886-c907-7000-80ce-cdc6564a519d.jsonl`
	)
	otherFile := `C:\work\omp\agent\sessions\-work-orch\2026-09-25T12-00-00-000Z_ffffffff-ffff-7000-80ce-ffffffffffff.jsonl`

	announced := Announcement{ID: id, File: file}

	cases := []struct {
		name      string
		announced Announcement
		recorded  string
		ref       SessionRef
		want      bool
	}{
		// The same session, in the two forms the runtime and Herdr each use.
		{"recorded id, Herdr reports the path", announced, id, SessionRef{Kind: SessionKindPath, Value: file}, true},
		{"recorded path, Herdr reports the id", announced, file, SessionRef{Kind: SessionKindID, Value: id}, true},
		{"recorded id, Herdr reports the id", announced, id, SessionRef{Kind: SessionKindID, Value: id}, true},
		{"recorded path, Herdr reports the path", announced, file, SessionRef{Kind: SessionKindPath, Value: file}, true},
		{"the recorded path spelled with other separators", announced, file,
			SessionRef{Kind: SessionKindPath, Value: strings.ReplaceAll(file, `\`, "/")}, true},

		// A different session is a different worker, whichever form it arrives in.
		{"another session as an id", announced, other, SessionRef{Kind: SessionKindID, Value: other}, false},
		{"another session reported as a path", announced, id, SessionRef{Kind: SessionKindPath, Value: otherFile}, false},
		{"the recorded path but another reported id", announced, file, SessionRef{Kind: SessionKindID, Value: other}, false},

		// The identity is the whole value, never a substring: an id that merely
		// appears inside a session file's name is not that session file.
		{"a path containing the recorded id is not the id", announced, id,
			SessionRef{Kind: SessionKindPath, Value: `C:\sessions\` + id + `.jsonl`}, false},
		{"a truncated path is not the path", announced, file,
			SessionRef{Kind: SessionKindPath, Value: strings.TrimSuffix(file, ".jsonl")}, false},

		// An announcement only carries the forms the runtime announced, so a form
		// it does not carry cannot be proven.
		{"only an id announced, Herdr reports a path", Announcement{ID: id}, id, SessionRef{Kind: SessionKindPath, Value: file}, false},
		{"only a path announced, Herdr reports an id", Announcement{File: file}, file, SessionRef{Kind: SessionKindID, Value: id}, false},
		{"no announcement at all", Announcement{}, id, SessionRef{Kind: SessionKindID, Value: id}, false},

		// Fail closed on anything that is not a usable identity.
		{"empty recorded value", announced, "", SessionRef{Kind: SessionKindID, Value: id}, false},
		{"blank recorded value", announced, "   ", SessionRef{Kind: SessionKindID, Value: id}, false},
		{"unsupported reported kind", announced, id, SessionRef{Kind: "pid", Value: "1234"}, false},
		{"empty reported value", announced, id, SessionRef{Kind: SessionKindID, Value: ""}, false},
		{"uppercase reported kind still has to match", announced, id, SessionRef{Kind: "ID", Value: id}, true},
	}
	for _, tc := range cases {
		if got := tc.announced.Proves(tc.recorded, tc.ref); got != tc.want {
			t.Errorf("%s: Proves(%q, %+v) = %v, want %v", tc.name, tc.recorded, tc.ref, got, tc.want)
		}
	}
}
