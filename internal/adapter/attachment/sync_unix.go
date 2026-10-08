//go:build !windows

package attachment

import (
	"errors"
	"os"
)

// syncDirectory makes the entries of one directory durable. A synced file
// does not survive a crash when its directory entry never reached storage.
func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // the path is the private store root or one of its derived directories
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
