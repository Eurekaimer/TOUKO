package repository

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

func sampleAsset(t testing.TB) domain.Asset {
	t.Helper()
	id, err := domain.NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	return domain.Asset{ID: id, Namespace: "blog", Object: domain.ContentObject{Hash: domain.ObjectHash(sha256.Sum256([]byte("image"))), MIME: "image/png", Ext: "png", Size: 5, Width: 64, Height: 48}, OriginalName: "剪贴板.png", Title: "图片", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)}
}
func TestCanonicalPlacementAndMetadata(t *testing.T) {
	a := sampleAsset(t)
	path := BuildObjectPath(a.Namespace, a.Object)
	h := a.Object.Hash.String()
	if path != "objects/blog/sha256/"+h[:2]+"/"+h[2:4]+"/"+h+".png" {
		t.Fatal(path)
	}
	if path == BuildObjectPath("notes", a.Object) {
		t.Fatal("namespace must route placement")
	}
	data, err := EncodeAsset(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"tags":[]`)) || data[len(data)-1] != '\n' {
		t.Fatal(string(data))
	}
	got, err := DecodeAsset(data, a.ID.String()+".json")
	if err != nil {
		t.Fatal(err)
	}
	again, err := EncodeAsset(got)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("unstable metadata %v", err)
	}
	if _, err := DecodeAsset(data, "wrong.json"); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	if _, err := DecodeAsset(bytes.Replace(data, []byte(`"blog"`), []byte(`"../blog"`), 1), a.ID.String()+".json"); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	if _, err := DecodeAsset([]byte(strings.Repeat("x", MaxMetadataBytes+1)), a.ID.String()+".json"); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
	id, _ := domain.NewRepositoryID()
	m := domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"}
	b, err := EncodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifest(bytes.Replace(b, []byte(`"format_version":1`), []byte(`"format_version":2`), 1)); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatal(err)
	}
}
func FuzzDecodeAsset(f *testing.F) {
	a := sampleAsset(f)
	b, _ := EncodeAsset(a)
	name := a.ID.String() + ".json"
	f.Add(b)
	f.Add([]byte(`{"title":"图片"}`))
	f.Add(b[:len(b)/2])
	f.Add(bytes.Replace(b, []byte(a.Object.Hash.String()), []byte("bad"), 1))
	f.Add(bytes.Replace(b, []byte(`"blog"`), []byte(`"../x"`), 1))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := DecodeAsset(data, name)
		if err != nil {
			return
		}
		encoded, err := EncodeAsset(got)
		if err != nil {
			t.Fatal(err)
		}
		round, err := DecodeAsset(encoded, name)
		if err != nil || round.ID != got.ID || round.Object != got.Object {
			t.Fatalf("round trip: %v", err)
		}
		if !strings.HasPrefix(BuildObjectPath(got.Namespace, got.Object), "objects/"+string(got.Namespace)+"/sha256/") {
			t.Fatal("escaped placement")
		}
	})
}
