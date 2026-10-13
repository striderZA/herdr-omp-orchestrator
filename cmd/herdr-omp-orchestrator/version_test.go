package main

import (
	"bytes"
	"strings"
	"testing"
)

// stampVersion sets the reported version for one test the way a release build
// does at link time, and restores the previous value when the test ends.
func stampVersion(t *testing.T, v string) {
	t.Helper()
	previous := version
	version = v
	t.Cleanup(func() { version = previous })
}

func TestVersionPrintsTheStampedRelease(t *testing.T) {
	stampVersion(t, "0.2.0")
	var stdout, stderr bytes.Buffer
	if status := runVersion(nil, &stdout, &stderr); status != 0 {
		t.Fatalf("runVersion status = %d, want 0 (stderr %q)", status, stderr.String())
	}
	if got, want := stdout.String(), "herdr-omp-orchestrator 0.2.0\n"; got != want {
		t.Fatalf("runVersion printed %q, want %q", got, want)
	}
}

func TestVersionRefusesArgumentsWithoutPrintingAVersion(t *testing.T) {
	stampVersion(t, "0.2.0")
	for _, args := range [][]string{{"extra"}, {"--verbose"}} {
		var stdout, stderr bytes.Buffer
		if status := runVersion(args, &stdout, &stderr); status != 2 {
			t.Fatalf("runVersion(%q) status = %d, want the usage status 2", args, status)
		}
		if stdout.Len() != 0 {
			t.Fatalf("runVersion(%q) printed %q to stdout; a refusal prints no version", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "usage: herdr-omp-orchestrator --version") {
			t.Fatalf("runVersion(%q) stderr = %q, want the usage line", args, stderr.String())
		}
	}
}
