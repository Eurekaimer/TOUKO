package index

import (
	"os"

	"golang.org/x/sys/windows"
)

// Windows cannot fsync an os.Open directory handle. Ask the rename itself
// to complete with write-through instead of ignoring a directory-sync error.
func durableRename(source, target string) (bool, error) {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return false, &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
	}
	to, err := windows.UTF16PtrFromString(target)
	if err == nil {
		err = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	}
	if err != nil {
		return false, &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
	}
	return true, nil
}
