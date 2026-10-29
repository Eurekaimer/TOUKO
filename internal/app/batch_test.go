package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

type batchBlobFault struct {
	repository.Git
	blobs int
}

func (f *batchBlobFault) CreateBlob(ctx context.Context, reader io.Reader, size int64) (repository.BlobID, error) {
	f.blobs++
	if f.blobs == 63 {
		return "", errPublicationFault
	}
	return f.Git.CreateBlob(ctx, reader, size)
}

func batchPNG(t *testing.T, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			img.SetRGBA(x, y, color.RGBA{R: uint8(n), G: uint8(x * 4), B: uint8(y * 5), A: 255})
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func assertBatchGallery(t *testing.T, ctx context.Context, r repository.Git, s *Service, head domain.RepositoryHead, expected map[domain.AssetID]domain.Asset) {
	t.Helper()
	current, err := r.Head(ctx)
	if err != nil || current != head {
		t.Fatalf("remote head = %s, want %s: %v", current, head, err)
	}
	snapshot, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Assets, expected) {
		t.Fatalf("published metadata differs: got %#v, want %#v", snapshot.Assets, expected)
	}
	objects := make(map[string]bool)
	paths := []string{repository.ManifestPath}
	for id, asset := range expected {
		paths = append(paths, repository.AssetPath(id))
		objects[repository.ObjectPath(asset)] = true
	}
	for path := range objects {
		paths = append(paths, path)
	}
	actualPaths := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if entry.Type != "tree" {
			actualPaths = append(actualPaths, entry.Path)
		}
	}
	sort.Strings(paths)
	sort.Strings(actualPaths)
	if !reflect.DeepEqual(actualPaths, paths) {
		t.Fatalf("latest tree paths = %v, want %v", actualPaths, paths)
	}
	for _, device := range []*Service{s, secondService(t, ctx, r)} {
		state, err := device.Store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.Head != head || !reflect.DeepEqual(state.Assets, expected) || len(state.Changes) != 0 || state.BaseHead != "" || state.Publication != nil {
			t.Fatalf("committed cache or fresh sync differs: %#v", state)
		}
		views, err := device.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		visible := make(map[domain.AssetID]domain.Asset, len(views))
		for _, view := range views {
			if view.State != "committed" {
				t.Fatalf("published asset %s has state %s", view.Asset.ID, view.State)
			}
			visible[view.Asset.ID] = view.Asset
		}
		if !reflect.DeepEqual(visible, expected) {
			t.Fatalf("visible gallery differs: got %#v, want %#v", visible, expected)
		}
	}
	verified, err := s.Verify(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Head != head || verified.Assets != len(expected) || verified.Objects != len(objects) {
		t.Fatalf("verify = %#v, want head %s, assets %d, objects %d", verified, head, len(expected), len(objects))
	}
}

func TestAtomicBatchRetryAfterBlobFailure(t *testing.T) {
	ctx, r, s := realRepository(t)
	base, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := repository.ReadSnapshot(ctx, r, base.Head)
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]Input, 100)
	payloads := make([][]byte, len(inputs))
	for i := range inputs {
		payloads[i] = batchPNG(t, i)
		inputs[i] = Input{Reader: bytes.NewReader(payloads[i]), Name: fmt.Sprintf("样例-%03d.png", i)}
	}
	title := "atomic demo batch"
	views, err := s.Add(ctx, inputs, AddOptions{Namespace: "blog", Title: &title, Tags: []string{"batch", "demo"}, Collections: []string{"samples"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 100 {
		t.Fatalf("batch returned %d assets, want 100", len(views))
	}
	expected := make(map[domain.AssetID]domain.Asset, 100)
	hashes := make(map[domain.ObjectHash]bool, 100)
	for _, view := range views {
		if view.State != "added" || hashes[view.Asset.Object.Hash] {
			t.Fatalf("batch must contain distinct staged PNGs: %#v", view)
		}
		hashes[view.Asset.Object.Hash] = true
		expected[view.Asset.ID] = view.Asset
	}
	staged, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if staged.BaseHead != base.Head || len(staged.Changes) != 100 {
		t.Fatalf("incomplete staged batch: %#v", staged)
	}
	if _, err := s.Commit(ctx, &batchBlobFault{Git: r}, "atomic batch must fail"); !errors.Is(err, errPublicationFault) {
		t.Fatalf("commit error = %v, want injected blob failure", err)
	}
	head, err := r.Head(ctx)
	if err != nil || head != base.Head {
		t.Fatalf("failed batch changed remote head to %s: %v", head, err)
	}
	after, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed batch changed remote gallery: %#v: %v", after, err)
	}

	// A new connection must recover every change and payload without re-adding.
	restarted, err := Open(ctx, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	persisted, err := restarted.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, staged) || persisted.Head != base.Head || persisted.Tree != base.Tree || !reflect.DeepEqual(persisted.Assets, base.Assets) {
		t.Fatalf("failed batch changed committed cache or lost staging: %#v", persisted)
	}
	visible, err := restarted.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	visibleAssets := make(map[domain.AssetID]domain.Asset, len(visible))
	for _, view := range visible {
		if view.State != "added" {
			t.Fatalf("failed publication exposed asset as %s", view.State)
		}
		visibleAssets[view.Asset.ID] = view.Asset
	}
	if !reflect.DeepEqual(visibleAssets, expected) {
		t.Fatalf("failed publication lost staged gallery: %#v", visibleAssets)
	}
	for i, view := range views {
		change, ok := persisted.Changes[view.Asset.ID]
		if !ok || change.Kind != domain.Add || change.Asset == nil || !reflect.DeepEqual(*change.Asset, view.Asset) {
			t.Fatalf("asset %s lost its persisted add: %#v", view.Asset.ID, change)
		}
		data, err := os.ReadFile(staging.PayloadPath(s.Dir, view.Asset.Namespace, view.Asset.Object))
		if err != nil || !bytes.Equal(data, payloads[i]) {
			t.Fatalf("asset %s lost its staged PNG: %v", view.Asset.ID, err)
		}
	}

	result := mustCommit(t, ctx, restarted, r)
	if result.ChangeCount != 100 || result.AssetCount != 100 || result.Head == base.Head {
		t.Fatalf("retry result = %#v", result)
	}
	commit, err := r.Commit(ctx, result.Head)
	if err != nil || !reflect.DeepEqual(commit.Parents, []domain.RepositoryHead{base.Head}) {
		t.Fatalf("retry must publish exactly one visible commit above base: %#v: %v", commit, err)
	}
	assertBatchGallery(t, ctx, r, restarted, result.Head, expected)
}

func TestCommitDeleteAndMetadataEdit(t *testing.T) {
	ctx, r, s := realRepository(t)
	views, err := s.Add(ctx, []Input{
		{Reader: bytes.NewReader(batchPNG(t, 1)), Name: "delete.png"},
		{Reader: bytes.NewReader(batchPNG(t, 2)), Name: "keep.png"},
	}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	base := mustCommit(t, ctx, s, r).Head
	removed, kept := views[0].Asset, views[1].Asset
	if err := s.Remove(ctx, []string{removed.ID.String()}); err != nil {
		t.Fatal(err)
	}
	title := "edited survivor"
	tags, collections := []string{" z ", "demo", "demo"}, []string{" final ", "final"}
	if err := s.Edit(ctx, kept.ID.String(), EditOptions{Title: &title, Tags: &tags, Collections: &collections}); err != nil {
		t.Fatal(err)
	}
	shown, err := s.Show(ctx, kept.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	expected := kept
	expected.Title = title
	expected.Tags = []string{"demo", "z"}
	expected.Collections = []string{"final"}
	expected.UpdatedAt = shown.Asset.UpdatedAt
	if shown.State != "modified" || !reflect.DeepEqual(shown.Asset, expected) {
		t.Fatalf("metadata edit changed immutable fields or lost values: %#v", shown)
	}
	if _, err := s.Show(ctx, removed.ID.String()); !errors.Is(err, domain.ErrAssetNotFound) {
		t.Fatalf("staged delete remains visible: %v", err)
	}
	result := mustCommit(t, ctx, s, r)
	if result.ChangeCount != 2 || result.AssetCount != 1 || result.Head == base {
		t.Fatalf("mixed delete/edit result = %#v", result)
	}
	assertBatchGallery(t, ctx, r, s, result.Head, map[domain.AssetID]domain.Asset{expected.ID: expected})
}

func TestCommitDeleteAndAddSameBytes(t *testing.T) {
	ctx, r, s := realRepository(t)
	original := stageImage(t, ctx, s)
	base := mustCommit(t, ctx, s, r).Head
	before, err := repository.ReadSnapshot(ctx, r, base)
	if err != nil {
		t.Fatal(err)
	}
	objectPath := repository.ObjectPath(original)
	var originalBlob string
	for _, entry := range before.Entries {
		if entry.Path == objectPath {
			originalBlob = entry.SHA
		}
	}
	if originalBlob == "" {
		t.Fatal("initial committed object missing")
	}
	if err := s.Remove(ctx, []string{original.ID.String()}); err != nil {
		t.Fatal(err)
	}
	replacement := stageImage(t, ctx, s)
	if replacement.ID == original.ID || replacement.Object != original.Object || replacement.Namespace != original.Namespace {
		t.Fatalf("replacement must have new identity and reuse same placement: %#v", replacement)
	}
	result := mustCommit(t, ctx, s, r)
	if result.ChangeCount != 2 || result.AssetCount != 1 || result.Head == base {
		t.Fatalf("replacement result = %#v", result)
	}
	assertBatchGallery(t, ctx, r, s, result.Head, map[domain.AssetID]domain.Asset{replacement.ID: replacement})
	after, err := repository.ReadSnapshot(ctx, r, result.Head)
	if err != nil {
		t.Fatal(err)
	}
	// Each asset owns its path. Identical bytes still share one Git blob, so the
	// replacement is published at its own path with the same SHA while the
	// removed asset's path disappears.
	replacementPath := repository.ObjectPath(replacement)
	present := false
	for _, entry := range after.Entries {
		if entry.Path == objectPath {
			t.Fatalf("removed asset kept its object path %s", objectPath)
		}
		if entry.Path == replacementPath {
			present = true
			if entry.SHA != originalBlob {
				t.Fatalf("replacement changed bytes: %s, want %s", entry.SHA, originalBlob)
			}
		}
	}
	if !present {
		t.Fatalf("replacement object %s missing", replacementPath)
	}
}

// The asset-id layout intentionally stops sharing placements: the same bytes
// uploaded twice are two assets with two paths and two objects.
func TestDuplicateBytesGetIndependentObjects(t *testing.T) {
	ctx, r, s := realRepository(t)
	first := stageImage(t, ctx, s)
	second := stageImage(t, ctx, s)
	if first.ID == second.ID || first.Object != second.Object {
		t.Fatalf("fixture must reuse bytes with new identity: %#v %#v", first.ID, second.ID)
	}
	if first.Path == second.Path || first.Path == "" || second.Path == "" {
		t.Fatalf("assets do not own distinct paths: %q %q", first.Path, second.Path)
	}
	head := mustCommit(t, ctx, s, r).Head
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string]string{}
	for _, entry := range snap.Entries {
		blobs[entry.Path] = entry.SHA
	}
	if blobs[first.Path] == "" || blobs[first.Path] != blobs[second.Path] {
		t.Fatalf("duplicate bytes must keep one blob at two paths: %#v", blobs)
	}
	if err := s.Remove(ctx, []string{first.ID.String()}); err != nil {
		t.Fatal(err)
	}
	head = mustCommit(t, ctx, s, r).Head
	snap, err = repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, entry := range snap.Entries {
		remaining[entry.Path] = true
	}
	if remaining[first.Path] || !remaining[second.Path] {
		t.Fatalf("deleting one asset removed the wrong path: %#v", remaining)
	}
}

func TestCommitStagedAddEditRemoveNormalization(t *testing.T) {
	t.Run("cancel_all", func(t *testing.T) {
		ctx, r, s := realRepository(t)
		base, err := r.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		asset := stageImage(t, ctx, s)
		title := "cancel edited add"
		if err := s.Edit(ctx, asset.ID.String(), EditOptions{Title: &title}); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(ctx, []string{asset.ID.String()}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(staging.PayloadPath(s.Dir, asset.Namespace, asset.Object)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled add retained payload: %v", err)
		}
		result := mustCommit(t, ctx, s, r)
		if result.Head != base || result.ChangeCount != 0 || result.AssetCount != 0 {
			t.Fatalf("canceled add created a commit or asset: %#v", result)
		}
		assertBatchGallery(t, ctx, r, s, base, map[domain.AssetID]domain.Asset{})
	})
	t.Run("shared_payload_survives", func(t *testing.T) {
		ctx, r, s := realRepository(t)
		kept := stageImage(t, ctx, s)
		canceled := stageImage(t, ctx, s)
		title := "normalized staged survivor"
		tags := []string{" demo ", "demo", "keep", ""}
		if err := s.Edit(ctx, kept.ID.String(), EditOptions{Title: &title, Tags: &tags}); err != nil {
			t.Fatal(err)
		}
		if err := s.Edit(ctx, canceled.ID.String(), EditOptions{Title: &title}); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(ctx, []string{canceled.ID.String()}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(staging.PayloadPath(s.Dir, kept.Namespace, kept.Object))
		if err != nil || !bytes.Equal(data, pngBytes(t)) {
			t.Fatalf("canceling duplicate damaged surviving payload: %v", err)
		}
		shown, err := s.Show(ctx, kept.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		expected := kept
		expected.Title = title
		expected.Tags = []string{"demo", "keep"}
		expected.UpdatedAt = shown.Asset.UpdatedAt
		if shown.State != "added" || !reflect.DeepEqual(shown.Asset, expected) {
			t.Fatalf("editing staged add lost identity or metadata: %#v", shown)
		}
		state, err := s.Store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		change, exists := state.Changes[kept.ID]
		if len(state.Changes) != 1 || !exists || change.Kind != domain.Add || change.Asset == nil || !reflect.DeepEqual(*change.Asset, expected) {
			t.Fatalf("add/edit/remove did not normalize to one final add: %#v", state.Changes)
		}
		result := mustCommit(t, ctx, s, r)
		if result.ChangeCount != 1 || result.AssetCount != 1 {
			t.Fatalf("normalized commit = %#v", result)
		}
		assertBatchGallery(t, ctx, r, s, result.Head, map[domain.AssetID]domain.Asset{expected.ID: expected})
	})
}
