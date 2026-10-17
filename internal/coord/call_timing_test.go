package coord

import (
	"strings"
	"testing"
	"time"
)

func TestShouldLogCallTimings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failed  bool
		elapsed time.Duration
		want    bool
	}{
		{name: "failed subtest", failed: true, elapsed: time.Second, want: true},
		{name: "slow subtest", elapsed: 31 * time.Second, want: true},
		{name: "fast passing subtest", elapsed: 30 * time.Second, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLogCallTimings(tc.failed, tc.elapsed); got != tc.want {
				t.Fatalf("shouldLogCallTimings(%t, %s) = %t, want %t", tc.failed, tc.elapsed, got, tc.want)
			}
		})
	}
}

func TestFormatCallTimingsPreservesOrderAndIncludesDurationAndOutcome(t *testing.T) {
	records := []callTimingRecord{
		{offset: time.Second, duration: 250 * time.Millisecond, argv: []string{"agent", "get", "w1:p1"}, outcome: "ok"},
		{offset: 2 * time.Second, duration: time.Second, argv: []string{"git", "commit"}, outcome: "exit status 1"},
	}
	lines := formatCallTimings(records)
	if len(lines) != 2 {
		t.Fatalf("formatCallTimings returned %d lines, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "1s") || !strings.Contains(lines[0], "250ms") || !strings.Contains(lines[0], "ok") || !strings.Contains(lines[0], `"agent" "get" "w1:p1"`) {
		t.Fatalf("first formatted record omits fields: %q", lines[0])
	}
	if !strings.Contains(lines[1], "2s") || !strings.Contains(lines[1], "1s") || !strings.Contains(lines[1], "exit status 1") || !strings.Contains(lines[1], `"git" "commit"`) {
		t.Fatalf("second formatted record omits fields: %q", lines[1])
	}
	if strings.Index(lines[0], "agent") > strings.Index(lines[1], "git") {
		t.Fatalf("formatted records are out of order: %q", lines)
	}
}
