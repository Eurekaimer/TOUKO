package repository_test

import (
	"bytes"
	"context"
	stdimage "image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func hashlessPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 10, G: 200, B: 30, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestReadSnapshotAcceptsDistinctHashlessAssets(t *testing.T) {
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	repositoryID, _ := domain.NewRepositoryID()
	manifest, err := repository.EncodeManifest(domain.Repository{ID: repositoryID, FormatVersion: 2, HashAlgorithm: "sha256"})
	if err != nil {
		t.Fatal(err)
	}
	manifestBlob, err := r.CreateBlob(ctx, bytes.NewReader(manifest), int64(len(manifest)))
	if err != nil {
		t.Fatal(err)
	}
	changes := []repository.TreeChange{{Path: repository.ManifestPath, Blob: &manifestBlob}}
	now := time.Date(2026, 11, 22, 10, 23, 45, 0, time.UTC)
	for i, spec := range []struct {
		width, height int
		path          string
	}{
		{3, 2, "1789000000000000000.png"},
		{4, 5, "1789000000000000000-1.png"},
	} {
		data := hashlessPNG(t, spec.width, spec.height)
		object := domain.ContentObject{MIME: "image/png", Ext: "png", Size: int64(len(data)), Width: spec.width, Height: spec.height}
		blob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := domain.NewAssetID()
		asset := domain.Asset{ID: id, Namespace: "blog", Object: object, Path: spec.path, OriginalName: "upload.png", Title: "upload", CreatedAt: now, UpdatedAt: now}
		metadata, err := repository.EncodeAsset(asset)
		if err != nil {
			t.Fatalf("asset %d: %v", i, err)
		}
		metaBlob, err := r.CreateBlob(ctx, bytes.NewReader(metadata), int64(len(metadata)))
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes,
			repository.TreeChange{Path: repository.AssetPath(id), Blob: &metaBlob},
			repository.TreeChange{Path: spec.path, Blob: &blob},
		)
	}
	tree, err := r.CreateTree(ctx, "", changes)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.CreateCommit(ctx, tree, "", "hashless")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatalf("hashless snapshot rejected: %v", err)
	}
	if len(snap.Assets) != 2 || snap.Manifest.FormatVersion != 2 {
		t.Fatalf("snapshot = %#v", snap)
	}
	for _, asset := range snap.Assets {
		if asset.Object.HasHash() || asset.Path == "" {
			t.Fatalf("hashless metadata altered: %#v", asset)
		}
	}
}
