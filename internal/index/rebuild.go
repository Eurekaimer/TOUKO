package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

func physicalCorruption(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 255
	return code == 11 || code == 26 // SQLITE_CORRUPT / SQLITE_NOTADB
}

// Rebuild replaces only a physically corrupt, closed cache, preserving the
// original database and sidecars in an index-corrupt-* directory. Healthy
// SQLite files, including invalid committed metadata, are rebuilt in place.
// The caller must hold the workspace lock and validate the entire source first.
func Rebuild(ctx context.Context, dir string, state State) error {
	s, err := Open(ctx, dir)
	if err == nil {
		var check string
		checkErr := s.DB.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
		if checkErr == nil && check == "ok" {
			err := s.Save(ctx, state)
			return errors.Join(err, s.Close())
		}
		closeErr := s.Close()
		if closeErr != nil {
			return closeErr
		}
		if checkErr != nil && !physicalCorruption(checkErr) {
			return checkErr
		}
	} else if !physicalCorruption(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Construct and flush a complete replacement before touching the old file.
	tmp, err := os.MkdirTemp(dir, ".reindex-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	next, err := Open(ctx, tmp)
	if err != nil {
		return err
	}
	if err := next.Save(ctx, state); err != nil {
		return errors.Join(err, next.Close())
	}
	if _, err := next.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return errors.Join(err, next.Close())
	}
	if err := next.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(dir, "index-corrupt-*")
	if err != nil {
		return err
	}
	var moved []string
	restore := func(cause error) error {
		for i := len(moved) - 1; i >= 0; i-- {
			name := moved[i]
			_, err := durableRename(filepath.Join(backup, name), filepath.Join(dir, name))
			cause = errors.Join(cause, err)
		}
		return cause
	}
	for _, name := range []string{"index.db", "index.db-wal", "index.db-shm"} {
		didMove, err := durableRename(filepath.Join(dir, name), filepath.Join(backup, name))
		if didMove {
			moved = append(moved, name)
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return restore(fmt.Errorf("preserve corrupt index: %w", err))
		}
	}
	if _, err := durableRename(filepath.Join(tmp, "index.db"), filepath.Join(dir, "index.db")); err != nil {
		return restore(fmt.Errorf("replace corrupt index: %w", err))
	}
	return saveWorkspace(ctx, dir, state)
}
