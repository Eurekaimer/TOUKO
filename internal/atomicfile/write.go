// Package atomicfile replaces small durable state files without ever exposing a
// half-written file to a concurrent reader.
package atomicfile

import (
	"context"
	"os"
	"path/filepath"
)

// Write atomically replaces path with data. The temporary file is created in
// the same directory so the final rename never crosses a filesystem, receives
// perm before any bytes are written, and is synced and closed before the
// replacement. The destination directory itself is not synced, matching the
// existing config.json and workspace.json writers.
func Write(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(perm); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
