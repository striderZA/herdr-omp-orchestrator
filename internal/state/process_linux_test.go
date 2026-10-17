//go:build linux

package state

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// procStatLine builds a /proc/<pid>/stat line with the given command name, state
// and start time. Fields 4 through 21 are zero.
func procStatLine(comm string, state byte, start int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "4242 (%s) %c", comm, state)
	for field := 4; field <= 21; field++ {
		b.WriteString(" 0")
	}
	fmt.Fprintf(&b, " %d", start)
	return b.String()
}

func TestParseProcStatReadsStartTimeAfterTheCommandName(t *testing.T) {
	for _, comm := range []string{"sleep", "my app", "a)b", "x) (y"} {
		state, start, err := parseProcStat(procStatLine(comm, 'S', 987654))
		if err != nil {
			t.Fatalf("parseProcStat(comm %q): %v", comm, err)
		}
		if state != 'S' || start != 987654 {
			t.Fatalf("parseProcStat(comm %q) = (%c, %d), want (S, 987654)", comm, state, start)
		}
	}
}

func TestParseProcStatRejectsLinesItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no command name":   "4242 sleep S 0 0 0",
		"too few fields":    "4242 (sleep) S 0 0 0",
		"non-numeric start": strings.TrimSuffix(procStatLine("sleep", 'S', 1), "1") + "soon",
	}
	for name, line := range cases {
		if _, _, err := parseProcStat(line); err == nil {
			t.Fatalf("parseProcStat(%s) returned no error", name)
		}
	}
}

func TestProcessAliveIsFalseForAnUnreapedExitedProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	pid := cmd.Process.Pid
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	deadline := time.Now().Add(5 * time.Second)
	var state byte
	var start int64
	for {
		if data, err := os.ReadFile(statPath); err == nil {
			if s, st, perr := parseProcStat(string(data)); perr == nil && s == 'Z' {
				state, start = s, st
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d did not reach zombie state within 5s", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}

	identity := ProcessIdentity{PID: uint32(pid), StartTime: start}
	alive, err := ProcessAlive(identity)
	if err != nil || alive {
		t.Fatalf("ProcessAlive(zombie %s, state %c) = (%v, %v), want (false, nil)", identity, state, alive, err)
	}
}
