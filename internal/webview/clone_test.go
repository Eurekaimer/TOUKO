package webview

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestLocalImageVerifiesIndexedBytesAtMutablePath(t *testing.T) {
	dir := t.TempDir()
	data := imageBytes(t)
	sum := sha256.Sum256(data)
	asset := domain.Asset{
		Namespace: "default",
		Object: domain.ContentObject{
			Hash: domain.ObjectHash(sum), MIME: "image/png", Ext: "png",
			Size: int64(len(data)), Width: 7, Height: 5,
		},
		Path: "20261122120000-0000.png",
	}
	path := filepath.Join(dir, filepath.FromSlash(repository.ObjectPath(asset)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	source, ok, err := localImage(dir, &asset)
	if err != nil || !ok {
		t.Fatalf("matching working file: ok=%v err=%v", ok, err)
	}
	got, err := io.ReadAll(source)
	source.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("working file bytes differ: %v", err)
	}

	wrong := bytes.Clone(data)
	wrong[len(wrong)-1] ^= 1
	if err := os.WriteFile(path, wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := localImage(dir, &asset); err != nil || ok {
		t.Fatalf("same-size changed file trusted: ok=%v err=%v", ok, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := localImage(dir, &asset); err != nil || ok {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}

	hashless := asset
	hashless.Object.Hash = domain.ObjectHash{}
	if _, ok, err := localImage(dir, &hashless); err != nil || ok {
		t.Fatalf("hashless asset trusted: ok=%v err=%v", ok, err)
	}
}
