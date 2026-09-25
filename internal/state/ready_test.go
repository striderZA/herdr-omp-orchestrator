package state_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// This file covers the readiness handshake's identity model: Herdr reports a
// session as a typed reference (kind "id" or kind "path"), a session announces
// both identities plus whether it is materialized on disk, and a confirmation
// records which form was actually verified.

const sessionFile = `C:\work\omp\sessions\-work-orch\2026-09-25T11-59-03-837Z_01a0d86e.jsonl`

func TestStoreReadyComparesTheConfirmedReferenceByKind(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionOne, File: sessionFile, OnDisk: true}, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	// Nothing is ready before a confirmation, in either kind.
	for _, announced := range []task.Announcement{{ID: sessionOne}, {File: sessionFile}} {
		if ready, err := s.Ready(ctx, launchPane, announced); err != nil || ready {
			t.Errorf("Ready(%+v) before confirmation = (%v, %v), want (false, nil)", announced, ready, err)
		}
	}

	// Confirming the id makes the id the form that must match.
	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindID, Value: sessionOne}); err != nil {
		t.Fatalf("ConfirmReady(id): %v", err)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || !ready {
		t.Errorf("Ready(confirmed id) = (%v, %v), want (true, nil)", ready, err)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionTwo}); err != nil || ready {
		t.Errorf("Ready(other id) = (%v, %v), want (false, nil)", ready, err)
	}
	// The annotated build reports the path form, so this is the case that matters
	// in practice: a confirmed path is compared as a path.
	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindPath, Value: sessionFile}); err != nil {
		t.Fatalf("ConfirmReady(path): %v", err)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{File: sessionFile}); err != nil || !ready {
		t.Errorf("Ready(confirmed path) = (%v, %v), want (true, nil)", ready, err)
	}
	// A path is compared with the platform's path rules, and the id form is a
	// different comparison that the confirmed path does not satisfy.
	caseVariant := `c:/WORK/omp/sessions/-work-orch/2026-09-25T11-59-03-837Z_01a0d86e.jsonl`
	ready, err := s.Ready(ctx, launchPane, task.Announcement{File: caseVariant})
	switch {
	case runtime.GOOS == "windows" && (err != nil || !ready):
		t.Errorf("Ready(path case variant) = (%v, %v), want (true, nil) on Windows", ready, err)
	case runtime.GOOS != "windows" && (err != nil || ready):
		t.Errorf("Ready(path case variant) = (%v, %v), want (false, nil) off Windows", ready, err)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || ready {
		t.Errorf("Ready(id against a confirmed path) = (%v, %v), want (false, nil)", ready, err)
	}
}

func TestStoreReadyRefusesMismatchesAndUnknownForms(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionOne, File: sessionFile}, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	for _, ref := range []task.SessionRef{
		{Kind: task.SessionKindID, Value: sessionTwo},
		{Kind: task.SessionKindPath, Value: `C:\work\somewhere-else.jsonl`},
	} {
		if err := s.ConfirmReady(ctx, launchPane, ref); !errors.Is(err, state.ErrMismatch) {
			t.Errorf("ConfirmReady(%+v) err = %v, want ErrMismatch", ref, err)
		}
	}
	// An unreadable reference never confirms, and never verifies anything.
	for _, ref := range []task.SessionRef{
		{Kind: "pid", Value: "4242"},
		{Kind: "", Value: sessionOne},
		{Kind: task.SessionKindID, Value: "  "},
	} {
		if err := s.ConfirmReady(ctx, launchPane, ref); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("ConfirmReady(%+v) err = %v, want ErrInvalid", ref, err)
		}
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || ready {
		t.Errorf("a refused confirmation verified the session: (%v, %v)", ready, err)
	}
	if err := s.ConfirmReady(ctx, "w9:p9", task.SessionRef{Kind: task.SessionKindID, Value: sessionOne}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ConfirmReady(unknown pane) err = %v, want ErrNotFound", err)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{}); err != nil || ready {
		t.Errorf("Ready(empty announcement) = (%v, %v), want (false, nil)", ready, err)
	}
	if err := s.RecordReady(ctx, launchPane, task.Announcement{}, 1); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("RecordReady(empty) err = %v, want ErrInvalid", err)
	}
	if err := s.RecordReady(ctx, "", task.Announcement{ID: sessionOne}, 1); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("RecordReady(no pane) err = %v, want ErrInvalid", err)
	}
}

func TestStoreReadyKeepsAnUnmaterializedSessionAnnouncement(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	// OMP allocates the session file lazily: the id exists, the file may be empty,
	// and the session is not resumable yet. The handshake still works.
	announced, ok := task.NewAnnouncement(sessionOne, "", false)
	if !ok {
		t.Fatal("NewAnnouncement rejected an id-only announcement")
	}
	if err := s.RecordReady(ctx, launchPane, announced, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindID, Value: sessionOne}); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || !ready {
		t.Fatalf("Ready = (%v, %v), want (true, nil) for an unmaterialized session", ready, err)
	}
	stored, found, err := s.AnnouncedSession(ctx, launchPane)
	if err != nil || !found {
		t.Fatalf("AnnouncedSession = (_, %v, %v), want found", found, err)
	}
	if stored.OnDisk || stored.File != "" || stored.ID != sessionOne {
		t.Errorf("stored announcement = %+v, want the id, no file and not materialized", stored)
	}

	// The runtime materializes the session later: the announcement is updated and
	// the identity that was confirmed is unchanged.
	materialized, ok := task.NewAnnouncement(sessionOne, sessionFile, true)
	if !ok {
		t.Fatal("NewAnnouncement rejected the materialized pair")
	}
	if err := s.RecordReady(ctx, launchPane, materialized, 1); err != nil {
		t.Fatalf("RecordReady(materialized): %v", err)
	}
	stored, _, err = s.AnnouncedSession(ctx, launchPane)
	if err != nil {
		t.Fatalf("AnnouncedSession: %v", err)
	}
	if stored.File != sessionFile || !stored.OnDisk {
		t.Errorf("stored announcement = %+v, want the file and the materialized state", stored)
	}
	if ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || !ready {
		t.Errorf("materializing the session lost the confirmation: (%v, %v)", ready, err)
	}
}

func TestStoreByOMPSessionMatchesEitherAnnouncedIdentity(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	// A worker whose recorded identity is the session file Herdr reported.
	worker := seedTask("t1")
	worker.OMPSessionID = sessionFile
	if err := s.CreateTask(ctx, worker); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// The extension announces both identities: either one finds the record.
	for _, announced := range []task.Announcement{
		{ID: sessionOne, File: sessionFile},
		{ID: "", File: sessionFile},
	} {
		binding, err := s.ByOMPSession(ctx, announced)
		if err != nil {
			t.Fatalf("ByOMPSession(%+v) = %v", announced, err)
		}
		if binding.Role != state.RoleWorker || binding.Task.ID != "t1" {
			t.Errorf("ByOMPSession(%+v) = %+v, want the worker", announced, binding)
		}
	}
	// An id-only announcement cannot be matched against a file-recorded session:
	// the two identities are different values, and guessing between them is what
	// this lookup must not do.
	if binding, err := s.ByOMPSession(ctx, task.Announcement{ID: sessionOne}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ByOMPSession(id only) matched %+v (%v), want a refusal", binding, err)
	}
	// The path form is compared with the platform's path rules on Windows.
	caseVariant := task.Announcement{File: `c:/WORK/OMP/sessions/-work-orch/2026-09-25T11-59-03-837Z_01a0d86e.jsonl`}
	if _, err := s.ByOMPSession(ctx, caseVariant); runtime.GOOS == "windows" && err != nil {
		t.Errorf("ByOMPSession(path case variant) = %v, want the worker on Windows", err)
	}
	if _, err := s.ByOMPSession(ctx, task.Announcement{ID: "someone-else", File: `C:\work\other.jsonl`}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ByOMPSession(unknown) err = %v, want ErrNotFound", err)
	}
	if _, err := s.ByOMPSession(ctx, task.Announcement{}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("ByOMPSession(empty) err = %v, want ErrInvalid", err)
	}
	// A record with no session identity is never matched.
	queued := seedTask("t2")
	queued.OMPSessionID = ""
	queued.LaunchPaneID = ""
	queued.PublicPaneID = ""
	if err := s.CreateTask(ctx, queued); err != nil {
		t.Fatalf("CreateTask(t2): %v", err)
	}
	if binding, err := s.ByOMPSession(ctx, task.Announcement{ID: "orphan"}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ByOMPSession matched a session-less record: %+v (%v)", binding, err)
	}
}

func TestStoreMigratesAnOlderReadyTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A state database written by the first release: only the columns that existed
	// then, with a verified handshake recorded the old way.
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE ready_sessions (
		launch_pane_id TEXT PRIMARY KEY,
		omp_session_id TEXT NOT NULL,
		protocol INTEGER NOT NULL,
		verified INTEGER NOT NULL DEFAULT 0,
		seen_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO ready_sessions (launch_pane_id, omp_session_id, protocol, verified, seen_at)
		VALUES (?, ?, 1, 1, 0)`, launchPane, sessionOne); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy handle: %v", err)
	}

	s := open(t, dir)
	announced := task.Announcement{ID: sessionOne}
	stored, found, err := s.AnnouncedSession(ctx, launchPane)
	if err != nil || !found {
		t.Fatalf("AnnouncedSession = (_, %v, %v), want the migrated row", found, err)
	}
	if stored.ID != sessionOne || stored.File != "" || stored.OnDisk {
		t.Errorf("migrated announcement = %+v, want the legacy id and no new state", stored)
	}
	// The legacy row cannot say which form was verified, so it is not ready until
	// it is confirmed again in the new shape.
	if ready, err := s.Ready(ctx, launchPane, announced); err != nil || ready {
		t.Errorf("migrated row ready = (%v, %v), want (false, nil)", ready, err)
	}
	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindID, Value: sessionOne}); err != nil {
		t.Fatalf("ConfirmReady after migration: %v", err)
	}
	if ready, err := s.Ready(ctx, launchPane, announced); err != nil || !ready {
		t.Errorf("Ready after migration = (%v, %v), want (true, nil)", ready, err)
	}
}

// TestStoreVerifiedAnnouncementChecksTheRowItReturns pins the one-read handshake a
// caller may trust: the announcement it gets back is the row it read, and the
// confirmation it was judged by belongs to that same row.
//
// Reading the announcement and the confirmation separately is a race: a refresh
// that announces a new id for the same session file keeps the path confirmation,
// so the earlier snapshot still passes Ready while it no longer describes what the
// pane announces.
func TestStoreVerifiedAnnouncementChecksTheRowItReturns(t *testing.T) {
	ctx := context.Background()
	ref := func(kind, value string) task.SessionRef { return task.SessionRef{Kind: kind, Value: value} }

	t.Run("a confirmed row", func(t *testing.T) {
		s := open(t, t.TempDir())
		announced := task.Announcement{ID: sessionOne, File: sessionFile, OnDisk: true}
		if err := s.RecordReady(ctx, launchPane, announced, 1); err != nil {
			t.Fatalf("RecordReady: %v", err)
		}
		if err := s.ConfirmReady(ctx, launchPane, ref(task.SessionKindPath, sessionFile)); err != nil {
			t.Fatalf("ConfirmReady: %v", err)
		}

		got, verified, err := s.VerifiedAnnouncement(ctx, launchPane)
		if err != nil {
			t.Fatalf("VerifiedAnnouncement: %v", err)
		}
		if !verified {
			t.Fatal("a confirmed row was not verified")
		}
		if got.ID != sessionOne || got.File != sessionFile || !got.OnDisk {
			t.Errorf("announcement = %+v, want the whole announced identity surface", got)
		}
	})

	t.Run("an unverified row", func(t *testing.T) {
		s := open(t, t.TempDir())
		if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionOne, File: sessionFile}, 1); err != nil {
			t.Fatalf("RecordReady: %v", err)
		}
		got, verified, err := s.VerifiedAnnouncement(ctx, launchPane)
		if err != nil {
			t.Fatalf("VerifiedAnnouncement: %v", err)
		}
		if verified || got.ID != "" || got.File != "" {
			t.Errorf("announced but unconfirmed row = (%+v, %v), want nothing verified", got, verified)
		}
	})

	t.Run("no row at all", func(t *testing.T) {
		s := open(t, t.TempDir())
		got, verified, err := s.VerifiedAnnouncement(ctx, launchPane)
		if err != nil || verified || got.ID != "" {
			t.Errorf("unknown pane = (%+v, %v, %v), want (zero, false, nil)", got, verified, err)
		}
		if _, _, err := s.VerifiedAnnouncement(ctx, "  "); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("empty launch pane err = %v, want ErrInvalid", err)
		}
	})

	t.Run("a refreshed row returns the current id, never the stale one", func(t *testing.T) {
		s := open(t, t.TempDir())
		// Confirmed the way the installed build confirms: the session file.
		if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionOne, File: sessionFile}, 1); err != nil {
			t.Fatalf("RecordReady: %v", err)
		}
		if err := s.ConfirmReady(ctx, launchPane, ref(task.SessionKindPath, sessionFile)); err != nil {
			t.Fatalf("ConfirmReady: %v", err)
		}
		stale, found, err := s.AnnouncedSession(ctx, launchPane)
		if err != nil || !found || stale.ID != sessionOne {
			t.Fatalf("AnnouncedSession = (%+v, %v, %v), want the first announcement", stale, found, err)
		}

		// The runtime refreshes the same session file under a new id. The path
		// confirmation survives, because the announced file is unchanged.
		if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionTwo, File: sessionFile}, 1); err != nil {
			t.Fatalf("RecordReady(refresh): %v", err)
		}
		// This is the race the one-read API removes: the earlier snapshot still
		// passes the two-read check, although the pane no longer announces it.
		if ready, err := s.Ready(ctx, launchPane, stale); err != nil || !ready {
			t.Errorf("Ready(stale snapshot) = (%v, %v); the trap this API exists for should be (true, nil)", ready, err)
		}

		got, verified, err := s.VerifiedAnnouncement(ctx, launchPane)
		if err != nil {
			t.Fatalf("VerifiedAnnouncement: %v", err)
		}
		if !verified {
			t.Fatal("the refreshed row is still confirmed for the announced path")
		}
		if got.ID != sessionTwo || got.File != sessionFile {
			t.Errorf("announcement = %+v, want the current id %q with the announced file", got, sessionTwo)
		}
	})

	t.Run("a confirmation that no longer describes the row is refused", func(t *testing.T) {
		s := open(t, t.TempDir())
		if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionOne, File: sessionFile}, 1); err != nil {
			t.Fatalf("RecordReady: %v", err)
		}
		if err := s.ConfirmReady(ctx, launchPane, ref(task.SessionKindID, sessionOne)); err != nil {
			t.Fatalf("ConfirmReady: %v", err)
		}
		// A different process announces a different session: the old confirmation
		// must not outlive it.
		if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionTwo, File: sessionFile}, 1); err != nil {
			t.Fatalf("RecordReady(restart): %v", err)
		}
		got, verified, err := s.VerifiedAnnouncement(ctx, launchPane)
		if err != nil {
			t.Fatalf("VerifiedAnnouncement: %v", err)
		}
		if verified || got.ID != "" {
			t.Errorf("restarted row = (%+v, %v), want the stale id confirmation to be gone", got, verified)
		}
	})
}
