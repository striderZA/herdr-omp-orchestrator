package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// Multiple startup/event processes may open one older plugin database at once.
// Every opener must see either the old shape and migrate it or the finished shape.
func TestConcurrentOpenersMigrateLegacyReadyTable(t *testing.T) {
	for round := range 3 {
		dsn := seedLegacyReadyTable(t, t.TempDir())

		const openers = 12
		start := make(chan struct{})
		failures := make(chan error, openers)
		var wg sync.WaitGroup
		for range openers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				db, err := sql.Open("sqlite", dsn)
				if err != nil {
					failures <- err
					return
				}
				defer db.Close()
				<-start
				if err := migrate(context.Background(), db); err != nil {
					failures <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Errorf("round %d: a concurrent opener failed: %v", round, err)
		}
	}
}

// Open also runs CREATE IF NOT EXISTS and the dependent index after migration.
// Concurrent hooks must all complete that full path against a legacy database.
func TestConcurrentOpenMigratesLegacyReadyTable(t *testing.T) {
	dir := t.TempDir()
	seedLegacyReadyTable(t, dir)
	const openers = 12
	start := make(chan struct{})
	failures := make(chan error, openers)
	var wg sync.WaitGroup
	for range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := Open(dir)
			if err != nil {
				failures <- err
				return
			}
			if err := store.Close(); err != nil {
				failures <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("concurrent Open failed: %v", err)
	}
}

func seedLegacyReadyTable(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, dbFile)
	dsn := "file:" + filepath.ToSlash(path) +
		"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	seed, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`DROP TABLE ready_sessions;
		CREATE TABLE ready_sessions (
			launch_pane_id TEXT PRIMARY KEY,
			omp_session_id TEXT NOT NULL,
			protocol INTEGER NOT NULL,
			verified INTEGER NOT NULL DEFAULT 0,
			seen_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	return dsn
}
