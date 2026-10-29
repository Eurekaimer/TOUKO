package localgit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func writeBlob(t *testing.T, r *Repository, text string) repository.BlobID {
	t.Helper()
	id, err := r.CreateBlob(t.Context(), bytes.NewReader([]byte(text)), int64(len(text)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func readBlob(t *testing.T, r repository.Reader, id repository.BlobID) string {
	t.Helper()
	body, err := r.OpenBlob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// NewLocalFirst must answer from the checkout only for objects it holds and
// leave every other object to the authoritative side.
func TestNewLocalFirstFallsBackPerObject(t *testing.T) {
	ctx := t.Context()
	local, err := Init(ctx, filepath.Join(t.TempDir(), "local.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	api, err := Init(ctx, filepath.Join(t.TempDir(), "api.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	onlyLocal := writeBlob(t, local, "local bytes")
	onlyAPI := writeBlob(t, api, "api bytes")
	reader := NewLocalFirst(api, local)
	if got := readBlob(t, reader, onlyLocal); got != "local bytes" {
		t.Fatalf("local object: %q", got)
	}
	if got := readBlob(t, reader, onlyAPI); got != "api bytes" {
		t.Fatalf("fallback object: %q", got)
	}
	// HEAD stays authoritative on the api side.
	head, err := api.CreateCommit(ctx, writeTree(t, api, "readme"), "", "root")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UpdateRef(ctx, "", head); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Head(ctx); err != nil || got != head {
		t.Fatalf("head: %s %v", got, err)
	}
	// Commits and trees follow the same per-object rule.
	localTree := writeTree(t, local, "local-tree")
	localHead, err := local.CreateCommit(ctx, localTree, "", "local commit")
	if err != nil {
		t.Fatal(err)
	}
	if commit, err := reader.Commit(ctx, localHead); err != nil || commit.Tree != localTree {
		t.Fatalf("local commit: %+v %v", commit, err)
	}
	if commit, err := reader.Commit(ctx, head); err != nil || commit.Tree == "" {
		t.Fatalf("api commit: %+v %v", commit, err)
	}
	if entries, err := reader.Tree(ctx, localTree); err != nil || len(entries) != 1 || entries[0].Path != "local-tree" {
		t.Fatalf("local tree: %+v %v", entries, err)
	}
	apiTree, err := api.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := reader.Tree(ctx, apiTree.Tree); err != nil || len(entries) != 1 || entries[0].Path != "readme" {
		t.Fatalf("api tree: %+v %v", entries, err)
	}
}

func writeTree(t *testing.T, r *Repository, name string) repository.TreeID {
	t.Helper()
	blob := writeBlob(t, r, name)
	tree, err := r.CreateTree(t.Context(), "", []repository.TreeChange{{Path: name, Blob: &blob}})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// A checkout left behind by an API commit must gain the indexed commit on
// demand without moving its own ref.
func TestIndexedObjectsFetchesIndexedCommit(t *testing.T) {
	ctx := t.Context()
	remote := filepath.Join(t.TempDir(), "remote.git")
	origin, err := Init(ctx, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	first, err := origin.CreateCommit(ctx, writeTree(t, origin, "a"), "", "first")
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.UpdateRef(ctx, "", first); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if err := Clone(ctx, remote, clone, "main", ""); err != nil {
		t.Fatal(err)
	}
	second, err := origin.CreateCommit(ctx, writeTree(t, origin, "b"), first, "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.UpdateRef(ctx, first, second); err != nil {
		t.Fatal(err)
	}
	local := IndexedObjects(ctx, clone, remote, "main", second, "")
	if local == nil || !local.HasObject(ctx, string(second)) {
		t.Fatal("indexed commit was not fetched")
	}
	if head, err := local.Head(ctx); err != nil || head != first {
		t.Fatalf("checkout moved: %s %v", head, err)
	}
	if IndexedObjects(ctx, filepath.Join(t.TempDir(), "absent"), remote, "main", second, "") != nil {
		t.Fatal("missing checkout was not reported")
	}
	if IndexedObjects(ctx, clone, remote, "main", "", "") == nil {
		t.Fatal("empty index head rejected the checkout")
	}
	if _, err := os.Stat(filepath.Join(clone, "a")); err != nil {
		t.Fatalf("checkout missing tracked file: %v", err)
	}
}
