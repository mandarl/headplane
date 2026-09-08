//go:build !unix

package integration

import "os"

// socketReadable fallback for non-unix platforms: existence check only.
func socketReadable(path string) error {
	_, err := os.Stat(path)
	return err
}
