package coord

// Bind verifies Herdr's OMP integration and the plugin's extension before it
// records or starts anything (the founding spec's "Launch and dispatch" step 1).
// These tests wire the real read-only setup check as the coordinator's
// preflight and drive it against the stand-in Herdr CLI, so "nothing happened"
// is asserted on the stand-in's own call log and on the store, not on a mock.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/setup"
	"herdr-omp-orchestrator/internal/state"
)

const (
	statusInstalled    = "omp: current (v10) (C:\\Users\\fixture\\.omp\\agent\\extensions\\herdr-omp-agent-state.ts)"
	statusNotInstalled = "omp: not installed (C:\\Users\\fixture\\.omp\\agent\\extensions\\herdr-omp-agent-state.ts)"
)

// ompProfile is an isolated OMP profile directory the preflight resolves from
// its environment, never from the developer's own ~/.omp.
type ompProfile struct {
	env        map[string]string
	extensions string
}

func newOMPProfile(t *testing.T) ompProfile {
	t.Helper()
	home := t.TempDir()
	extensions := filepath.Join(home, ".omp", "agent", "extensions")
	if err := os.MkdirAll(extensions, 0o755); err != nil {
		t.Fatalf("create the extensions directory: %v", err)
	}
	return ompProfile{env: map[string]string{"HOME": home}, extensions: extensions}
}

func (p ompProfile) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.extensions, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// installIntegration stands in for `herdr integration install omp`.
func (p ompProfile) installIntegration(t *testing.T) {
	t.Helper()
	p.write(t, setup.HerdrIntegrationFile, "// Herdr's own integration.\nexport default {};\n")
}

// installExtension stands in for the setup action: a file that carries the
// managed marker, which is all the read-only check requires of it.
func (p ompProfile) installExtension(t *testing.T) {
	t.Helper()
	p.write(t, setup.ExtensionName, setup.ManagedMarker+"\nexport default {};\n")
}

// satisfiedPreflight stands in for a passing setup check in the tests whose
// subject is something other than the gate. It is an explicit choice made in the
// fixture: a coordinator built without any preflight refuses to bind.
func satisfiedPreflight(context.Context) error { return nil }

// gate replaces the fixture's coordinator with one that runs the real setup
// check, through the stand-in Herdr CLI, before Bind does anything.
func (f *coordFixture) gate(t *testing.T, profile ompProfile) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	coordinator, err := New(f.store, f.client, Options{
		Concurrency:      3,
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
		Preflight: func(ctx context.Context) error {
			return setup.Verify(ctx, profile.env, exe)
		},
	})
	if err != nil {
		t.Fatalf("coord.New: %v", err)
	}
	f.coord = coordinator
}

// assertOnlyTheStatusCheckRan fails unless the stand-in saw `herdr integration
// status` and nothing else: no pane lookup, no workspace, no agent.
func (f *coordFixture) assertOnlyTheStatusCheckRan(t *testing.T) {
	t.Helper()
	var got [][]string
	for _, call := range f.calls() {
		got = append(got, call.Argv)
	}
	want := [][]string{{"integration", "status"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Herdr calls = %v, want only %v", got, want)
	}
}

func (f *coordFixture) assertNoProject(t *testing.T) {
	t.Helper()
	if _, err := f.store.Project(context.Background(), f.root); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a refused bind left a project row: err = %v", err)
	}
}

func TestBindRefusesBeforeAnyEffectWhenHerdrsIntegrationIsMissing(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{IntegrationStatus: statusNotInstalled})
	profile := newOMPProfile(t)
	profile.installIntegration(t) // the file is there; Herdr's own report says otherwise
	profile.installExtension(t)
	f.gate(t, profile)

	_, err := f.bind("", "")
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, setup.ErrIntegrationMissing) {
		t.Fatalf("Bind = %v, want ErrNotReady wrapping setup.ErrIntegrationMissing", err)
	}
	if !strings.Contains(err.Error(), "herdr integration install omp") {
		t.Errorf("error %q does not name the remedy", err)
	}
	f.assertOnlyTheStatusCheckRan(t)
	f.assertNoProject(t)
}

func TestBindRefusesBeforeAnyEffectWhenTheIntegrationFileIsMissing(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	profile := newOMPProfile(t) // Herdr reports installed, but not for this profile's directory
	profile.installExtension(t)
	f.gate(t, profile)

	if _, err := f.bind("", ""); !errors.Is(err, ErrNotReady) || !errors.Is(err, setup.ErrIntegrationMissing) {
		t.Fatalf("Bind = %v, want ErrNotReady wrapping setup.ErrIntegrationMissing", err)
	}
	f.assertOnlyTheStatusCheckRan(t)
	f.assertNoProject(t)
}

func TestBindRefusesBeforeAnyEffectWhenTheExtensionIsMissing(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	profile := newOMPProfile(t)
	profile.installIntegration(t)
	f.gate(t, profile)

	_, err := f.bind("", "")
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, setup.ErrExtensionMissing) {
		t.Fatalf("Bind = %v, want ErrNotReady wrapping setup.ErrExtensionMissing", err)
	}
	if !strings.Contains(err.Error(), "setup action") {
		t.Errorf("error %q does not name the remedy", err)
	}
	f.assertOnlyTheStatusCheckRan(t)
	f.assertNoProject(t)
	if _, statErr := os.Stat(filepath.Join(profile.extensions, setup.ExtensionName)); !os.IsNotExist(statErr) {
		t.Errorf("the check installed the extension: stat err = %v", statErr)
	}
}

// TestBindRetriesCleanlyOnceThePrerequisiteIsInstalled pins that a refusal
// costs nothing: after the remedy the same bind creates exactly one workspace
// and one agent, and the integration was verified before either.
func TestBindRetriesCleanlyOnceThePrerequisiteIsInstalled(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	profile := newOMPProfile(t)
	profile.installIntegration(t)
	f.gate(t, profile)

	if _, err := f.bind("", ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Bind without the extension = %v, want ErrNotReady", err)
	}
	profile.installExtension(t)

	project, err := f.bind("", "")
	if err != nil {
		t.Fatalf("Bind after the extension was installed: %v", err)
	}
	if project.OMPSessionID == "" {
		t.Errorf("the bind recorded no session: %+v", project)
	}
	calls := f.calls()
	if len(calls) < 3 || strings.Join(calls[0].Argv, " ") != "integration status" || strings.Join(calls[1].Argv, " ") != "integration status" {
		t.Fatalf("calls = %v, want the refused and the retried bind to each begin with `integration status`", calls)
	}
	for _, command := range []string{"workspace create", "agent start"} {
		if got := f.countCalls(command); got != 1 {
			t.Errorf("%s ran %d times, want exactly once", command, got)
		}
	}
	if got := f.countCalls("integration status"); got != 2 {
		t.Errorf("integration status ran %d times, want once per bind", got)
	}
}

// TestBindOfARecordedProjectIsGatedToo pins that the check also runs before the
// reopen path, which can start the recorded primary's agent: a refusal changes
// no row and touches no pane.
func TestBindOfARecordedProjectIsGatedToo(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.mustBind() // bound without a gate, as it was before the prerequisite vanished
	bound, err := f.store.Project(context.Background(), f.root)
	if err != nil {
		t.Fatalf("read the bound project: %v", err)
	}
	if err := os.Remove(filepath.Join(f.dir, fixtureCallsName)); err != nil {
		t.Fatalf("reset the call log: %v", err)
	}
	profile := newOMPProfile(t)
	profile.installIntegration(t)
	f.gate(t, profile)

	if _, err := f.bind("", ""); !errors.Is(err, ErrNotReady) || !errors.Is(err, setup.ErrExtensionMissing) {
		t.Fatalf("Bind = %v, want ErrNotReady wrapping setup.ErrExtensionMissing", err)
	}
	f.assertOnlyTheStatusCheckRan(t)
	after, err := f.store.Project(context.Background(), f.root)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	if !reflect.DeepEqual(after, bound) {
		t.Errorf("a refused re-bind changed the project:\nbefore: %+v\nafter:  %+v", bound, after)
	}
}

// TestBindWrapsAnyPreflightFailureAndRunsItOnce pins the seam itself: whatever
// the proof returns is the cause of ErrNotReady, it runs once per bind, and it
// runs only after the repository is known to be a repository.
func TestBindWrapsAnyPreflightFailureAndRunsItOnce(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	cause := errors.New("the prerequisite is gone")
	runs := 0
	coordinator, err := New(f.store, f.client, Options{
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
		Preflight: func(context.Context) error {
			runs++
			return cause
		},
	})
	if err != nil {
		t.Fatalf("coord.New: %v", err)
	}
	f.coord = coordinator

	if _, err := f.coord.Bind(context.Background(), t.TempDir(), "", ""); !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("Bind outside a repository = %v, want ErrNotGitRepository", err)
	}
	if runs != 0 {
		t.Errorf("the preflight ran %d times for a path that is not a repository", runs)
	}

	_, err = f.bind("", "")
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, cause) {
		t.Fatalf("Bind = %v, want ErrNotReady wrapping the preflight's error", err)
	}
	if runs != 1 {
		t.Errorf("the preflight ran %d times, want once", runs)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("a refused bind called Herdr: %v", calls)
	}
	f.assertNoProject(t)
}

// TestBindRefusesWithoutAConfiguredPreflight pins that the gate cannot be
// bypassed by omission: a coordinator built without a preflight refuses to bind
// before it touches Herdr or the store, instead of launching an unverified primary.
func TestBindRefusesWithoutAConfiguredPreflight(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	coordinator, err := New(f.store, f.client, Options{
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coord.New: %v", err)
	}
	f.coord = coordinator

	_, err = f.bind("", "")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Bind without a preflight = %v, want ErrNotReady", err)
	}
	if !strings.Contains(err.Error(), "no readiness check is configured") {
		t.Errorf("error %q does not say why the bind was refused", err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("a refused bind called Herdr: %v", calls)
	}
	f.assertNoProject(t)
}
