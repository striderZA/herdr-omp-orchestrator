package coord

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Issue #89 asks that a slow subtest's output attribute its time to each git
// call. Coordinator.git is where bind, dispatch and cleanup run git, so those
// calls must reach the same observer the fixture's call timing log uses.

func TestGitObserverReceivesEachCoordinatorGitCall(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}

	type observed struct {
		argv    []string
		elapsed time.Duration
		err     error
	}
	var calls []observed
	c := &Coordinator{opts: Options{
		Git:         "git",
		CallTimeout: 5 * time.Second,
		GitObserver: func(argv []string, elapsed time.Duration, err error) {
			calls = append(calls, observed{argv: argv, elapsed: elapsed, err: err})
		},
	}}

	ctx := context.Background()
	out, err := c.git(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("git rev-parse in a work tree: %v (output %q)", err, out)
	}
	want := []string{"git", "-C", root, "rev-parse", "--show-toplevel"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].argv, want) || calls[0].err != nil || calls[0].elapsed < 0 {
		t.Fatalf("observed %+v, want one successful call with argv %q", calls, want)
	}

	// A failing call is observed with the same error the caller receives.
	_, err = c.git(ctx, t.TempDir(), "rev-parse", "--show-toplevel")
	if err == nil {
		t.Fatal("git rev-parse outside a work tree succeeded, want an error")
	}
	if len(calls) != 2 || calls[1].err != err {
		t.Fatalf("observed %+v, want the failure the caller received: %v", calls, err)
	}
}

func TestFixtureTimingLogRecordsCoordinatorGitCalls(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	abs, err := filepath.Abs(f.root)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	want := []string{"git", "-C", abs, "rev-parse", "--show-toplevel"}
	ctx := context.Background()

	if _, err := f.coord.canonicalRoot(ctx, f.root); err != nil {
		t.Fatalf("canonicalRoot: %v", err)
	}
	if got := callsWithArgv(f.timings.snapshot(), want); len(got) != 1 {
		t.Fatalf("fixture log has %d records for the coordinator's git call, want 1: %+v", len(got), f.timings.snapshot())
	}

	// A sibling coordinator, as a second bridge process runs one, reports to the same log.
	if _, err := f.sibling().canonicalRoot(ctx, f.root); err != nil {
		t.Fatalf("sibling canonicalRoot: %v", err)
	}
	if got := callsWithArgv(f.timings.snapshot(), want); len(got) != 2 {
		t.Fatalf("fixture log has %d records after the sibling's git call, want 2", len(got))
	}
}

func TestReportFixtureTimingLogRecordsFailedCoordinatorGitCalls(t *testing.T) {
	f := newReportFixture(t)
	abs, err := filepath.Abs(f.root)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	want := []string{"git", "-C", abs, "rev-parse", "--show-toplevel"}

	if _, err := f.coord.canonicalRoot(context.Background(), f.root); err == nil {
		t.Fatal("canonicalRoot of a directory outside any work tree succeeded, want an error")
	}
	got := callsWithArgv(f.timings.snapshot(), want)
	if len(got) != 1 || got[0].outcome == "ok" {
		t.Fatalf("report fixture log has %+v for the failed git call, want one failed record", got)
	}
}

// callsWithArgv returns the timing records whose argv is exactly want.
func callsWithArgv(records []callTimingRecord, want []string) []callTimingRecord {
	var out []callTimingRecord
	for _, record := range records {
		if reflect.DeepEqual(record.argv, want) {
			out = append(out, record)
		}
	}
	return out
}
