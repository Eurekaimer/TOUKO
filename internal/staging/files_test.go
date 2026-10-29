package staging

import (
	"bytes"
	"context"
	"errors"
	stdimage "image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/image"
)

func ingestPNG(t *testing.T, dir string) image.Result {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	result, err := image.Ingest(context.Background(), &b, filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func stagedAsset(t *testing.T, ns domain.Namespace, result image.Result) domain.Asset {
	t.Helper()
	id, err := domain.NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	return domain.Asset{ID: id, Namespace: ns, Object: result.Object}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

func TestInstallDeduplicatesWithinNamespace(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	first := ingestPNG(t, dir)
	firstAsset := stagedAsset(t, "blog", first)
	path, created, err := Install(ctx, dir, firstAsset, first)
	if err != nil || !created || path != PayloadPath(dir, firstAsset) {
		t.Fatalf("first installation: %s %v %v", path, created, err)
	}
	assertMissing(t, first.TempPath)
	second := ingestPNG(t, dir)
	secondAsset := stagedAsset(t, "blog", second)
	if secondAsset.ID == firstAsset.ID {
		t.Fatal("test assets share an identity")
	}
	samePath, created, err := Install(ctx, dir, secondAsset, second)
	if err != nil || created || samePath != path {
		t.Fatalf("deduplicated installation: %s %v %v", samePath, created, err)
	}
	assertMissing(t, second.TempPath)
	third := ingestPNG(t, dir)
	thirdAsset := stagedAsset(t, "notes", third)
	otherPath, created, err := Install(ctx, dir, thirdAsset, third)
	if err != nil || !created || otherPath == path || third.Object.Hash != first.Object.Hash {
		t.Fatalf("namespace placement: %s %v %v", otherPath, created, err)
	}
	for _, filePath := range []string{path, otherPath} {
		info, err := os.Stat(filePath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("payload permissions: %v %v", info, err)
		}
	}
	for _, directory := range []string{dir, filepath.Join(dir, "stage"), filepath.Join(dir, "stage", "objects"), filepath.Dir(path)} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("directory permissions: %v %v", info, err)
		}
	}
}

func TestInstallRejectsCorruptExistingWithoutOverwrite(t *testing.T) {
	for _, sameSize := range []bool{false, true} {
		t.Run(map[bool]string{false: "size", true: "hash"}[sameSize], func(t *testing.T) {
			dir := t.TempDir()
			result := ingestPNG(t, dir)
			asset := stagedAsset(t, "blog", result)
			path := PayloadPath(dir, asset)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			corrupt := []byte("wrong")
			if sameSize {
				corrupt = bytes.Repeat([]byte("x"), int(result.Object.Size))
			}
			if err := os.WriteFile(path, corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			gotPath, created, err := Install(context.Background(), dir, asset, result)
			if !errors.Is(err, domain.ErrHashMismatch) || created || gotPath != "" {
				t.Fatalf("corrupt target accepted: %s %v %v", gotPath, created, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, corrupt) {
				t.Fatalf("existing target overwritten: %v", err)
			}
			assertMissing(t, result.TempPath)
		})
	}
}

func TestInstallFailuresLeaveNoPayload(t *testing.T) {
	for _, name := range []string{"canceled", "corrupt_source", "invalid_namespace", "no_identity", "no_hash"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			result := ingestPNG(t, dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			asset := stagedAsset(t, "blog", result)
			var expected error
			switch name {
			case "canceled":
				cancel()
				expected = context.Canceled
			case "corrupt_source":
				if err := os.WriteFile(result.TempPath, []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
				expected = domain.ErrHashMismatch
			case "invalid_namespace":
				asset.Namespace = "../escape"
			case "no_identity":
				asset.ID = domain.AssetID{}
			case "no_hash":
				asset.Object.Hash = domain.ObjectHash{}
				result.Object.Hash = domain.ObjectHash{}
			}
			path, created, err := Install(ctx, dir, asset, result)
			if err == nil || created || path != "" || (expected != nil && !errors.Is(err, expected)) {
				t.Fatalf("unexpected failure result: %s %v %v", path, created, err)
			}
			assertMissing(t, result.TempPath)
			if asset.Object.HasHash() {
				assertMissing(t, PayloadPathFor(dir, "blog", result.Object))
			}
		})
	}
}

func TestCleanupPreservesReferencedPayloadAndUnrelatedFiles(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	result := ingestPNG(t, dir)
	asset := stagedAsset(t, "blog", result)
	path, _, err := Install(ctx, dir, asset, result)
	if err != nil {
		t.Fatal(err)
	}
	other := ingestPNG(t, dir)
	otherAsset := stagedAsset(t, "notes", other)
	otherPath, _, err := Install(ctx, dir, otherAsset, other)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".lock")
	unrelated := filepath.Join(dir, "stage", "unrelated")
	for _, p := range []string{lock, unrelated} {
		if err := os.WriteFile(p, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Multiple staged assets can reference this single placement: membership,
	// not number of assets or content hash alone, determines its retention.
	if err := Cleanup(ctx, dir, map[string]bool{path: true}); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, otherPath)
	for _, p := range []string{path, lock, unrelated} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("removed retained file %s: %v", p, err)
		}
	}
	if err := Cleanup(ctx, dir, nil); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, path)
	if err := Cleanup(ctx, dir, nil); err != nil {
		t.Fatalf("cleanup not idempotent: %v", err)
	}
}

func TestCleanupCanceledPreservesPayload(t *testing.T) {
	dir := t.TempDir()
	result := ingestPNG(t, dir)
	asset := stagedAsset(t, "blog", result)
	path, _, err := Install(context.Background(), dir, asset, result)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Cleanup(ctx, dir, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
