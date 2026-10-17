//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// configureDetachedRecoveryChild starts the worker in its own session, so the
// foreground process's exit does not signal it. Stdio stays nil: os/exec opens
// /dev/null for it, so a later write from the worker never reaches a closed pipe.
func configureDetachedRecoveryChild(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
