package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This file pins the read-only install plan. PlanInstall must describe exactly
// what Install then does, refuse with the same stable code Install refuses with,
// and never write: not the extension, not its directory, not Herdr's file.

// plan runs the read-only plan for the fixture's environment and executable.
func (f *fixture) plan() (InstallPlan, error) { return PlanInstall(f.env, f.exe) }

// assertRefusal fails unless err carries the stable refusal code want.
func assertRefusal(t *testing.T, what string, err error, want *Error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s = %v, want %s", what, err, want.Code())
	}
}

// installEffect runs install and reports what it did to the extension at path,
// observed from the file system alone: an absent file is created, a file whose
// bytes changed is refreshed, and a file whose sentinel mtime survived is left
// alone. A rewrite of identical bytes matches none of the three actions, because
// an unchanged install is promised to write nothing.
func installEffect(t *testing.T, path string, install func() error) Action {
	t.Helper()
	sentinel := time.Unix(1_500_000_000, 0)
	before, readErr := os.ReadFile(path)
	existed := readErr == nil
	switch {
	case existed:
		if err := os.Chtimes(path, sentinel, sentinel); err != nil {
			t.Fatalf("set sentinel mtime on %s: %v", path, err)
		}
	case !os.IsNotExist(readErr):
		t.Fatalf("read %s before Install: %v", path, readErr)
	}

	if err := install(); err != nil {
		t.Fatalf("Install: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s after Install: %v", path, err)
	}
	switch {
	case !existed:
		return ActionCreate
	case string(before) != string(after):
		return ActionRefresh
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s after Install: %v", path, err)
	}
	if !info.ModTime().Equal(sentinel) {
		t.Fatalf("Install rewrote identical %s (mtime %v); an unchanged install must write nothing", path, info.ModTime())
	}
	return ActionUnchanged
}

// TestPlanInstallPredictsWhatInstallDoes runs the plan over each state the
// extension can be in. The plan must write nothing, and the action Install then
// takes on disk must be the action the plan named.
func TestPlanInstallPredictsWhatInstallDoes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, f *fixture)
		want    Action
	}{
		{
			name:    "create: no extension yet",
			prepare: func(t *testing.T, f *fixture) { f.installIntegration(t, "") },
			want:    ActionCreate,
		},
		{
			name: "refresh: the extension embeds a plugin executable that has moved",
			prepare: func(t *testing.T, f *fixture) {
				f.installed(t)
				moved := filepath.Join(t.TempDir(), "herdr-omp-orchestrator.exe")
				writeFile(t, moved, "MZ relocated plugin executable")
				f.exe = moved
			},
			want: ActionRefresh,
		},
		{
			name:    "unchanged: the extension already holds what setup would write",
			prepare: func(t *testing.T, f *fixture) { f.installed(t) },
			want:    ActionUnchanged,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.prepare(t, f)
			target := f.extensionPath("")
			before := snapshotTree(t, f.profile())

			plan, err := f.plan()
			if err != nil {
				t.Fatalf("PlanInstall: %v", err)
			}
			if plan.Action != tc.want {
				t.Fatalf("plan action = %q, want %q", plan.Action, tc.want)
			}
			if plan.Extension != target {
				t.Errorf("plan extension = %q, want %q", plan.Extension, target)
			}
			assertSameTree(t, before, snapshotTree(t, f.profile()))

			if got := installEffect(t, target, f.install); got != plan.Action {
				t.Fatalf("Install took action %q, but the plan said %q", got, plan.Action)
			}
		})
	}
}

// TestPlanInstallRefusesWithInstallsCode checks that the plan refuses for each
// cause Install refuses for, with the same stable code, and that neither of them
// writes anything when it refuses.
func TestPlanInstallRefusesWithInstallsCode(t *testing.T) {
	foreign := "// an extension this plugin did not generate\nexport default () => {};\n"
	for _, tc := range []struct {
		name    string
		want    *Error
		prepare func(t *testing.T, f *fixture)
	}{
		{
			name: "missing Herdr integration",
			want: ErrIntegrationMissing,
			prepare: func(t *testing.T, f *fixture) {
				f.setStatus(integrationReportMissing)
			},
		},
		{
			name: "unavailable plugin executable",
			want: ErrExecutableUnavailable,
			prepare: func(t *testing.T, f *fixture) {
				f.exe = filepath.Join(t.TempDir(), "gone.exe")
			},
		},
		{
			name: "missing state directory",
			want: ErrStateDirUnavailable,
			prepare: func(t *testing.T, f *fixture) {
				delete(f.env, "HERDR_PLUGIN_STATE_DIR")
			},
		},
		{
			name: "unavailable Herdr binary",
			want: ErrHerdrBinaryUnavailable,
			prepare: func(t *testing.T, f *fixture) {
				f.env["HERDR_BIN_PATH"] = filepath.Join(t.TempDir(), "no-such-herdr.exe")
			},
		},
		{
			name: "unmanaged conflicting extension",
			want: ErrExtensionConflict,
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, f.extensionPath(""), foreign)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.installIntegration(t, "")
			tc.prepare(t, f)
			before := snapshotTree(t, f.profile())

			_, planErr := f.plan()
			assertRefusal(t, "PlanInstall", planErr, tc.want)
			assertRefusal(t, "Install", f.install(), tc.want)
			assertSameTree(t, before, snapshotTree(t, f.profile()))
		})
	}
}

// TestPlanInstallNeverCreatesTheExtensionsDirectory pins that a plan which cannot
// proceed creates nothing, not even the profile directory that is missing.
func TestPlanInstallNeverCreatesTheExtensionsDirectory(t *testing.T) {
	f := newFixture(t) // no integration: the profile and Herdr's file are absent

	_, err := f.plan()
	assertRefusal(t, "PlanInstall", err, ErrIntegrationMissing)
	assertAbsent(t, f.extensionsDir(""))
	assertAbsent(t, f.profile())
}

// TestPlanInstallResolvesTheEffectiveProfile pins that the plan targets the same
// extension directory Install writes into, here for a named profile.
func TestPlanInstallResolvesTheEffectiveProfile(t *testing.T) {
	f := newFixture(t)
	f.installed(t) // the default profile is integrated and has the extension
	f.env["OMP_PROFILE"] = "qa"
	f.installIntegration(t, "qa")

	plan, err := f.plan()
	if err != nil {
		t.Fatalf("PlanInstall under OMP_PROFILE=qa: %v", err)
	}
	if want := f.extensionPath("qa"); plan.Extension != want || plan.Action != ActionCreate {
		t.Fatalf("plan = %+v, want a create at %s", plan, want)
	}
	if got := installEffect(t, f.extensionPath("qa"), f.install); got != ActionCreate {
		t.Fatalf("Install under OMP_PROFILE=qa took action %q, want %q", got, ActionCreate)
	}
}

// TestPlanInstallAndInstallAgreeWhenTheExtensionCannotBeRead pins that an
// extension the plugin cannot read is refused with the same write_failed code by
// the plan and by the install, and that neither of them touches it.
func TestPlanInstallAndInstallAgreeWhenTheExtensionCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.installIntegration(t, "")
	target := f.extensionPath("")
	// A directory where the extension belongs: reading it fails, and that
	// failure is not "absent", so it must not be taken for a create.
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create the blocking directory: %v", err)
	}
	before := snapshotTree(t, f.profile())

	_, planErr := f.plan()
	assertRefusal(t, "PlanInstall", planErr, ErrWriteFailed)
	assertRefusal(t, "Install", f.install(), ErrWriteFailed)
	assertSameTree(t, before, snapshotTree(t, f.profile()))
}
