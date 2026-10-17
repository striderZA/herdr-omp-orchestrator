//go:build !windows && !linux

package state

import "errors"

var errUnsupportedProcessIdentity = errors.New("process identity is not supported on this platform")

// CurrentProcessIdentity refuses on platforms without a process start-time source.
// Linux support is x86_64 only; other hosts have no verified PID identity.
func CurrentProcessIdentity() (ProcessIdentity, error) {
	return ProcessIdentity{}, errUnsupportedProcessIdentity
}

// ProcessAlive refuses on platforms without a process start-time source.
func ProcessAlive(identity ProcessIdentity) (bool, error) {
	return false, errUnsupportedProcessIdentity
}
