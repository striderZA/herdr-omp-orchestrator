//go:build windows

package state

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// CurrentProcessIdentity returns this process's PID and Windows creation time.
func CurrentProcessIdentity() (ProcessIdentity, error) {
	handle, err := windows.GetCurrentProcess()
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("get current process handle: %w", err)
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return ProcessIdentity{}, fmt.Errorf("read current process creation time: %w", err)
	}
	identity := ProcessIdentity{
		PID:       windows.GetCurrentProcessId(),
		StartTime: int64(uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)),
	}
	if !identity.valid() {
		return ProcessIdentity{}, fmt.Errorf("current process returned an invalid identity")
	}
	return identity, nil
}

// ProcessAlive returns true only when pid still names the process with the
// recorded creation time. Access-denied and query failures are errors so callers
// can keep the claim instead of mistaking an unknown owner for a dead one.
func ProcessAlive(identity ProcessIdentity) (bool, error) {
	if !identity.valid() {
		return false, fmt.Errorf("invalid process identity %s", identity)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, identity.PID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open process %s: %w", identity, err)
	}
	defer windows.CloseHandle(handle)

	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return false, fmt.Errorf("read process %s creation time: %w", identity, err)
	}
	start := int64(uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime))
	if start != identity.StartTime {
		return false, nil
	}
	// GetExitCodeProcess reports STILL_ACTIVE (259) for a process that exited with
	// that code too, so an exit code cannot tell a running process from a finished
	// one. The process handle is signaled once the process has terminated: a zero
	// timeout wait asks exactly that.
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, fmt.Errorf("read process %s state: %w", identity, err)
	}
	switch event {
	case uint32(windows.WAIT_OBJECT_0):
		return false, nil
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	default:
		return false, fmt.Errorf("read process %s state: unexpected wait result %#x", identity, event)
	}
}
