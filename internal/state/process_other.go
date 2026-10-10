//go:build !windows

package state

import (
	"fmt"
	"os"
	"syscall"
)

// CurrentProcessIdentity returns a best-effort PID identity on unsupported
// development hosts. The shipped plugin targets Windows, where StartTime is
// required to distinguish PID reuse.
func CurrentProcessIdentity() (ProcessIdentity, error) {
	return ProcessIdentity{PID: uint32(os.Getpid()), StartTime: 1}, nil
}

// ProcessAlive uses signal 0 on development hosts. It cannot verify process
// creation time, so it is not the production Windows liveness implementation.
func ProcessAlive(identity ProcessIdentity) (bool, error) {
	if identity.PID == 0 {
		return false, fmt.Errorf("invalid process identity %s", identity)
	}
	process, err := os.FindProcess(int(identity.PID))
	if err != nil {
		return false, nil
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		if err == os.ErrProcessDone {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
