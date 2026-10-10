//go:build windows

package main

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

func configureDetachedRecoveryChild(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.DETACHED_PROCESS,
	}
	return nil
}
