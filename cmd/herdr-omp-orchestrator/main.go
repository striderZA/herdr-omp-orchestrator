// Command herdr-omp-orchestrator is the plugin's single Go executable.
//
// Herdr runs it as a plugin action (for example `setup`) and the OMP extension
// runs it as `bridge <method>` for every tool invocation. Both routes read the
// environment they were started with and answer with one JSON object, so a
// human reading a Herdr plugin log and the extension parsing stdout see the
// same result.
//
// Later tasks add the `bind`, `startup`, `event` and board routes; this file
// deliberately wires only the routes that exist.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/bridge"
	"herdr-omp-orchestrator/internal/setup"
)

// bridgeTimeout bounds one bridge invocation in the executable itself. The
// extension imposes its own, shorter timeout; this is the backstop that keeps a
// wedged process from outliving the turn that started it.
const bridgeTimeout = 2 * time.Minute

const usage = "usage: herdr-omp-orchestrator <setup|bridge <method>>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	env := environMap(os.Environ())
	switch os.Args[1] {
	case "setup":
		os.Exit(runSetup(env, os.Stdout, os.Stderr))
	case "bridge":
		os.Exit(runBridge(env, os.Stdin, os.Stdout, os.Stderr, os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "herdr-omp-orchestrator: unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

// runSetup installs the plugin's OMP extension and reports the outcome. The
// executable path it embeds is this binary, resolved at runtime, so a moved or
// reinstalled plugin refreshes the embedded path instead of leaving an
// extension that points at a stale executable.
func runSetup(env map[string]string, stdout, stderr io.Writer) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: setup: %v\n", err)
		return emit(stdout, stderr, failureBody("executable_unavailable", "the plugin executable path could not be resolved"))
	}
	if err := setup.Install(env, executable); err != nil {
		return emit(stdout, stderr, setupFailure(err))
	}
	path, err := setup.ExtensionPath(env)
	if err != nil {
		return emit(stdout, stderr, setupFailure(err))
	}
	return emit(stdout, stderr, map[string]any{
		"ok":        true,
		"extension": path,
		"protocol":  setup.Protocol,
	})
}

// runBridge answers one extension call. The request is read from stdin with a
// hard bound and the answer is written to stdout, so an oversized or malformed
// request is still answered in the documented envelope.
func runBridge(env map[string]string, stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	method := ""
	if len(args) > 0 {
		method = strings.TrimSpace(args[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), bridgeTimeout)
	defer cancel()

	out, err := bridge.Run(ctx, env, stdin, bridge.Options{ArgvMethod: method})
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: bridge: %v\n", err)
		return 2
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: bridge: %v\n", err)
		return 2
	}
	return 0
}

// setupFailure renders a setup failure as the same envelope the bridge uses, so
// both routes answer with a stable code and a bounded message. It never quotes
// file contents: only the code and the message setup produced.
func setupFailure(err error) map[string]any {
	var coded *setup.Error
	if errors.As(err, &coded) {
		return failureBody(coded.Code(), err.Error())
	}
	return failureBody("internal", err.Error())
}

func failureBody(code, message string) map[string]any {
	return map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message}}
}

// emit writes one JSON answer and returns the process exit status.
func emit(stdout, stderr io.Writer, body map[string]any) int {
	encoded, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: %v\n", err)
		return 2
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: %v\n", err)
		return 2
	}
	if ok, _ := body["ok"].(bool); !ok {
		return 1
	}
	return 0
}

// environMap turns a process environment into a map without losing entries that
// contain '=' inside their value.
func environMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			out[key] = value
		}
	}
	return out
}
