//go:build !windows

package index

import (
	"os"
	"path/filepath"
)

// durableRename reports whether the move happened even when syncing fails,
// so cache recovery can restore every file that was actually moved.
func durableRename(source, target string) (bool, error) {
	if err := os.Rename(source, target); err != nil {
		return false, err
	}
	f, err := os.Open(filepath.Dir(target))
	if err != nil {
		return true, err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return true, err
	}
	return true, closeErr
}
