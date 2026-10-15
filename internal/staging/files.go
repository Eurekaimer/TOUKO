package staging

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/image"
)

func PayloadPath(stateDir string, ns domain.Namespace, object domain.ContentObject) string {
	path := filepath.Join(stateDir, "stage", "objects", string(ns), object.Hash.String()+"."+object.Ext)
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

// Install consumes result.TempPath even on failure. The caller must hold the
// state directory's operation lock, so the destination check and rename cannot
// race another staging mutation. Existing payloads are verified, never replaced.
func Install(ctx context.Context, stateDir string, ns domain.Namespace, result image.Result) (string, bool, error) {
	path := PayloadPath(stateDir, ns, result.Object)
	source, err := filepath.Abs(result.TempPath)
	if err != nil {
		return "", false, err
	}
	if source == path {
		return "", false, fmt.Errorf("ingest temporary is already the destination")
	}
	defer os.Remove(result.TempPath)
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	parsed, err := domain.ParseNamespace(string(ns))
	if err != nil || parsed != ns {
		return "", false, fmt.Errorf("invalid staging namespace %q", ns)
	}
	if err := result.Object.Validate(); err != nil {
		return "", false, err
	}
	if err := verifyFile(ctx, result.TempPath, result.Object, true); err != nil {
		return "", false, err
	}
	for _, dir := range []string{stateDir, filepath.Join(stateDir, "stage"), filepath.Join(stateDir, "stage", "objects"), filepath.Dir(path)} {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", false, fmt.Errorf("create staging directory: %w", err)
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", false, err
		}
		if !info.IsDir() {
			return "", false, fmt.Errorf("staging directory is not a directory: %s", dir)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return "", false, fmt.Errorf("secure staging directory: %w", err)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		if err := verifyFile(ctx, path, result.Object, false); err != nil {
			return "", false, err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return "", false, fmt.Errorf("secure existing payload: %w", err)
		}
		return path, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("inspect staged payload: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if err := os.Rename(result.TempPath, path); err != nil {
		return "", false, fmt.Errorf("install staged payload: %w", err)
	}
	return path, true, nil
}

func verifyFile(ctx context.Context, path string, object domain.ContentObject, syncFile bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect payload: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != object.Size {
		return fmt.Errorf("%w: staged payload size or file type differs: %s", domain.ErrHashMismatch, path)
	}
	flags := os.O_RDONLY
	if syncFile {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return fmt.Errorf("open payload: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(contextReader{ctx, file}, object.Size+1))
	if err != nil {
		return fmt.Errorf("verify staged payload: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var actual domain.ObjectHash
	hash.Sum(actual[:0])
	if size != object.Size || actual != object.Hash {
		return fmt.Errorf("%w: staged payload bytes differ: %s", domain.ErrHashMismatch, path)
	}
	if syncFile {
		if err := file.Chmod(0600); err != nil {
			return fmt.Errorf("secure payload: %w", err)
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync payload: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close payload: %w", err)
	}
	return nil
}

// Cleanup only visits stage/objects; it never touches the operation lock,
// config, database, or ingest temporaries. References are absolute payload paths.
func Cleanup(ctx context.Context, stateDir string, referenced map[string]bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := filepath.Abs(filepath.Join(stateDir, "stage", "objects"))
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == root && errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() || referenced[path] {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove unreferenced staged payload: %w", err)
		}
		return nil
	})
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}
