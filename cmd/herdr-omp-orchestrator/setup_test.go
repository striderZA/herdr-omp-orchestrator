package main

// These tests drive the setup route through runSetup against real temporary
// profiles. Herdr's integration status is answered by this test binary through
// the route fixture in main_test.go, so the Herdr check is a real child process.
// Every profile, plugin root and state directory is under t.TempDir(); nothing
// here touches the developer's own ~/.omp tree.

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/setup"
)

// setupRouteFixture is one isolated profile for the setup route: Herdr's OMP
// integration is installed in the default profile and the canonical extension
// source sits in a temporary plugin root.
type setupRouteFixture struct {
	home string
	env  map[string]string
}

func newSetupRouteFixture(t *testing.T) setupRouteFixture {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()

	source, err := os.ReadFile(filepath.Join("..", "..", "omp", "herdr-orchestrator.ts"))
	if err != nil {
		t.Fatalf("read the canonical extension source: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "omp"), 0o755); err != nil {
		t.Fatalf("create the plugin root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "omp", "herdr-orchestrator.ts"), source, 0o644); err != nil {
		t.Fatalf("copy the canonical extension source: %v", err)
	}

	extensions := filepath.Join(home, ".omp", "agent", "extensions")
	if err := os.MkdirAll(extensions, 0o755); err != nil {
		t.Fatalf("create the extensions directory: %v", err)
	}
	integration := filepath.Join(extensions, "herdr-omp-agent-state.ts")
	if err := os.WriteFile(integration, []byte("export default {};\n"), 0o644); err != nil {
		t.Fatalf("write Herdr's integration file: %v", err)
	}

	herdr, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve the test binary as the Herdr CLI: %v", err)
	}
	return setupRouteFixture{home: home, env: map[string]string{
		"HOME":                   home,
		"HERDR_PLUGIN_ROOT":      root,
		"HERDR_PLUGIN_STATE_DIR": filepath.Join(t.TempDir(), "state"),
		"HERDR_BIN_PATH":         herdr,
		routeFixtureEnv:          "1",
		routeFixtureCalls:        filepath.Join(t.TempDir(), "herdr-calls.jsonl"),
		routeFixtureStatus:       "omp: current (v10) (" + integration + ")",
	}}
}

// extension is the plugin's extension file in the fixture's default profile.
func (f setupRouteFixture) extension() string {
	return filepath.Join(f.home, ".omp", "agent", "extensions", setup.ExtensionName)
}

// configTree records every file under the fixture's OMP configuration tree with
// its content, so a test can prove a setup route wrote nothing.
func (f setupRouteFixture) configTree(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(f.home, ".omp")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[path] = "<dir>"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// setupAnswer is the one JSON answer the setup route writes.
type setupAnswer struct {
	OK        bool   `json:"ok"`
	DryRun    bool   `json:"dryRun"`
	Action    string `json:"action"`
	Extension string `json:"extension"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func runSetupRoute(t *testing.T, env map[string]string, args ...string) (int, setupAnswer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := runSetup(env, args, &stdout, &stderr)
	var answer setupAnswer
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %q: %v (stderr %q)", stdout.String(), err, stderr.String())
	}
	return status, answer
}

// TestSetupDryRunReportsTheActionAndWritesNothing pins the preview for each state
// the extension can be in: the answer names what setup would do, and the OMP
// configuration tree is exactly what it was before the preview ran.
func TestSetupDryRunReportsTheActionAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, f setupRouteFixture)
		want    string
	}{
		{
			name:    "create: no extension yet",
			prepare: func(t *testing.T, f setupRouteFixture) {},
			want:    "create",
		},
		{
			name: "refresh: a managed extension with other content",
			prepare: func(t *testing.T, f setupRouteFixture) {
				stale := setup.ManagedMarker + "\nconst PLUGIN_EXECUTABLE = \"C:\\\\moved\\\\herdr-omp-orchestrator.exe\";\n"
				if err := os.WriteFile(f.extension(), []byte(stale), 0o644); err != nil {
					t.Fatalf("write the stale extension: %v", err)
				}
			},
			want: "refresh",
		},
		{
			name: "unchanged: the extension is already what setup writes",
			prepare: func(t *testing.T, f setupRouteFixture) {
				if status, answer := runSetupRoute(t, f.env); status != 0 || !answer.OK {
					t.Fatalf("prepare the installed extension: status %d, answer %+v", status, answer)
				}
			},
			want: "unchanged",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupRouteFixture(t)
			tc.prepare(t, f)
			before := f.configTree(t)

			status, answer := runSetupRoute(t, f.env, "--dry-run")
			if status != 0 || !answer.OK || !answer.DryRun || answer.Action != tc.want || answer.Extension != f.extension() {
				t.Fatalf("dry run = status %d, answer %+v; want an ok dry run with action %q at %s",
					status, answer, tc.want, f.extension())
			}
			if after := f.configTree(t); !maps.Equal(before, after) {
				t.Fatalf("dry run changed the OMP configuration tree:\nbefore: %v\nafter:  %v", before, after)
			}
		})
	}
}

// TestSetupRefusesUnrecognizedArgumentsBeforeInstalling pins that a mistyped flag
// fails with the stable usage code and exit status 2, and never reaches the real
// install: a refused invocation leaves the configuration tree and the extension
// exactly as they were.
func TestSetupRefusesUnrecognizedArgumentsBeforeInstalling(t *testing.T) {
	for _, args := range [][]string{
		{"--dryrun"},
		{"-dry-run"},
		{"--dry-run=false"},
		{"--dry-run", "--dry-run"},
		{"--dry-run", "extra"},
		{"extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newSetupRouteFixture(t)
			before := f.configTree(t)

			status, answer := runSetupRoute(t, f.env, args...)
			if status != 2 {
				t.Fatalf("status = %d for %q, want 2", status, args)
			}
			if answer.OK || answer.Error == nil || answer.Error.Code != "usage" {
				t.Fatalf("answer = %+v for %q, want the stable usage code", answer, args)
			}
			if after := f.configTree(t); !maps.Equal(before, after) {
				t.Fatalf("a refused invocation changed the OMP configuration tree:\nbefore: %v\nafter:  %v", before, after)
			}
			if _, err := os.Stat(f.extension()); !os.IsNotExist(err) {
				t.Fatalf("a refused invocation installed the extension (stat err %v)", err)
			}
		})
	}
}

// TestSetupWithoutArgumentsStillInstalls keeps the setup action's own behavior:
// no arguments installs the extension and answers with its path.
func TestSetupWithoutArgumentsStillInstalls(t *testing.T) {
	f := newSetupRouteFixture(t)

	status, answer := runSetupRoute(t, f.env)
	if status != 0 || !answer.OK || answer.DryRun {
		t.Fatalf("setup = status %d, answer %+v; want an installed extension", status, answer)
	}
	if answer.Extension != f.extension() {
		t.Fatalf("setup answered extension %q, want %q", answer.Extension, f.extension())
	}
	if _, err := os.Stat(f.extension()); err != nil {
		t.Fatalf("setup did not write the extension: %v", err)
	}
}

// TestSetupDryRunRefusesWhereSetupRefuses pins that the preview gives the same
// stable refusal and exit status as the real setup it previews, and that neither
// of them writes when it refuses.
func TestSetupDryRunRefusesWhereSetupRefuses(t *testing.T) {
	f := newSetupRouteFixture(t)
	f.env[routeFixtureStatus] = "omp: not installed (" + filepath.Join(f.home, ".omp", "agent", "extensions", "herdr-omp-agent-state.ts") + ")"
	before := f.configTree(t)

	planStatus, planAnswer := runSetupRoute(t, f.env, "--dry-run")
	installStatus, installAnswer := runSetupRoute(t, f.env)

	for _, got := range []struct {
		name   string
		status int
		answer setupAnswer
	}{
		{"dry run", planStatus, planAnswer},
		{"setup", installStatus, installAnswer},
	} {
		if got.status != 1 || got.answer.OK || got.answer.Error == nil || got.answer.Error.Code != "integration_missing" {
			t.Fatalf("%s = status %d, answer %+v; want a refusal with integration_missing and status 1",
				got.name, got.status, got.answer)
		}
	}
	if after := f.configTree(t); !maps.Equal(before, after) {
		t.Fatalf("a refused preview or setup changed the OMP configuration tree")
	}
}
