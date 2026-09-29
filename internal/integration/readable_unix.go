//go:build unix

package integration

import "syscall"

// r_OK is the access(2) read-permission flag (POSIX value 4; Go's syscall
// package does not export R_OK on linux).
const r_OK = 0x4

// socketReadable mirrors node's fs.access(path, R_OK): it checks read
// permission on the socket path without opening it (open(2) on a socket
// path fails with ENXIO, so os.Open cannot be used for this check).
func socketReadable(path string) error {
	return syscall.Access(path, r_OK)
}
