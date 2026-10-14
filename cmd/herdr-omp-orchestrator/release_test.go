package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// releaseWorkflow is the release workflow, relative to this package.
var releaseWorkflow = filepath.Join("..", "..", ".github", "workflows", "release.yml")

// releaseTagRuleLine captures the SemVer pattern the workflow's tag check runs.
var releaseTagRuleLine = regexp.MustCompile(`(?m)^\s*semver='(.*)'\s*$`)

// TestReleaseTagRuleAcceptsExactlySemVer pins the release tag rule to the pattern
// the workflow runs in bash. The test compiles the same text, so a weakened or
// reformatted rule fails go test before a release can publish it.
func TestReleaseTagRuleAcceptsExactlySemVer(t *testing.T) {
	text, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}
	found := releaseTagRuleLine.FindSubmatch(text)
	if found == nil {
		t.Fatalf("%s has no semver='...' assignment for the tag check", releaseWorkflow)
	}
	rule, err := regexp.Compile(string(found[1]))
	if err != nil {
		t.Fatalf("the workflow's tag rule does not compile: %v", err)
	}
	for _, v := range []string{
		"0.0.0", "0.1.0", "10.20.30",
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92", "1.0.0-rc.1",
		"1.0.0-alpha-a.b-c-somethinglong+build.1-aef.1-its-okay",
		"1.0.0+20130313144700", "1.0.0-beta+exp.sha.5114f85", "1.0.0+21AF26D3----117B344092BD",
		"1.0.0-preview.2026-09-21", "0.2.0+build.5", "1.0.0-alpha+001",
	} {
		if !rule.MatchString(v) {
			t.Errorf("SemVer version %q is refused by the workflow's tag rule", v)
		}
	}
	for _, v := range []string{
		"01.2.3", "1.02.3", "1.2.03", "1.2", "1.2.3.4", "1..3", ".1.2", "1.2.", "x.y.z", "1.2.x",
		"-1.2.3", "1.-2.3", "1.2.3-", "1.2.3-01", "1.2.3-a..b", "1.2.3+", "1.2.3+a..b",
		"1.2.3-rc_1", "1.2.3+build_1", " 1.2.3", "1.2.3 ", "1.2.3-rc.1*", "1.2.3/4", "1.2.3*",
		"1.2.3\"x", "0x1x0", "v1.2.3", "",
	} {
		if rule.MatchString(v) {
			t.Errorf("non-SemVer version %q is accepted by the workflow's tag rule", v)
		}
	}
}
