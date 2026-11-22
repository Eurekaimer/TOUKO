package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func cloneTestService(t *testing.T) (context.Context, *Service) {
	t.Helper()
	ctx := t.Context()
	service, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	if err := service.Select(ctx, repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	return ctx, service
}

// writeCloneFixture creates a checkout whose .git contents and worktree bytes
// must survive a move unchanged.
func writeCloneFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects", "ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "one.png"), []byte("image-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Git object files are read-only; the move must preserve that bit.
	object := filepath.Join(dir, ".git", "objects", "ab", "cdef")
	if err := os.WriteFile(object, []byte("object-bytes"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(object, 0o444); err != nil {
		t.Fatal(err)
	}
}

func TestMoveClonePreservesCheckoutAndPersistsLocation(t *testing.T) {
	ctx, service := cloneTestService(t)
	source, err := service.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	writeCloneFixture(t, source)
	destination := filepath.Join(t.TempDir(), "moved", "clone")

	result, err := service.MoveClone(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != destination || !result.Custom || !result.Moved || result.DefaultPath != repository.CloneDir(service.Dir) {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old checkout still present: %v", err)
	}
	head, err := os.ReadFile(filepath.Join(destination, ".git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != "ref: refs/heads/main\n" {
		t.Fatalf("git metadata changed: %q", head)
	}
	image, err := os.ReadFile(filepath.Join(destination, "images", "one.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(image) != "image-bytes" {
		t.Fatalf("worktree bytes changed: %q", image)
	}
	info, err := os.Stat(filepath.Join(destination, ".git", "objects", "ab", "cdef"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("object mode changed: %v", info.Mode())
	}
	got, err := service.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("CloneDir = %s, want %s", got, destination)
	}
	// A later process must see the same location, not fall back to clone/.
	reopened, err := Open(ctx, service.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = reopened.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("reopened CloneDir = %s, want %s", got, destination)
	}
}

func TestMoveCloneRestoresDefaultLocation(t *testing.T) {
	ctx, service := cloneTestService(t)
	defaultPath, err := service.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	writeCloneFixture(t, defaultPath)
	custom := filepath.Join(t.TempDir(), "custom", "clone")
	if _, err := service.MoveClone(ctx, custom); err != nil {
		t.Fatal(err)
	}

	result, err := service.MoveClone(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != defaultPath || result.Custom || !result.Moved {
		t.Fatalf("unexpected reset result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(defaultPath, "images", "one.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(custom); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom checkout still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.Dir, cloneConfigName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default location should not keep clone.json: %v", err)
	}
}

func TestMoveCloneRefusesBadDestinationsAndKeepsSource(t *testing.T) {
	ctx, service := cloneTestService(t)
	source, err := service.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	writeCloneFixture(t, source)
	defaultConfig, err := os.ReadFile(filepath.Join(service.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
	}{
		{"relative", "relative/clone"},
		{"non_empty", occupied},
		{"nested_in_source", filepath.Join(source, "nested")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.MoveClone(ctx, test.path)
			var invalid *ClonePathError
			if !errors.As(err, &invalid) {
				t.Fatalf("expected ClonePathError, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(source, "images", "one.png")); err != nil {
				t.Fatalf("source damaged: %v", err)
			}
			got, err := service.CloneDir()
			if err != nil {
				t.Fatal(err)
			}
			if got != source {
				t.Fatalf("location changed after refusal: %s", got)
			}
			currentConfig, err := os.ReadFile(filepath.Join(service.Dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(currentConfig) != string(defaultConfig) {
				t.Fatal("repository configuration changed")
			}
			if data, err := os.ReadFile(filepath.Join(occupied, "keep.txt")); err == nil && string(data) != "keep" {
				t.Fatal("occupied destination was modified")
			}
		})
	}
}

func TestMoveCloneFailureKeepsSourceAndConfiguration(t *testing.T) {
	ctx, service := cloneTestService(t)
	source, err := service.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	writeCloneFixture(t, source)
	// The destination's parent is a regular file, so neither the rename nor the
	// copy fallback can create the destination.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MoveClone(ctx, filepath.Join(blocker, "clone")); err == nil {
		t.Fatal("move into a file path succeeded")
	}
	if _, err := os.Stat(filepath.Join(source, ".git", "HEAD")); err != nil {
		t.Fatalf("source lost after failed move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "images", "one.png")); err != nil {
		t.Fatalf("worktree lost after failed move: %v", err)
	}
	got, err := service.CloneDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != source {
		t.Fatalf("location changed after failed move: %s", got)
	}
	if _, err := os.Stat(filepath.Join(service.Dir, cloneConfigName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed move wrote clone.json: %v", err)
	}
}

func TestMoveCloneRefusesLocalRepository(t *testing.T) {
	ctx := t.Context()
	local := t.TempDir()
	service, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Select(ctx, repository.Location{Backend: "localgit", Branch: "main", Path: local}); err != nil {
		t.Fatal(err)
	}
	_, err = service.MoveClone(ctx, filepath.Join(t.TempDir(), "clone"))
	var invalid *ClonePathError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected ClonePathError, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.Dir, cloneConfigName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local repository gained clone.json: %v", err)
	}
}

func TestCloneDirRejectsRelativeConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, cloneConfigName), []byte(`{"path":"relative/clone"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CloneDirIn(dir); err == nil {
		t.Fatal("relative configured path accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, cloneConfigName), []byte(`{"path":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := CloneDirIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != repository.CloneDir(dir) {
		t.Fatalf("empty configuration = %s, want default", got)
	}
}

func TestCopyCloneTreePreservesContentsAndModes(t *testing.T) {
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "file.txt"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "sub", "file.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	hasLink := true
	if err := os.Symlink("sub/file.txt", filepath.Join(source, "link.txt")); err != nil {
		hasLink = false // Windows without symlink privileges.
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := copyCloneTree(ctx, source, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "sub", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("copy changed bytes: %q", data)
	}
	info, err := os.Stat(filepath.Join(target, "sub", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("copy changed mode: %v", info.Mode())
	}
	if hasLink {
		link, err := os.Readlink(filepath.Join(target, "link.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if link != "sub/file.txt" {
			t.Fatalf("copy changed symlink: %s", link)
		}
	}
}
