package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/atomicfile"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// cloneConfigName stores a workspace's optional custom working-copy location.
// It is separate from config.json so selecting a repository keeps its existing
// contract, and an old state directory keeps using the default clone/ path.
const cloneConfigName = "clone.json"

// ClonePathError reports a working-copy location that cannot be used. Message
// is controlled text authored by TOUKO and never contains credentials.
type ClonePathError struct{ Message string }

func (e *ClonePathError) Error() string { return e.Message }

func invalidClonePath(format string, args ...any) error {
	return &ClonePathError{Message: fmt.Sprintf(format, args...)}
}

type cloneConfig struct {
	Path string `json:"path"`
}

// CloneDirIn resolves a state directory's GitHub working copy. A missing or
// empty configuration means the historical <state>/clone directory; a stored
// path must be absolute so a moved checkout never depends on the caller's cwd.
func CloneDirIn(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, cloneConfigName))
	if errors.Is(err, os.ErrNotExist) {
		return repository.CloneDir(stateDir), nil
	}
	if err != nil {
		return "", err
	}
	var config cloneConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("clone configuration: %w", err)
	}
	path := strings.TrimSpace(config.Path)
	if path == "" {
		return repository.CloneDir(stateDir), nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("clone configuration: path must be absolute")
	}
	return filepath.Clean(path), nil
}

// CloneDir is the absolute directory holding this workspace's GitHub working
// copy. Local repositories are read from their own path and never use it.
func (s *Service) CloneDir() (string, error) { return CloneDirIn(s.Dir) }

// CloneMoveResult describes a completed migration. DefaultPath is always the
// implicit location, so the interface can offer returning to it.
type CloneMoveResult struct {
	Path        string `json:"path"`
	DefaultPath string `json:"default_path"`
	Custom      bool   `json:"custom"`
	Moved       bool   `json:"moved"`
	// Warning is set when the new location is complete and authoritative but
	// the old copy could not be deleted. The interface reports it verbatim.
	Warning string `json:"warning,omitempty"`
}

// MoveClone relocates this workspace's GitHub working copy and records the new
// location. An empty destination restores the default <state>/clone. The whole
// operation runs under the workspace lock, so a concurrent CLI sync cannot
// fetch into one path while this process moves it.
//
// Data is moved before the configuration is committed, and a failed move or a
// failed configuration write leaves the previous location authoritative. When
// the destination is on another filesystem the tree is copied, the copy is
// removed on failure, and the source survives until the copy is complete.
func (s *Service) MoveClone(ctx context.Context, destination string) (CloneMoveResult, error) {
	if err := ctx.Err(); err != nil {
		return CloneMoveResult{}, err
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return CloneMoveResult{}, err
	}
	defer unlock()
	location, err := s.Location(ctx)
	if err != nil {
		return CloneMoveResult{}, err
	}
	if location.Backend != "github" {
		return CloneMoveResult{}, invalidClonePath("本地仓库直接读取原路径，不能迁移读图副本。")
	}
	current, err := s.CloneDir()
	if err != nil {
		return CloneMoveResult{}, err
	}
	defaultPath := filepath.Clean(repository.CloneDir(s.Dir))
	target := strings.TrimSpace(destination)
	if target == "" {
		target = defaultPath
	}
	if !filepath.IsAbs(target) {
		return CloneMoveResult{}, invalidClonePath("目标位置必须是绝对路径。")
	}
	target = filepath.Clean(target)
	result := CloneMoveResult{Path: target, DefaultPath: defaultPath, Custom: target != defaultPath}

	if target == current {
		// Nothing to move; still persist a reset that removes a stale custom
		// configuration, and an explicit choice of the current custom path.
		if err := writeCloneConfig(ctx, s.Dir, target, defaultPath); err != nil {
			return CloneMoveResult{}, err
		}
		return result, nil
	}
	if pathWithin(current, target) || pathWithin(target, current) {
		return CloneMoveResult{}, invalidClonePath("目标位置不能与当前副本互相包含。")
	}

	sourceInfo, sourceErr := os.Lstat(current)
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return CloneMoveResult{}, sourceErr
	}
	hasSource := sourceErr == nil
	if !hasSource {
		// A workspace can be configured before its first clone exists. The new
		// location becomes authoritative for the clone that sync will create.
		if _, _, err := validateCloneDestination(target); err != nil {
			return CloneMoveResult{}, err
		}
		if err := writeCloneConfig(ctx, s.Dir, target, defaultPath); err != nil {
			return CloneMoveResult{}, err
		}
		return result, nil
	}

	// Only a directory can be a working copy, and only an absent or empty
	// directory can receive it. An empty destination is removed first so the
	// rename works on platforms that refuse to replace a directory.
	if !sourceInfo.IsDir() {
		return CloneMoveResult{}, invalidClonePath("当前副本不是目录，无法迁移。")
	}
	mode, empty, err := validateCloneDestination(target)
	if err != nil {
		return CloneMoveResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return CloneMoveResult{}, fmt.Errorf("create clone destination parent: %w", err)
	}
	if empty {
		if err := os.Remove(target); err != nil {
			return CloneMoveResult{}, err
		}
	}
	copied, err := relocateCloneTree(ctx, current, target)
	if err != nil {
		if empty && mode != nil {
			_ = os.Mkdir(target, *mode)
		}
		return CloneMoveResult{}, fmt.Errorf("move clone: %w", err)
	}
	result.Moved = true
	if err := writeCloneConfig(ctx, s.Dir, target, defaultPath); err != nil {
		// Until the new location is recorded, the source stays authoritative and
		// complete: a rename is undone, a cross-device copy is deleted.
		if copied {
			if rollbackErr := os.RemoveAll(target); rollbackErr != nil {
				return CloneMoveResult{}, fmt.Errorf("save clone location: %w (removing the copy at %s also failed: %v)", err, target, rollbackErr)
			}
		} else if rollbackErr := os.Rename(target, current); rollbackErr != nil {
			return CloneMoveResult{}, fmt.Errorf("save clone location: %w (rollback to %s failed: %v)", err, current, rollbackErr)
		}
		return CloneMoveResult{}, fmt.Errorf("save clone location: %w", err)
	}
	if copied {
		// The destination is complete and authoritative. A source that cannot be
		// deleted is reported, never "rolled back" by deleting the good copy.
		if err := os.RemoveAll(current); err != nil {
			result.Warning = fmt.Sprintf("新位置已生效，但旧位置 %s 未能完全删除：%v", current, err)
		}
	}
	return result, nil
}

// validateCloneDestination accepts an absent or empty directory and returns
// that directory's mode plus whether it exists. A non-empty destination is
// refused so a migration can never merge two working copies or overwrite files
// the user keeps there.
func validateCloneDestination(target string) (*fs.FileMode, bool, error) {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, invalidClonePath("目标位置已存在且不是目录：%s", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, false, err
	}
	if len(entries) > 0 {
		return nil, false, invalidClonePath("目标位置已存在且不为空：%s", target)
	}
	mode := info.Mode().Perm()
	return &mode, true, nil
}

// writeCloneConfig persists target atomically, or removes the file when the
// default location is in use so old state keeps its historical behavior.
func writeCloneConfig(ctx context.Context, stateDir, target, defaultPath string) error {
	path := filepath.Join(stateDir, cloneConfigName)
	if filepath.Clean(target) == filepath.Clean(defaultPath) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(cloneConfig{Path: target})
	if err != nil {
		return err
	}
	return atomicfile.Write(ctx, path, append(data, '\n'), 0o600)
}

// relocateCloneTree renames a working copy, copying it when the destination is
// on another filesystem. The second result reports a copy, in which case the
// source is deliberately left intact for the caller to delete only after the
// new location is recorded. An incomplete copy is always removed.
func relocateCloneTree(ctx context.Context, from, to string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	err := os.Rename(from, to)
	if err == nil {
		return false, nil
	}
	if !sameDeviceError(err) {
		return false, err
	}
	if err := copyCloneTree(ctx, from, to); err != nil {
		_ = os.RemoveAll(to)
		return false, err
	}
	return true, nil
}

// copyCloneTree preserves directories, regular files, symlinks, and their
// permission bits. Nothing is followed, so a crafted link cannot make the move
// read or overwrite a file outside the working copy.
func copyCloneTree(ctx context.Context, from, to string) error {
	root, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !root.IsDir() {
		return fmt.Errorf("%s is not a directory", from)
	}
	if err := os.Mkdir(to, root.Mode().Perm()); err != nil {
		return err
	}
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == from {
			return nil
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Mkdir(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyCloneFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("unsupported file in clone: %s", path)
		}
	})
}

func copyCloneFile(from, to string, perm fs.FileMode) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	// Git object files are read-only; create writable and chmod afterwards.
	destination, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, source)
	if copyErr == nil {
		copyErr = destination.Chmod(perm)
	}
	closeErr := destination.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// pathWithin reports whether child is parent itself or below it.
func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
