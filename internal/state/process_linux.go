//go:build linux

package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// CurrentProcessIdentity returns this process's PID and its start time in clock
// ticks since boot, read from /proc/<pid>/stat.
func CurrentProcessIdentity() (ProcessIdentity, error) {
	pid := os.Getpid()
	_, start, err := readProcStat(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read current process start time: %w", err)
	}
	identity := ProcessIdentity{PID: uint32(pid), StartTime: start}
	if !identity.valid() {
		return ProcessIdentity{}, fmt.Errorf("%w: current process identity %s is unavailable", ErrInvalid, identity)
	}
	return identity, nil
}

// ProcessAlive reports whether identity still names a running process. A zombie
// (exited but not yet reaped) or a process whose start time differs is not alive.
// A read or parse failure other than a missing process is returned as an error so
// the caller keeps its claim.
func ProcessAlive(identity ProcessIdentity) (bool, error) {
	if !identity.valid() {
		return false, fmt.Errorf("invalid process identity %s", identity)
	}
	state, start, err := readProcStat(int(identity.PID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if state == 'Z' || state == 'X' {
		return false, nil
	}
	if start != identity.StartTime {
		return false, nil
	}
	return true, nil
}

func readProcStat(pid int) (byte, int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	return parseProcStat(string(data))
}

// parseProcStat reads the state and start time from a /proc/<pid>/stat line. The
// command name is parenthesized and may contain spaces or parentheses, so fields
// are counted from the last ')'.
func parseProcStat(data string) (state byte, start int64, err error) {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return 0, 0, errors.New("proc stat: missing command name")
	}
	fields := strings.Fields(data[end+1:])
	if len(fields) < 20 {
		return 0, 0, fmt.Errorf("proc stat: %d fields after command name, want at least 20", len(fields))
	}
	if len(fields[0]) == 0 {
		return 0, 0, errors.New("proc stat: empty state field")
	}
	start, err = strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("proc stat: start time: %w", err)
	}
	return fields[0][0], start, nil
}
