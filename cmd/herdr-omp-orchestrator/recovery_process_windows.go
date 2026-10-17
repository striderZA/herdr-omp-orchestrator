//go:build windows

package main

import (
	"io"
	"os/exec"

	"golang.org/x/sys/windows"
)

func configureDetachedRecoveryChild(cmd *exec.Cmd) error {
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS,
	}
	return nil
}
