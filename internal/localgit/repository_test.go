package localgit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func gitCommand(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@localhost", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@localhost")
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestInitAndOpenRejectUnsafeTargets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gallery.git")
	r, err := Init(ctx, path, "main")
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head(ctx)
	if err != nil || head != "" {
		t.Fatalf("empty branch: %q %v", head, err)
	}
	if got := gitCommand(t, path, nil, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatal(got)
	}
	if _, err := Init(ctx, path, "main"); err == nil {
		t.Fatal("overwrote initialized directory")
	}
	for _, branch := range []string{"../bad", "HEAD", "-main", "@{-1}"} {
		if _, err := Open(ctx, path, branch); err == nil {
			t.Fatalf("accepted unsafe branch %q", branch)
		}
	}
	nonempty := t.TempDir()
	marker := filepath.Join(nonempty, "keep")
	if err := os.WriteFile(marker, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(ctx, nonempty, "main"); err == nil {
		t.Fatal("accepted nonempty directory")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatalf("marker: %q %v", data, err)
	}
	empty := t.TempDir()
	if _, err := Init(ctx, empty, "main"); err != nil {
		t.Fatal(err)
	}
	sha256 := filepath.Join(t.TempDir(), "sha256.git")
	gitCommand(t, t.TempDir(), nil, "init", "--bare", "--object-format=sha256", sha256)
	if _, err := Open(ctx, sha256, "main"); err == nil {
		t.Fatal("accepted SHA-256 repository")
	}
}

func TestRootPublicationCASAndAncestry(t *testing.T) {
	ctx := context.Background()
	r, err := Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.CreateBlob(ctx, strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.CreateTree(ctx, "", []repository.TreeChange{{Path: "README", Blob: &blob}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.CreateCommit(ctx, tree, "", "root")
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := r.Head(ctx); head != "" {
		t.Fatal("detached commit published")
	}
	if err := r.UpdateRef(ctx, "", root); err != nil {
		t.Fatal(err)
	}
	second, err := r.CreateCommit(ctx, tree, root, "second")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := r.CreateCommit(ctx, tree, root, "sibling")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, root, second); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, root, sibling); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	if head, err := r.Head(ctx); err != nil || head != second {
		t.Fatalf("head %q %v", head, err)
	}
	for _, tc := range []struct {
		a, b domain.RepositoryHead
		want bool
	}{{root, second, true}, {second, root, false}, {sibling, second, false}, {root, root, true}} {
		got, err := r.IsAncestor(ctx, tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Fatalf("ancestry %s %s: %v %v", tc.a, tc.b, got, err)
		}
	}
	commit, err := r.Commit(ctx, second)
	if err != nil || commit.Tree != tree || len(commit.Parents) != 1 || commit.Parents[0] != root {
		t.Fatalf("commit: %+v %v", commit, err)
	}
	if author := gitCommand(t, r.path, nil, "show", "-s", "--format=%an <%ae>|%cn <%ce>", string(root)); author != "TOUKO <touko@localhost>|TOUKO <touko@localhost>" {
		t.Fatal(author)
	}
	if _, err := r.Commit(ctx, "--help"); err == nil {
		t.Fatal("accepted non-object ID")
	}
}

func TestBlobStreamingAndEarlyClose(t *testing.T) {
	ctx := context.Background()
	r, err := Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0, 1, 255, '\n', 17}, 1<<20)
	blob, err := r.CreateBlob(ctx, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := r.OpenBlob(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, reader); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, got.Bytes()) {
		t.Fatal("blob bytes differ")
	}
	reader, err = r.OpenBlob(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 9)); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateBlob(ctx, strings.NewReader("long"), 3); err == nil {
		t.Fatal("accepted mismatched size")
	}
	if _, err := r.CreateBlob(ctx, strings.NewReader("short"), 6); err == nil {
		t.Fatal("accepted short blob")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.CreateBlob(canceled, bytes.NewReader(payload), int64(len(payload))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestCreateTreePreservesModesAndRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	r, err := Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	text := gitCommand(t, r.path, []byte("original"), "hash-object", "-w", "--stdin")
	link := gitCommand(t, r.path, []byte("README"), "hash-object", "-w", "--stdin")
	emptyTree := gitCommand(t, r.path, nil, "mktree")
	root := gitCommand(t, r.path, []byte("fixture"), "commit-tree", emptyTree)
	subtree := gitCommand(t, r.path, []byte("100644 blob "+text+"\told\x00"), "mktree", "-z")
	base := gitCommand(t, r.path, []byte("100644 blob "+text+"\tREADME\x00100755 blob "+text+"\texecutable\x00120000 blob "+link+"\tlink\x00160000 commit "+root+"\tsubmodule\x00040000 tree "+subtree+"\tdir\x00"), "mktree", "-z")
	blob, err := r.CreateBlob(ctx, strings.NewReader("new"), 3)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.CreateTree(ctx, repository.TreeID(base), []repository.TreeChange{{Path: "dir/new", Blob: &blob}, {Path: "dir/old"}, {Path: "nested/deep/file", Blob: &blob}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.Tree(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]repository.GitEntry)
	for _, entry := range entries {
		paths[entry.Path] = entry
	}
	for path, mode := range map[string]string{"README": "100644", "executable": "100755", "link": "120000", "submodule": "160000", "dir": "040000", "dir/new": "100644", "nested/deep/file": "100644"} {
		if entry, ok := paths[path]; !ok || entry.Mode != mode {
			t.Fatalf("missing/changed %s: %+v", path, entry)
		}
	}
	if paths["README"].SHA != text || paths["executable"].SHA != text || paths["link"].SHA != link || paths["submodule"].SHA != root {
		t.Fatal("changed preserved objects")
	}
	if _, ok := paths["dir/old"]; ok {
		t.Fatal("deleted entry remains")
	}
	for _, path := range []string{"../escape", "/absolute", "a//b", "a/./b", "README/child", "link/child", "submodule/child", ".git/config"} {
		if _, err := r.CreateTree(ctx, repository.TreeID(base), []repository.TreeChange{{Path: path, Blob: &blob}}); err == nil {
			t.Fatalf("accepted unsafe path %q", path)
		}
	}
	if _, err := r.CreateTree(ctx, repository.TreeID(base), []repository.TreeChange{{Path: "dir", Blob: &blob}}); err == nil {
		t.Fatal("overwrote directory with blob")
	}
	if _, err := r.CreateTree(ctx, repository.TreeID(base), []repository.TreeChange{{Path: "a", Blob: &blob}, {Path: "a-else", Blob: &blob}, {Path: "a/b", Blob: &blob}}); err == nil {
		t.Fatal("accepted overlapping paths")
	}
}
