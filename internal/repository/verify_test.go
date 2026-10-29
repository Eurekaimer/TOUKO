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
