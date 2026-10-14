package main

import (
	"fmt"
	"io"
)

// version is the release version this executable reports. A release build sets
// it at link time with `-ldflags "-X main.version=<version>"`, as
// .github/workflows/release.yml does. A build without that flag reports "dev".
var version = "dev"

// runVersion answers `--version` with the version the executable was built
// with. The route takes no arguments: anything after the flag is refused, so a
// mistyped invocation never prints a version it was not asked for.
func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "herdr-omp-orchestrator: --version takes no arguments")
		fmt.Fprintln(stderr, "usage: herdr-omp-orchestrator --version")
		return 2
	}
	fmt.Fprintf(stdout, "herdr-omp-orchestrator %s\n", version)
	return 0
}
