package app

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/image"
)

func preparedPng(t *testing.T, name string) PreparedInput {
	t.Helper()
	data := pngBytes(t)
	result, err := image.Ingest(t.Context(), bytes.NewReader(data), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cleanup := result.TempPath
	t.Cleanup(func() {
		// Staging moves the file; removing a missing temporary is harmless.
		_ = os.Remove(cleanup)
	})
	tags := []string{"picgo", "legacy"}
	item, err := PrepareInput(result, name, AddOptions{Namespace: "default", Tags: tags, Collections: []string{"MyIMGs"}})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestPrepareInputKeepsIdentityAndMetadata(t *testing.T) {
	item := preparedPng(t, "旧图 无后缀")
	a := item.Asset
	if a.ID == (domain.AssetID{}) || a.Object != item.Payload.Object {
		t.Fatalf("prepared input lost its identity: %+v", item)
	}
	if a.OriginalName != "旧图 无后缀" || a.Title != "旧图 无后缀" || a.Namespace != "default" {
		t.Fatalf("metadata: %+v", a)
	}
	if len(a.Tags) != 2 || a.Tags[0] != "legacy" || a.Tags[1] != "picgo" || len(a.Collections) != 1 {
		t.Fatalf("normalized lists: %+v", a.Tags)
	}
	// Preparing the same bytes twice must yield two assets with one object: the
	// content identity is shared, the asset identity is not.
	other := preparedPng(t, "same bytes")
	if other.Asset.ID == a.ID || other.Asset.Object != a.Object {
		t.Fatalf("content/asset identity conflated: %+v", other.Asset)
	}
}

func TestAddPreparedRejectsCapacityWithoutChangingState(t *testing.T) {
	ctx, repositoryGit, s := realRepository(t)
	item := preparedPng(t, "big.png")
	limit := item.Asset.Object.Size - 1
	_, err := s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: repositoryGit, MaxBytes: limit})
	var full *CapacityError
	if !errors.As(err, &full) || !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("expected capacity rejection, got %v", err)
	}
	if full.Capacity.ProjectedBytes <= limit || full.Capacity.RemainingBytes != 0 {
		t.Fatalf("capacity report: %+v", full.Capacity)
	}
	st, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 0 {
		t.Fatalf("rejected batch staged changes: %+v", st.Changes)
	}
}

func TestAddPreparedAcceptsExactBoundaryAndRetainsEarlierStage(t *testing.T) {
	ctx, repositoryGit, s := realRepository(t)
	first, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "first.png"}}, AddOptions{Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	item := preparedPng(t, "second.png")
	capacity, err := s.PreviewPrepared(ctx, repositoryGit, []PreparedInput{item}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Head == "" || capacity.ProjectedBytes <= capacity.UsedBytes {
		t.Fatalf("capacity preview: %+v", capacity)
	}
	views, err := s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: repositoryGit, MaxBytes: capacity.ProjectedBytes})
	if err != nil {
		t.Fatalf("exact boundary must be accepted: %v", err)
	}
	if len(views) != 1 || views[0].State != "added" {
		t.Fatalf("added views: %+v", views)
	}
	st, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 2 {
		t.Fatalf("earlier staged work was lost: %+v", st.Changes)
	}
	if _, ok := st.Changes[first[0].Asset.ID]; !ok {
		t.Fatal("first staged asset disappeared")
	}
}

func TestAddPreparedReportsStaleHead(t *testing.T) {
	ctx, repositoryGit, s := realRepository(t)
	item := preparedPng(t, "stale.png")
	// Stage an unrelated change first, then advance the repository HEAD so the
	// cached snapshot is genuinely stale.
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "advance.png"}}, AddOptions{Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	publishRaw(t, ctx, repositoryGit, nil)
	_, err := s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: repositoryGit, MaxBytes: 0})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Reason != "head_changed" {
		t.Fatalf("expected head conflict, got %v", err)
	}
}
