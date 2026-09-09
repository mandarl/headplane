//go:build unix

package integration

import "syscall"

// sendSIGHUP sends SIGHUP to pid.
func sendSIGHUP(pid int) error {
	return syscall.Kill(pid, syscall.SIGHUP)
}
