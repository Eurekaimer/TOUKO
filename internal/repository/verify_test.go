package repository_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestVerifyRejectsCorruptObject(t *testing.T) {
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	m := image.NewRGBA(image.Rect(0, 0, 2, 3))
	m.Set(0, 0, color.RGBA{255, 0, 0, 255})
	if err = png.Encode(&data, m); err != nil {
		t.Fatal(err)
	}
	obj := domain.ContentObject{Hash: domain.ObjectHash(sha256.Sum256(data.Bytes())), MIME: "image/png", Ext: "png", Size: int64(data.Len()), Width: 2, Height: 3}
	good, err := r.CreateBlob(ctx, bytes.NewReader(data.Bytes()), int64(data.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.VerifyObject(ctx, r, good, obj); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), data.Bytes()...)
	bad[len(bad)-1] ^= 1
	wrong, err := r.CreateBlob(ctx, bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.VerifyObject(ctx, r, wrong, obj); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
	badSize := obj
	badSize.Size++
	if err = repository.VerifyObject(ctx, r, good, badSize); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
	badDimension := obj
	badDimension.Width++
	if err = repository.VerifyObject(ctx, r, good, badDimension); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	id, _ := domain.NewAssetID()
	now := time.Now().UTC()
	a := domain.Asset{ID: id, Namespace: "blog", Object: obj, CreatedAt: now, UpdatedAt: now}
	s := repository.Snapshot{Head: "fixed", Assets: map[domain.AssetID]domain.Asset{id: a}, Entries: []repository.GitEntry{{Path: repository.LegacyObjectPath(a.Namespace, obj), Mode: "100644", Type: "blob", SHA: string(wrong), Size: obj.Size}}}
	if _, err = repository.VerifySnapshot(ctx, r, s); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
	s.Entries[0].SHA = string(good)
	other := a
	other.ID, _ = domain.NewAssetID()
	other.Namespace = "notes"
	s.Assets[other.ID] = other
	s.Entries = append(s.Entries, repository.GitEntry{Path: repository.LegacyObjectPath(other.Namespace, obj), Mode: "100644", Type: "blob", SHA: string(good), Size: obj.Size})
	result, err := repository.VerifySnapshot(ctx, r, s)
	if err != nil || result.Assets != 2 || result.Objects != 2 || result.Head != "fixed" {
		t.Fatal(result, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = repository.VerifyObject(canceled, r, good, obj); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestVerifyHashlessExplicitObjectUsesDescription(t *testing.T) {
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	m := image.NewRGBA(image.Rect(0, 0, 4, 5))
	m.Set(1, 1, color.RGBA{G: 255, A: 255})
	if err = png.Encode(&data, m); err != nil {
		t.Fatal(err)
	}
	object := domain.ContentObject{MIME: "image/png", Ext: "png", Size: int64(data.Len()), Width: 4, Height: 5}
	blob, err := r.CreateBlob(ctx, bytes.NewReader(data.Bytes()), int64(data.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.VerifyObject(ctx, r, blob, object); err != nil {
		t.Fatalf("hashless explicit object rejected: %v", err)
	}
	wrongSize := object
	wrongSize.Size--
	if err = repository.VerifyObject(ctx, r, blob, wrongSize); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
	wrongHeader := object
	wrongHeader.Height++
	if err = repository.VerifyObject(ctx, r, blob, wrongHeader); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}

	id, _ := domain.NewAssetID()
	now := time.Now().UTC()
	asset := domain.Asset{ID: id, Namespace: "blog", Object: object, Path: "1789000000000000000.png", CreatedAt: now, UpdatedAt: now}
	snap := repository.Snapshot{Head: "fixed", Manifest: domain.Repository{FormatVersion: 2, HashAlgorithm: "sha256"}, Assets: map[domain.AssetID]domain.Asset{id: asset}, Entries: []repository.GitEntry{{Path: asset.Path, Mode: "100644", Type: "blob", SHA: string(blob), Size: object.Size}}}
	if result, err := repository.VerifySnapshot(ctx, r, snap); err != nil || result.Objects != 1 {
		t.Fatalf("hashless snapshot verify = %#v, %v", result, err)
	}
	broken := snap
	brokenAsset := asset
	brokenAsset.Object.Height++
	broken.Assets = map[domain.AssetID]domain.Asset{id: brokenAsset}
	if _, err := repository.VerifySnapshot(ctx, r, broken); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("hashless header corruption accepted: %v", err)
	}
	pathless := snap
	pathlessAsset := asset
	pathlessAsset.Object.Hash = domain.ObjectHash{}
	pathlessAsset.Path = ""
	pathless.Assets = map[domain.AssetID]domain.Asset{id: pathlessAsset}
	if _, err := repository.VerifySnapshot(ctx, r, pathless); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("hashless asset without explicit path accepted: %v", err)
	}
}
