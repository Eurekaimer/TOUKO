package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// legacyGallery turns the current repository into one written by an older
// client: objects live at content-addressed paths and metadata has no path.
func legacyGallery(t *testing.T, ctx context.Context, r repository.Git, s *Service) {
	t.Helper()
	views, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		asset := view.Asset
		oldPath := repository.ObjectPath(asset)
		entry, err := findEntry(ctx, r, oldPath)
		if err != nil {
			t.Fatal(err)
		}
		legacyPath := repository.LegacyObjectPath(asset.Namespace, asset.Object)
		blob := repository.BlobID(entry.SHA)
		asset.Path = ""
		data, err := repository.EncodeAsset(asset)
		if err != nil {
			t.Fatal(err)
		}
		metaBlob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		publishRaw(t, ctx, r, []repository.TreeChange{
			{Path: legacyPath, Blob: &blob},
			{Path: repository.AssetPath(asset.ID), Blob: &metaBlob},
			{Path: oldPath},
		})
	}
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func findEntry(ctx context.Context, r repository.Git, path string) (repository.GitEntry, error) {
	head, err := r.Head(ctx)
	if err != nil {
		return repository.GitEntry{}, err
	}
	commit, err := r.Commit(ctx, head)
	if err != nil {
		return repository.GitEntry{}, err
	}
	entries, err := r.Tree(ctx, commit.Tree)
	if err != nil {
		return repository.GitEntry{}, err
	}
	entry, ok := entryAt(entries, path)
	if !ok {
		return repository.GitEntry{}, errors.New("object entry missing: " + path)
	}
	return entry, nil
}

func TestMigratePathsRewritesAndKeepsOldObjects(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{
		{Reader: bytes.NewReader(batchPNG(t, 11)), Name: "one.png"},
		{Reader: bytes.NewReader(batchPNG(t, 12)), Name: "two.png"},
	}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	before := mustCommit(t, ctx, s, r).Head
	legacyGallery(t, ctx, r, s)
	legacyHead, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacySnap, err := repository.ReadSnapshot(ctx, r, legacyHead)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range legacySnap.Assets {
		if asset.Path != "" || repository.ObjectPath(asset) != repository.LegacyObjectPath(asset.Namespace, asset.Object) {
			t.Fatalf("fixture is not legacy: %#v", asset)
		}
	}
	if before == legacyHead {
		t.Fatal("fixture did not advance")
	}

	// A dry run reports the plan and changes nothing.
	dry, err := s.MigratePaths(ctx, r, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Migrated != 2 || dry.Failed != 0 || dry.Head != legacyHead {
		t.Fatalf("dry run = %#v", dry)
	}
	if head, _ := r.Head(ctx); head != legacyHead {
		t.Fatal("dry run moved the branch")
	}

	result, err := s.MigratePaths(ctx, r, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 2 || result.Failed != 0 || result.Head == legacyHead {
		t.Fatalf("migration = %#v", result)
	}
	snap, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, entry := range snap.Entries {
		paths[entry.Path] = true
	}
	for _, asset := range snap.Assets {
		want := repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
		if asset.Path != want {
			t.Fatalf("asset path = %q, want %q", asset.Path, want)
		}
		if !paths[want] {
			t.Fatalf("new object %s missing", want)
		}
		// Old URLs keep working because the legacy object stays in the tree.
		if !paths[repository.LegacyObjectPath(asset.Namespace, asset.Object)] {
			t.Fatalf("legacy object removed before verification: %#v", asset)
		}
	}
	if _, err := s.Verify(ctx, r); err != nil {
		t.Fatalf("migrated gallery does not verify: %v", err)
	}

	pruned, err := s.MigratePaths(ctx, r, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Pruned != 2 || pruned.Head == result.Head {
		t.Fatalf("prune = %#v", pruned)
	}
	snap, err = repository.ReadSnapshot(ctx, r, pruned.Head)
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, entry := range snap.Entries {
		remaining[entry.Path] = true
	}
	for _, asset := range snap.Assets {
		if !remaining[repository.AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)] {
			t.Fatalf("prune removed a live object: %#v", asset)
		}
	}
}

func TestMigratePathsFailureKeepsOldLayout(t *testing.T) {
	ctx, r, s := realRepository(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 21)), Name: "one.png"}}, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	legacyGallery(t, ctx, r, s)
	head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigratePaths(ctx, &publicationFault{Git: r, fail: "blob"}, false, false); err == nil {
		t.Fatal("migration ignored a blob failure")
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("failed migration moved the branch")
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range snap.Assets {
		if asset.Path != "" {
			t.Fatalf("failed migration rewrote metadata: %#v", asset)
		}
	}
}

func TestWriteURLMapping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migration")
	items := []MigrateItem{
		{AssetID: "a", OldPath: "objects/x/sha256/aa/bb/h.png", NewPath: "objects/x/a.png", OldGitHub: "u1", NewGitHub: "u2", OldR2: "r1", NewR2: "r2", SizeBytes: 12, Status: "migrated"},
		{AssetID: "b", OldPath: "objects/x/b.png", NewPath: "objects/x/b.png", Status: "unchanged"},
	}
	csvPath, jsonPath, err := WriteURLMapping(dir, items)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("csv rows = %d: %v", len(rows), err)
	}
	if rows[0][0] != "asset_id" || rows[1][8] != "migrated" || rows[1][7] != "12" || rows[2][8] != "unchanged" {
		t.Fatalf("csv content: %#v", rows)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil || !bytes.Contains(raw, []byte(`"migrated"`)) {
		t.Fatalf("json mapping: %v %s", err, raw)
	}
}
