package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func realRepository(t *testing.T) (context.Context, *localgit.Repository, *Service) {
	t.Helper()
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.Initialize(ctx, r); err != nil {
		t.Fatal(err)
	}
	return ctx, r, s
}
func publishRaw(t *testing.T, ctx context.Context, r repository.Git, changes []repository.TreeChange) domain.RepositoryHead {
	t.Helper()
	head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.CreateTree(ctx, commit.Tree, changes)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.CreateCommit(ctx, tree, head, "test publication")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.UpdateRef(ctx, head, next); err != nil {
		t.Fatal(err)
	}
	return next
}
func blob(t *testing.T, ctx context.Context, r repository.Git, data []byte) *repository.BlobID {
	t.Helper()
	id, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return &id
}
func TestFreshSyncRestoresCompleteMetadata(t *testing.T) {
	ctx, r, s := realRepository(t)
	data := pngBytes(t)
	id, _ := domain.NewAssetID()
	now := time.Date(2026, 2, 3, 4, 5, 6, 7, time.UTC)
	a := domain.Asset{ID: id, Namespace: "blog", Object: domain.ContentObject{Hash: domain.ObjectHash(sha256.Sum256(data)), MIME: "image/png", Ext: "png", Size: int64(len(data)), Width: 3, Height: 2}, OriginalName: "截图.png", Title: "图库 demo", Tags: []string{"tag", "标签"}, Collections: []string{"collection"}, CreatedAt: now, UpdatedAt: now}
	metadata, err := repository.EncodeAsset(a)
	if err != nil {
		t.Fatal(err)
	}
	head := publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.AssetPath(id), Blob: blob(t, ctx, r, metadata)}, {Path: repository.BuildObjectPath(a.Namespace, a.Object), Blob: blob(t, ctx, r, data)}})
	result, err := s.Sync(ctx, r)
	if err != nil || result.Head != head || result.AssetCount != 1 {
		t.Fatal(result, err)
	}
	st, err := s.Store.Load(ctx)
	if err != nil || !reflect.DeepEqual(st.Assets[id], a) {
		t.Fatalf("metadata differs: %#v %v", st.Assets[id], err)
	}
	// A fresh process/cache must reconstruct all metadata from Git alone.
	fresh, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err = fresh.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	other, _ := fresh.Store.Load(ctx)
	if !reflect.DeepEqual(other.Assets, st.Assets) || other.Head != st.Head {
		t.Fatal("fresh snapshot mismatch")
	}
	bad := blob(t, ctx, r, []byte(`{"broken":true}`))
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.AssetPath(id), Blob: bad}})
	if _, err = s.Sync(ctx, r); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	after, _ := s.Store.Load(ctx)
	if after.Head != head || !reflect.DeepEqual(after.Assets, st.Assets) {
		t.Fatal("corrupt remote replaced cache")
	}
}
func TestSyncRejectsMissingObjectAndRepositoryIdentity(t *testing.T) {
	ctx, r, s := realRepository(t)
	a := testSyncAsset(t)
	b, _ := repository.EncodeAsset(a)
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.AssetPath(a.ID), Blob: blob(t, ctx, r, b)}})
	before, _ := s.Store.Load(ctx)
	if _, err := s.Sync(ctx, r); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	after, _ := s.Store.Load(ctx)
	if before.Head != after.Head {
		t.Fatal("cache replaced")
	}
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.AssetPath(a.ID)}})
	id, _ := domain.NewRepositoryID()
	m, _ := repository.EncodeManifest(domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"})
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ManifestPath, Blob: blob(t, ctx, r, m)}})
	if _, err := s.Sync(ctx, r); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
}
func testSyncAsset(t *testing.T) domain.Asset {
	t.Helper()
	id, _ := domain.NewAssetID()
	now := time.Now().UTC()
	return domain.Asset{ID: id, Namespace: "default", Object: domain.ContentObject{Hash: domain.ObjectHash(sha256.Sum256([]byte("data"))), MIME: "image/png", Ext: "png", Size: 4, Width: 1, Height: 1}, CreatedAt: now, UpdatedAt: now}
}
func TestInitializeRefusesReservedPathsAndIsIdempotent(t *testing.T) {
	ctx, r, s := realRepository(t)
	head, _ := r.Head(ctx)
	if _, err := s.Initialize(ctx, r); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Head(ctx)
	if after != head {
		t.Fatal("idempotent init created commit")
	}
	raw, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "raw.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	id := blob(t, ctx, raw, []byte("README"))
	tree, err := raw.CreateTree(ctx, "", []repository.TreeChange{{Path: "README.md", Blob: id}, {Path: "objects/occupied", Blob: id}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := raw.CreateCommit(ctx, tree, "", "raw")
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.UpdateRef(ctx, "", h); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err = fresh.Initialize(ctx, raw); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	current, _ := raw.Head(ctx)
	if current != h {
		t.Fatal("init overwrote reserved paths")
	}
}
