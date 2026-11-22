package app

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
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
	if a.Path != "" || item.Directory != "" {
		t.Fatalf("preparation must not fix the staging path: %#v", item)
	}
	// Preparing the same bytes twice must yield two assets with one object: the
	// content identity is shared, the asset identity is not.
	other := preparedPng(t, "same bytes")
	if other.Asset.ID == a.ID || other.Asset.Object != a.Object {
		t.Fatalf("content/asset identity conflated: %+v", other.Asset)
	}
}

func TestAddAssignsStagingTimePathsAndOptionalHash(t *testing.T) {
	s, ctx := newStagedApp(t)
	data := pngBytes(t)
	plain, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "one.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	asset := plain[0].Asset
	if asset.Object.HasHash() {
		t.Fatalf("default upload recorded a digest: %+v", asset.Object)
	}
	if !repository.IsTimestampObjectPath(asset.Path, "png") || strings.Contains(asset.Path, "/") {
		t.Fatalf("default path is not a root timestamp name: %q", asset.Path)
	}
	if _, err := os.Stat(staging.PayloadPath(s.Dir, asset)); err != nil {
		t.Fatalf("hashless payload is not keyed by asset identity: %v", err)
	}
	second, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "two.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Asset.Path == asset.Path {
		t.Fatal("two uploads claimed one path")
	}
	directory, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "three.png"}}, AddOptions{Namespace: "blog", Directory: "my images"})
	if err != nil {
		t.Fatal(err)
	}
	if path := directory[0].Asset.Path; !strings.HasPrefix(path, "my-images/") || !repository.IsTimestampObjectPath(path, "png") {
		t.Fatalf("explicit upload directory lost: %q", path)
	}
	hashed, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "four.png"}}, AddOptions{Namespace: "blog", ComputeHash: true})
	if err != nil {
		t.Fatal(err)
	}
	if !hashed[0].Asset.Object.HasHash() || hashed[0].Asset.Object.Hash != domain.ObjectHash(sha256.Sum256(data)) {
		t.Fatalf("opt-in digest: %+v", hashed[0].Asset.Object)
	}
}

func TestAllocateTimestampPathsResolvesSharedNanosecond(t *testing.T) {
	at := time.Date(2026, 11, 22, 10, 23, 45, 123456789, time.UTC)
	object := domain.ContentObject{MIME: "image/png", Ext: "png", Size: 10, Width: 1, Height: 1}
	inputs := make([]PreparedInput, 4)
	for i := range inputs {
		inputs[i].Asset.Object = object
	}
	inputs[3].Directory = "my images"
	occupied := map[string]domain.ContentObject{}
	if err := allocateTimestampPaths(inputs, at, occupied); err != nil {
		t.Fatal(err)
	}
	stamp := repository.TimestampObjectPath(at, "png", 0)
	if got := inputs[0].Asset.Path; got != stamp {
		t.Fatalf("first path = %q", got)
	}
	if got := inputs[1].Asset.Path; got != repository.TimestampObjectPath(at, "png", 1) {
		t.Fatalf("shared nanosecond did not take the next suffix: %q", got)
	}
	if got := inputs[2].Asset.Path; got != repository.TimestampObjectPath(at, "png", 2) {
		t.Fatalf("shared collision counter did not advance: %q", got)
	}
	if got := inputs[3].Asset.Path; !strings.HasPrefix(got, "my-images/") || !repository.IsTimestampObjectPath(got, "png") {
		t.Fatalf("directory prefix not preserved: %q", got)
	}
	next := []PreparedInput{{Asset: domain.Asset{Object: object}}}
	if err := allocateTimestampPaths(next, at, occupied); err != nil {
		t.Fatal(err)
	}
	if next[0].Asset.Path != repository.TimestampObjectPath(at, "png", 3) {
		t.Fatalf("staged path was reused: %q", next[0].Asset.Path)
	}
	explicit := []PreparedInput{{Asset: domain.Asset{Path: "already/set.png", Object: object}}}
	if err := allocateTimestampPaths(explicit, at, occupied); err != nil || explicit[0].Asset.Path != "already/set.png" {
		t.Fatalf("explicit path overwritten: %q %v", explicit[0].Asset.Path, err)
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
