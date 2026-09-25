package setup_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExtensionLoadsUnderOMPSchemaBuilder runs omp/load-extension.check.ts.
//
// Transpiling the extension is not loading it: OMP builds every registered tool's
// parameters schema while it runs the factory, so a builder-level mistake (a
// mutable value handed to `.default(...)`, which OMP rejects with "A mutable
// default value must be specified as a factory") makes the whole extension fail to
// load and registers none of its tools. That failure only shows on a real machine,
// as "Warning: Failed to load extension …", which is what this check exists to
// prevent: it executes the factory against a stub ExtensionAPI whose schema builder
// is OMP's own, then parses values through the schemas it recorded.
//
// The check needs Bun and the machine's OMP installation (the harness imports
// OMP's schema builder by name). Where either is unavailable the test skips with
// the reason, so a machine without OMP does not turn a green tree red.
func TestExtensionLoadsUnderOMPSchemaBuilder(t *testing.T) {
	root := pluginRoot(t)
	check := filepath.Join(root, "omp", "load-extension.check.ts")
	if _, err := os.Stat(check); err != nil {
		t.Skipf("the load check is not in this checkout: %v", err)
	}
	source := filepath.Join(root, "omp", "herdr-orchestrator.ts")
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("the canonical extension source is missing: %v", err)
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skipf("bun is not installed, so the extension cannot be loaded here: %v", err)
	}
	builder := filepath.Join(homeDir(t), "node_modules", "@oh-my-pi", "omptype")
	if _, err := os.Stat(builder); err != nil {
		t.Skipf("OMP's schema builder is not installed, so it cannot be loaded here: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "run", "omp/load-extension.check.ts")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "NODE_PATH="+filepath.Dir(builder))
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		t.Fatalf("the extension did not load under OMP's schema builder: %v\n%s", err, output)
	}
	if !strings.Contains(output, "load check ok") {
		t.Fatalf("the load check did not report success:\n%s", output)
	}
}

// pluginRoot finds the plugin working tree from this package's directory.
func pluginRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the plugin root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "herdr-plugin.toml")); err != nil {
		t.Skipf("%s is not the plugin root: %v", root, err)
	}
	return root
}

// homeDir is the user's home directory, the way the setup package resolves it
// when HOME is not set on Windows.
func homeDir(t *testing.T) string {
	t.Helper()
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home := strings.TrimSpace(os.Getenv("USERPROFILE"))
	if home == "" {
		t.Skip("neither HOME nor USERPROFILE is set, so OMP's modules cannot be found")
	}
	return home
}
