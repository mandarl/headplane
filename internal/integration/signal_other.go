//go:build !unix

package integration

import "errors"

// sendSIGHUP is unsupported off unix. The /proc-based integrations are
// Linux-only anyway, so this path is unreachable in practice.
func sendSIGHUP(pid int) error {
	return errors.New("SIGHUP is not supported on this platform")
}
