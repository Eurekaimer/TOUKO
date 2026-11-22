//go:build !windows

package app

import (
	"errors"
	"syscall"
)

// sameDeviceError reports a rename that failed only because the source and
// destination are on different filesystems, where a copy is required.
func sameDeviceError(err error) bool { return errors.Is(err, syscall.EXDEV) }
