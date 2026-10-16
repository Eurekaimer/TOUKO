package localgit_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/localgit"
)

func smokeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@localhost",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@localhost",
		"GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestCloneRefusesForeignDirectoryAndUpdatesExactTree(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	remote := filepath.Join(base, "gallery.git")
	if _, err := localgit.Init(ctx, remote, "main"); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(base, "seed")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	smokeGit(t, seed, "init", "-q", "-b", "main")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.png", "aaa")
	write("b.png", "bbb")
	smokeGit(t, seed, "add", "-A")
	smokeGit(t, seed, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "one")
	smokeGit(t, seed, "push", "-q", remote, "main")

	clone := filepath.Join(base, "clone")
	if err := localgit.Clone(ctx, remote, clone, "main", ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(clone, "b.png"))
	if err != nil || string(data) != "bbb" {
		t.Fatalf("pictures must be plain files: %q %v", data, err)
	}
	// Reuse rather than fail or duplicate.
	if err := localgit.Clone(ctx, remote, clone, "main", ""); err != nil {
		t.Fatalf("re-entrant clone: %v", err)
	}
	write("c.png", "ccc")
	if err := os.Remove(filepath.Join(seed, "a.png")); err != nil {
		t.Fatal(err)
	}
	smokeGit(t, seed, "add", "-A")
	smokeGit(t, seed, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "two")
	smokeGit(t, seed, "push", "-q", remote, "main")
	if err := localgit.Update(ctx, clone, "main", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clone, "a.png")); !os.IsNotExist(err) {
		t.Fatalf("remote deletion must leave the working copy, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "c.png")); err != nil {
		t.Fatalf("remote addition must appear: %v", err)
	}
	// A directory that is not this repository must be refused untouched.
	other := filepath.Join(base, "mine")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(other, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := localgit.Clone(ctx, remote, other, "main", ""); err == nil {
		t.Fatal("clone overwrote a directory it did not create")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("user file was removed: %v", err)
	}
}

func TestCloneRejectsUnsafeInputs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, remote := range []string{"", "-x", "https://user:pass@github.com/o/r.git", "https://tok@github.com/o/r.git"} {
		if err := localgit.Clone(ctx, remote, filepath.Join(dir, "c"), "main", ""); err == nil {
			t.Errorf("remote %q was accepted", remote)
		}
	}
	for _, branch := range []string{"", "-x", "a b", "a\nb"} {
		if err := localgit.Clone(ctx, "https://github.com/o/r.git", filepath.Join(dir, "d"), branch, ""); err == nil {
			t.Errorf("branch %q was accepted", branch)
		}
	}
}
