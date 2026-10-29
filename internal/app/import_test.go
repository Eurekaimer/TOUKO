package app

import (
	"bytes"
	"context"
	"errors"
	stdimage "image"
	"image/color"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/gen2brain/webp"
)

func webpBytes(t *testing.T) []byte {
	t.Helper()
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, 9, 6))
	img.SetNRGBA(3, 2, color.NRGBA{R: 20, G: 180, B: 90, A: 255})
	var b bytes.Buffer
	if err := webp.Encode(&b, img, webp.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImportLegacyPreservesPathsAndPublishesRecoverableV2(t *testing.T) {
	ctx, r, s := realRepository(t)
	imageData := pngBytes(t)
	imageBlob := blob(t, ctx, r, imageData)
	base := publishRaw(t, ctx, r, []repository.TreeChange{
		{Path: "img/无后缀", Blob: imageBlob},
		{Path: "img/photo.PNG", Blob: imageBlob},
		{Path: "img/notes.txt", Blob: blob(t, ctx, r, []byte("not an image"))},
		{Path: "outside.png", Blob: imageBlob},
	})
	result, err := s.ImportLegacy(ctx, r, ImportOptions{Directory: "img", Namespace: "blog", Tags: []string{"picgo", "picgo"}})
	if err != nil || result.Imported != 2 || result.Skipped != 1 {
		t.Fatalf("import: %#v, %v", result, err)
	}
	if head, _ := r.Head(ctx); head != base {
		t.Fatal("import published instead of staging")
	}
	staged, err := s.Store.Load(ctx)
	if err != nil || staged.Manifest.FormatVersion != 1 || len(staged.Changes) != 2 {
		t.Fatalf("staging upgraded manifest prematurely: %#v, %v", staged, err)
	}
	for _, change := range staged.Changes {
		if change.Asset == nil || (change.Asset.Path != "img/无后缀" && change.Asset.Path != "img/photo.PNG") || !reflect.DeepEqual(change.Asset.Tags, []string{"picgo"}) {
			t.Fatalf("incorrect imported metadata: %#v", change)
		}
	}
	if again, err := s.ImportLegacy(ctx, r, ImportOptions{Directory: "img"}); err != nil || again.Imported != 0 || again.Skipped != 3 {
		t.Fatalf("staged import is not idempotent: %#v, %v", again, err)
	}
	fault := &publicationFault{Git: r, fail: "ref"}
	if _, err := s.Commit(ctx, fault, "import PicGo"); !errors.Is(err, errPublicationFault) {
		t.Fatalf("expected unpublished candidate: %v", err)
	}
	if _, err := s.Commit(ctx, r, "retry imported images"); err != nil {
		t.Fatalf("v2 candidate recovery: %v", err)
	}
	head, _ := r.Head(ctx)
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil || snap.Manifest.FormatVersion != 2 || len(snap.Assets) != 2 {
		t.Fatalf("published v2: %#v, %v", snap, err)
	}
	entries := make(map[string]repository.GitEntry)
	for _, entry := range snap.Entries {
		entries[entry.Path] = entry
	}
	for _, path := range []string{"img/无后缀", "img/photo.PNG", "outside.png"} {
		if entries[path].SHA != string(*imageBlob) {
			t.Fatalf("original image changed at %s", path)
		}
	}
	for _, asset := range snap.Assets {
		if _, exists := entries[repository.LegacyObjectPath(asset.Namespace, asset.Object)]; exists {
			t.Fatal("import copied original image to canonical objects")
		}
		body, err := r.OpenBlob(ctx, repository.BlobID(entries[repository.ObjectPath(asset)].SHA))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || !bytes.Equal(data, imageData) {
			t.Fatalf("real-path image unavailable: %v", err)
		}
	}
	fresh := secondService(t, ctx, r)
	state, err := fresh.Store.Load(ctx)
	if err != nil || !reflect.DeepEqual(state.Assets, snap.Assets) {
		t.Fatalf("fresh sync lost paths: %#v, %v", state, err)
	}
	if verified, err := fresh.Verify(ctx, r); err != nil || verified.Assets != 2 || verified.Objects != 2 {
		t.Fatalf("verify custom paths: %#v, %v", verified, err)
	}
	if again, err := fresh.ImportLegacy(ctx, r, ImportOptions{Directory: "img"}); err != nil || again.Imported != 0 || again.Skipped != 3 {
		t.Fatalf("committed import is not idempotent: %#v, %v", again, err)
	}
	for _, asset := range state.Assets {
		if asset.Path == "img/photo.PNG" {
			if err := fresh.Remove(ctx, []string{asset.ID.String()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	deleted := mustCommit(t, ctx, fresh, r)
	remaining, err := repository.ReadSnapshot(ctx, r, deleted.Head)
	if err != nil || len(remaining.Assets) != 1 {
		t.Fatalf("delete custom path: %#v, %v", remaining, err)
	}
	remainingPaths := make(map[string]bool)
	for _, entry := range remaining.Entries {
		remainingPaths[entry.Path] = true
	}
	if remainingPaths["img/photo.PNG"] || !remainingPaths["img/无后缀"] || !remainingPaths["outside.png"] || !remainingPaths["img/notes.txt"] {
		t.Fatalf("custom deletion removed unrelated or retained deleted files: %#v", remainingPaths)
	}
}

func TestImportLegacyRecognizesWebP(t *testing.T) {
	ctx, r, s := realRepository(t)
	data := webpBytes(t)
	publishRaw(t, ctx, r, []repository.TreeChange{
		{Path: "img/photo.webp", Blob: blob(t, ctx, r, data)},
	})
	result, err := s.ImportLegacy(ctx, r, ImportOptions{Directory: "img"})
	if err != nil || result.Imported != 1 || result.Skipped != 0 {
		t.Fatalf("WebP import: %#v, %v", result, err)
	}
	state, err := s.Store.Load(ctx)
	if err != nil || len(state.Changes) != 1 {
		t.Fatalf("WebP staging: %#v, %v", state, err)
	}
	for _, change := range state.Changes {
		if change.Asset == nil || change.Asset.Object.Ext != "webp" || change.Asset.Object.MIME != "image/webp" || change.Asset.Object.Width != 9 || change.Asset.Object.Height != 6 {
			t.Fatalf("WebP metadata: %+v", change)
		}
	}
}

func TestImportLegacyFailurePreservesExistingStage(t *testing.T) {
	ctx, r, s := realRepository(t)
	publishRaw(t, ctx, r, []repository.TreeChange{
		{Path: "legacy/a-valid", Blob: blob(t, ctx, r, pngBytes(t))},
		{Path: "legacy/z-broken.png", Blob: blob(t, ctx, r, []byte("\x89PNG\r\n\x1a\ntruncated"))},
	})
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	stageImage(t, ctx, s)
	before, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacy(ctx, r, ImportOptions{Directory: "legacy"}); err == nil {
		t.Fatal("corrupt supported image accepted")
	}
	after, err := s.Store.Load(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed import changed durable stage: %#v, %v", after, err)
	}
}

func TestInitializeLegacyObjectsAndRequireExplicitInitialization(t *testing.T) {
	ctx := context.Background()
	r, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "legacy.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	original := blob(t, ctx, r, pngBytes(t))
	tree, err := r.CreateTree(ctx, "", []repository.TreeChange{{Path: "objects/old-image", Blob: original}})
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.CreateCommit(ctx, tree, "", "legacy image")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "", head); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ImportLegacy(ctx, r, ImportOptions{}); !errors.Is(err, domain.ErrRepositoryUninitialized) {
		t.Fatalf("import did not require explicit initialization: %v", err)
	}
	if _, err := s.Initialize(ctx, r); err != nil {
		t.Fatalf("initialization rejected legacy objects: %v", err)
	}
	if result, err := s.ImportLegacy(ctx, r, ImportOptions{}); err != nil || result.Imported != 1 {
		t.Fatalf("initialized legacy import: %#v, %v", result, err)
	}
	mustCommit(t, ctx, s, r)
}

func TestAddDirectoryControlsActualPublishedLocation(t *testing.T) {
	ctx, r, s := realRepository(t)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "upload.png"}}, AddOptions{Namespace: "blog", Directory: "img/new"})
	if err != nil {
		t.Fatal(err)
	}
	asset := views[0].Asset
	want := "img/new/" + asset.ID.String() + ".png"
	if repository.ObjectPath(asset) != want || asset.Path != want {
		t.Fatalf("directory ignored: %#v", asset)
	}
	result := mustCommit(t, ctx, s, r)
	snap, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil || snap.Manifest.FormatVersion != 2 || snap.Assets[asset.ID].Path != want {
		t.Fatalf("directory publication: %#v, %v", snap, err)
	}
	if _, err := s.Verify(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t))}}, AddOptions{Directory: "../escape"}); err == nil {
		t.Fatal("unsafe directory accepted")
	}
}

type legacyModeRepository struct {
	repository.Git
	path string
	mode string
}

func (r legacyModeRepository) Tree(ctx context.Context, tree repository.TreeID) ([]repository.GitEntry, error) {
	entries, err := r.Git.Tree(ctx, tree)
	if err != nil {
		return nil, err
	}
	entries = append([]repository.GitEntry(nil), entries...)
	for i := range entries {
		if entries[i].Path == r.path {
			entries[i].Mode = r.mode
		}
	}
	return entries, nil
}

func TestImportLegacySkipsSymlinkEntries(t *testing.T) {
	ctx, r, s := realRepository(t)
	head := publishRaw(t, ctx, r, []repository.TreeChange{{Path: "links/image", Blob: blob(t, ctx, r, pngBytes(t))}})
	links := legacyModeRepository{Git: r, path: "links/image", mode: "120000"}
	result, err := s.ImportLegacy(ctx, links, ImportOptions{Directory: "links"})
	if err != nil || result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("symlink import: %#v, %v", result, err)
	}
	state, err := s.Store.Load(ctx)
	if err != nil || len(state.Changes) != 0 {
		t.Fatalf("symlink staged: %#v, %v", state, err)
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("symlink scan changed repository")
	}
}

func TestSnapshotExplicitPathRequiresV2AndRegularFile(t *testing.T) {
	ctx, r, s := realRepository(t)
	publishRaw(t, ctx, r, []repository.TreeChange{{Path: "img/existing", Blob: blob(t, ctx, r, pngBytes(t))}})
	if _, err := s.ImportLegacy(ctx, r, ImportOptions{Directory: "img"}); err != nil {
		t.Fatal(err)
	}
	state, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var imported domain.Asset
	for _, change := range state.Changes {
		imported = *change.Asset
	}
	metadata, err := repository.EncodeAsset(imported)
	if err != nil {
		t.Fatal(err)
	}
	v1 := publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.AssetPath(imported.ID), Blob: blob(t, ctx, r, metadata)}})
	if _, err := repository.ReadSnapshot(ctx, r, v1); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("v1 accepted explicit path: %v", err)
	}
	manifest := *state.Manifest
	manifest.FormatVersion = 2
	data, err := repository.EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	v2 := publishRaw(t, ctx, r, []repository.TreeChange{{Path: repository.ManifestPath, Blob: blob(t, ctx, r, data)}})
	snapshot, err := repository.ReadSnapshot(ctx, r, v2)
	if err != nil || snapshot.Assets[imported.ID].Path != "img/existing" {
		t.Fatalf("v2 lost explicit path: %#v, %v", snapshot, err)
	}
	links := legacyModeRepository{Git: r, path: "img/existing", mode: "120000"}
	if _, err := repository.ReadSnapshot(ctx, links, v2); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("metadata accepted symlink object: %v", err)
	}
}
