//go:build windows

package app

import (
	"errors"

	"golang.org/x/sys/windows"
)

// sameDeviceError reports a rename that failed only because the source and
// destination are on different volumes, where a copy is required.
func sameDeviceError(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }
