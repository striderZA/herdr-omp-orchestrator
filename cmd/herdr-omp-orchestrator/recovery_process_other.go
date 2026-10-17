//go:build !windows && !linux

package main

import (
	"fmt"
	"os/exec"
)

func configureDetachedRecoveryChild(*exec.Cmd) error {
	return fmt.Errorf("detached recovery workers are supported only on Windows and Linux")
}
