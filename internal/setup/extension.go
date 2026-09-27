// Package setup owns the plugin's explicit setup action: it installs exactly
// one OMP extension file into OMP's effective profile extension directory.
//
// Setup never edits OMP configuration and never writes Herdr's own
// herdr-omp-agent-state.ts: it only creates or refreshes herdr-orchestrator.ts
// beside it, after proving that Herdr's OMP integration is installed in that
// same effective directory and that it has a usable Herdr CLI and plugin state
// directory to embed. A failed proof changes nothing on disk.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Protocol is the bridge protocol version the generated extension speaks and
// embeds. The bridge refuses any other request version, so a stale installed
// extension fails visibly instead of talking a half-understood protocol.
const Protocol = 1

// ExtensionName is the plugin's own extension file. It is deliberately distinct
// from Herdr's HerdrIntegrationFile so the two can coexist in one directory.
const ExtensionName = "herdr-orchestrator.ts"

// HerdrIntegrationFile is Herdr's own OMP integration. The plugin verifies it
// exists and never writes to it.
const HerdrIntegrationFile = "herdr-omp-agent-state.ts"

// ManagedMarker is the first line of every file this plugin owns, in the
// repository source and in the installed copy. Setup refuses to replace a
// herdr-orchestrator.ts that does not start with it.
const ManagedMarker = "// herdr-omp-orchestrator managed OMP extension (edit this source, not the installed copy)."

// Placeholders in the canonical source that setup substitutes with JSON-escaped
// absolute paths.
const (
	PlaceholderExecutable = "@@OMP_ORCH_EXECUTABLE@@"
	PlaceholderStateDir   = "@@OMP_ORCH_STATE_DIR@@"
	PlaceholderHerdrBin   = "@@OMP_ORCH_HERDR_BINARY@@"
)

// sourceRelPath is the canonical extension source inside the plugin root.
var sourceRelPath = filepath.Join("omp", "herdr-orchestrator.ts")

const (
	// statusTimeout bounds `herdr integration status`.
	statusTimeout = 15 * time.Second
	// maxStatusBytes bounds the status report; it is a few short lines.
	maxStatusBytes = 64 << 10
	// defaultConfigDir is OMP's canonical user configuration directory name
	// (packages/utils/src/dirs.ts: CONFIG_DIR_NAME).
	defaultConfigDir = ".omp"
)

// Error is a setup failure with a stable machine-readable code, so the action's
// JSON answer and its tests can classify it without matching message text.
type Error struct {
	code    string
	message string
	err     error
}

// Code reports the stable failure code.
func (e *Error) Code() string { return e.code }

func (e *Error) Error() string {
	if e.err != nil {
		return "setup: " + e.code + ": " + e.message + ": " + e.err.Error()
	}
	return "setup: " + e.code + ": " + e.message
}

// Unwrap keeps the underlying cause reachable through errors.Is/As.
func (e *Error) Unwrap() error { return e.err }

// Is matches by code, so callers compare against the sentinels below.
func (e *Error) Is(target error) bool {
	var other *Error
	if errors.As(target, &other) {
		return other.code == e.code
	}
	return false
}

// Sentinel failures. errors.Is(err, ErrX) is the supported comparison.
var (
	ErrProfileUnavailable     = &Error{code: "profile_unavailable"}
	ErrExecutableUnavailable  = &Error{code: "executable_unavailable"}
	ErrStateDirUnavailable    = &Error{code: "state_dir_unavailable"}
	ErrHerdrBinaryUnavailable = &Error{code: "herdr_binary_unavailable"}
	ErrIntegrationMissing     = &Error{code: "integration_missing"}
	ErrSourceUnavailable      = &Error{code: "source_unavailable"}
	ErrExtensionConflict      = &Error{code: "extension_conflict"}
	ErrWriteFailed            = &Error{code: "write_failed"}
)

func fail(sentinel *Error, format string, args ...any) error {
	return &Error{code: sentinel.code, message: fmt.Sprintf(format, args...)}
}

func wrap(sentinel *Error, err error, format string, args ...any) error {
	return &Error{code: sentinel.code, message: fmt.Sprintf(format, args...), err: err}
}

// ---------------------------------------------------------------------------
// profile resolution
// ---------------------------------------------------------------------------

// ExtensionDir resolves OMP's effective user extension directory for the
// environment env, following OMP's own active-profile rules:
//
//   - OMP_PROFILE if it is defined (even empty), otherwise PI_PROFILE.
//   - "default", empty and whitespace select the default profile.
//   - a named profile lives in $HOME/$PI_CONFIG_DIR/profiles/<name>/agent and
//     ignores PI_CODING_AGENT_DIR.
//   - the default profile uses PI_CODING_AGENT_DIR when set, then
//     $HOME/$PI_CONFIG_DIR/agent, then $HOME/.omp/agent.
//
// The environment is taken as given, never read from the process: the caller
// passes exactly the environment the plugin action received.
func ExtensionDir(env map[string]string) (string, error) {
	home := strings.TrimSpace(env["HOME"])
	if home == "" {
		home = strings.TrimSpace(env["USERPROFILE"])
	}
	if home == "" {
		return "", fail(ErrProfileUnavailable, "neither HOME nor USERPROFILE is set, so OMP's profile directory cannot be resolved")
	}
	configDir := strings.TrimSpace(env["PI_CONFIG_DIR"])
	if configDir == "" {
		configDir = defaultConfigDir
	}

	profile := env["OMP_PROFILE"]
	if _, defined := env["OMP_PROFILE"]; !defined {
		profile = env["PI_PROFILE"]
	}
	profile = strings.TrimSpace(profile)

	if profile != "" && profile != "default" {
		return filepath.Join(home, configDir, "profiles", profile, "agent", "extensions"), nil
	}
	if agentDir := strings.TrimSpace(env["PI_CODING_AGENT_DIR"]); agentDir != "" {
		return filepath.Join(agentDir, "extensions"), nil
	}
	return filepath.Join(home, configDir, "agent", "extensions"), nil
}

// ExtensionPath is the absolute path of the file setup installs.
func ExtensionPath(env map[string]string) (string, error) {
	dir, err := ExtensionDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ExtensionName), nil
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// Install creates or refreshes the plugin's OMP extension for env, refusing to
// change anything when the integration, the plugin state directory, the Herdr
// CLI or the plugin executable cannot be verified. executable is the absolute
// path of the plugin executable OMP will spawn for every bridge call (the setup
// action passes os.Executable).
func Install(env map[string]string, executable string) error {
	pluginExecutable, err := checkExecutable(ErrExecutableUnavailable, "plugin executable", executable)
	if err != nil {
		return err
	}
	stateDir := strings.TrimSpace(env["HERDR_PLUGIN_STATE_DIR"])
	if stateDir == "" {
		return fail(ErrStateDirUnavailable, "HERDR_PLUGIN_STATE_DIR is not set, so the extension would have no orchestrator state to talk to")
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return wrap(ErrStateDirUnavailable, err, "resolve HERDR_PLUGIN_STATE_DIR %q", stateDir)
	}
	herdrBinary := strings.TrimSpace(env["HERDR_BIN_PATH"])
	if herdrBinary == "" {
		return fail(ErrHerdrBinaryUnavailable, "HERDR_BIN_PATH is not set, so the extension would have no verified Herdr CLI to embed")
	}
	herdrBinary, err = checkExecutable(ErrHerdrBinaryUnavailable, "Herdr binary", herdrBinary)
	if err != nil {
		return err
	}

	dir, err := ExtensionDir(env)
	if err != nil {
		return err
	}
	if err := checkIntegration(env, dir, herdrBinary); err != nil {
		return err
	}

	source, err := readSource(env)
	if err != nil {
		return err
	}
	content, err := render(source, pluginExecutable, stateDir, herdrBinary)
	if err != nil {
		return err
	}

	target := filepath.Join(dir, ExtensionName)
	if err := writeIfChanged(target, content); err != nil {
		return err
	}
	return nil
}

// checkExecutable resolves path to an absolute path and requires it to be an
// existing regular file. sentinel is the failure to report, because a stale
// plugin executable and a stale Herdr binary mean different things to a human:
// the first needs a reinstall, the second needs the integration re-verified.
func checkExecutable(sentinel *Error, what, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fail(sentinel, "%s path is empty", what)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", wrap(sentinel, err, "resolve %s %s", what, path)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", wrap(sentinel, err, "%s %s is missing or stale", what, absolute)
	}
	if !info.Mode().IsRegular() {
		return "", fail(sentinel, "%s %s is not a regular file", what, absolute)
	}
	return absolute, nil
}

// checkIntegration proves Herdr's OMP integration is installed for this
// environment before anything is written:
//
//   - `herdr integration status` must report omp installed. That command is run
//     through the CLI (not through internal/herdr) because it prints a text
//     report rather than the JSON envelope the typed client decodes; the
//     caller's environment is passed through so the profile that Herdr resolves
//     is the one this plugin just resolved.
//   - Herdr's own herdr-omp-agent-state.ts must exist in the same effective
//     extension directory the extension will be installed into. Herdr 0.9.1
//     does not scope that path by OMP profile, so this second check is what
//     makes a named profile fail explicitly instead of silently installing an
//     extension for an integration that is not there.
func checkIntegration(env map[string]string, dir, herdrBinary string) error {
	const remedy = "install it with `herdr integration install omp` (for the profile in the environment the plugin runs in) and re-run setup"

	report, err := herdrStatus(env, herdrBinary)
	if err != nil {
		return wrap(ErrIntegrationMissing, err, "Herdr's OMP integration status could not be read; %s", remedy)
	}
	if !statusReportsInstalled(report) {
		return fail(ErrIntegrationMissing, "Herdr reports its OMP integration is not installed; %s", remedy)
	}
	integrationFile := filepath.Join(dir, HerdrIntegrationFile)
	if info, err := os.Stat(integrationFile); err != nil || !info.Mode().IsRegular() {
		return fail(ErrIntegrationMissing, "Herdr's own %s is not installed in %s (the extension directory for this profile); %s",
			HerdrIntegrationFile, dir, remedy)
	}
	return nil
}

// statusReportsInstalled reads the text report of `herdr integration status`.
//
// Herdr's vocabulary for one integration line is what it prints, not a fixed
// word: observed on Herdr 0.9.1-preview this machine prints
// `omp: not installed (<path>)` before the integration exists and
// `omp: current (v10) (<path>)` once it is installed and up to date. An
// installed-but-outdated integration is still installed, so the only wording
// that proves absence is "not installed": every other status on an `omp:` line
// is accepted, and a report with no `omp:` line at all proves nothing.
func statusReportsInstalled(report string) bool {
	for _, line := range strings.Split(report, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "omp:") {
			continue
		}
		return !strings.Contains(strings.ToLower(trimmed), "not installed")
	}
	return false
}

// herdrStatus runs `herdr integration status` with a bounded report and an
// explicit timeout, argv only, never a shell.
func herdrStatus(env map[string]string, herdrBinary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, herdrBinary, "integration", "status")
	cmd.Env = mergedEnv(env)
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = maxStatusBytes, maxStatusBytes
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if i := strings.IndexAny(detail, "\r\n"); i >= 0 {
			detail = strings.TrimSpace(detail[:i])
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("herdr integration status: %s", detail)
	}
	return stdout.String(), nil
}

// mergedEnv overlays env on the process environment, so the child sees the
// caller's values (and a Windows process still gets SystemRoot/PATH) while the
// parent's own environment is never mutated.
func mergedEnv(env map[string]string) []string {
	merged := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}
	for key, value := range env {
		merged[key] = value
	}
	out := make([]string, 0, len(merged))
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// readSource reads the canonical extension source from the plugin root, falling
// back to the working directory (plugin commands run with the plugin directory
// as their working directory, which keeps local development working without a
// linked plugin).
func readSource(env map[string]string) (string, error) {
	candidates := make([]string, 0, 2)
	if root := strings.TrimSpace(env["HERDR_PLUGIN_ROOT"]); root != "" {
		candidates = append(candidates, filepath.Join(root, sourceRelPath))
	} else {
		candidates = append(candidates, sourceRelPath)
	}

	var lastErr error
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		source := string(data)
		if err := checkSource(source); err != nil {
			return "", err
		}
		return source, nil
	}
	return "", wrap(ErrSourceUnavailable, lastErr, "cannot read the extension source %q", candidates[0])
}

// checkSource refuses a source that is not ours: a file without the managed
// marker would make the installed extension unrecognizable on the next run.
func checkSource(source string) error {
	if !strings.HasPrefix(source, ManagedMarker+"\n") {
		return fail(ErrSourceUnavailable, "the extension source does not start with the managed marker %q", ManagedMarker)
	}
	return nil
}

// render substitutes the JSON-escaped absolute paths into the canonical source.
// The source supplies the surrounding quotes, so only the string body is
// substituted; a path containing a quote or backslash is escaped and cannot
// terminate the literal it lands in.
func render(source, executable, stateDir, herdrBinary string) (string, error) {
	for _, replacement := range []struct {
		token string
		value string
	}{
		{PlaceholderExecutable, executable},
		{PlaceholderStateDir, stateDir},
		{PlaceholderHerdrBin, herdrBinary},
	} {
		if !strings.Contains(source, replacement.token) {
			return "", fail(ErrSourceUnavailable, "the extension source is missing the placeholder %q", replacement.token)
		}
		source = strings.ReplaceAll(source, replacement.token, jsStringBody(replacement.value))
	}
	return source, nil
}

// jsStringBody renders s as the body of a JSON string literal: every character
// that would terminate or alter a TypeScript string literal is escaped. JSON
// string escaping is a strict subset of TypeScript's, so the result is safe
// between the source's own quotes.
func jsStringBody(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil || len(encoded) < 2 {
		// json.Marshal cannot fail for a string, but never silently emit an
		// unescaped path if that ever changes.
		return ""
	}
	return string(encoded[1 : len(encoded)-1])
}

// writeIfChanged installs content at target atomically: a same-directory
// temporary file is written, flushed, closed and renamed over the target, so a
// reader never observes a partial extension. Byte-identical content is left
// untouched, which keeps a repeated setup action a true no-op.
func writeIfChanged(target, content string) error {
	if existing, err := os.ReadFile(target); err == nil {
		if string(existing) == content {
			return nil
		}
		if !strings.HasPrefix(string(existing), ManagedMarker+"\n") {
			return fail(ErrExtensionConflict, "%q already exists and was not written by this plugin; move it away or rename it before running setup", target)
		}
	} else if !os.IsNotExist(err) {
		return wrap(ErrWriteFailed, err, "read existing extension %q", target)
	}

	dir := filepath.Dir(target)
	temp, err := os.CreateTemp(dir, "."+ExtensionName+".*.tmp")
	if err != nil {
		return wrap(ErrWriteFailed, err, "create a temporary extension beside %q", target)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	if _, err := temp.WriteString(content); err != nil {
		_ = temp.Close()
		return wrap(ErrWriteFailed, err, "write the temporary extension %q", tempName)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return wrap(ErrWriteFailed, err, "flush the temporary extension %q", tempName)
	}
	if err := temp.Close(); err != nil {
		return wrap(ErrWriteFailed, err, "close the temporary extension %q", tempName)
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return wrap(ErrWriteFailed, err, "set permissions on the temporary extension %q", tempName)
	}
	if err := os.Rename(tempName, target); err != nil {
		return wrap(ErrWriteFailed, err, "install %q", target)
	}
	return nil
}

// boundedBuffer fails closed once limit bytes were written, so an oversized
// report is an error rather than a silently truncated one.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("output exceeded %d bytes", b.limit)
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) String() string { return b.buf.String() }
