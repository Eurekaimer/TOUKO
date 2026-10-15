package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

func pngBytes(t testing.TB) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func newStagedApp(t *testing.T) (*Service, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, _ := domain.NewRepositoryID()
	m := domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"}
	if err = s.Store.Save(ctx, index.State{Head: "base", Manifest: &m, Assets: map[domain.AssetID]domain.Asset{}, Changes: map[domain.AssetID]domain.Change{}}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}
func TestStagingNormalizationAndSharedPayload(t *testing.T) {
	s, ctx := newStagedApp(t)
	data := pngBytes(t)
	a, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "图片.png"}, {Reader: bytes.NewReader(data), Name: "same.png"}}, AddOptions{Namespace: "blog", Tags: []string{" z ", "a", "a", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Asset.ID == a[1].Asset.ID || a[0].Asset.Object != a[1].Asset.Object || a[0].Asset.OriginalName != "图片.png" {
		t.Fatal(a)
	}
	title := "edited"
	if err = s.Edit(ctx, a[0].Asset.ID.String(), EditOptions{Title: &title}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Store.Load(ctx)
	if st.Changes[a[0].Asset.ID].Kind != domain.Add {
		t.Fatal(st.Changes)
	}
	path := staging.PayloadPath(s.Dir, "blog", a[0].Asset.Object)
	if err = s.Remove(ctx, []string{a[0].Asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("shared payload removed", err)
	}
	if err = s.Remove(ctx, []string{a[1].Asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if len(st.Changes) != 0 || st.BaseHead != "" {
		t.Fatal(st)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unreferenced payload retained: %v", err)
	}
	if err = s.Remove(ctx, []string{a[0].Asset.ID.String()}); !errors.Is(err, domain.ErrAssetNotFound) {
		t.Fatal(err)
	}
}
func TestBatchFailureAndCommittedTransitions(t *testing.T) {
	s, ctx := newStagedApp(t)
	data := pngBytes(t)
	if _, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "ok.png"}, {Reader: bytes.NewReader([]byte("bad")), Name: "bad"}}, AddOptions{}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	st, _ := s.Store.Load(ctx)
	if len(st.Changes) != 0 {
		t.Fatal(st.Changes)
	}
	id, _ := domain.NewAssetID()
	now := time.Now().UTC()
	a := domain.Asset{ID: id, Namespace: "default", OriginalName: "base.png", Title: "base", Object: domain.ContentObject{Hash: domain.ObjectHash(sha256.Sum256(data)), MIME: "image/png", Ext: "png", Size: int64(len(data)), Width: 3, Height: 2}, CreatedAt: now, UpdatedAt: now}
	st.Assets[id] = a
	if err := s.Store.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, []string{id.String(), "ffffffff"}); !errors.Is(err, domain.ErrAssetNotFound) {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if len(st.Changes) != 0 {
		t.Fatal("partial remove")
	}
	title := "new"
	if err := s.Edit(ctx, id.String(), EditOptions{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, []string{id.String()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, []string{id.String()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Edit(ctx, id.String(), EditOptions{Title: &title}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	views, err := s.List(ctx)
	if err != nil || len(views) != 0 {
		t.Fatal(views, err)
	}
}
func TestLocalOperationLock(t *testing.T) {
	s, ctx := newStagedApp(t)
	unlock, err := s.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = s.Lock(ctx); !errors.Is(err, domain.ErrBusy) {
		t.Fatal(err)
	}
}
func TestRemoveDuplicateSelectorsCancelsAdd(t *testing.T) {
	s, ctx := newStagedApp(t)
	v, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t))}}, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := v[0].Asset.ID.String()
	if err = s.Remove(ctx, []string{id, id[:8]}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Store.Load(ctx)
	if err != nil || len(st.Changes) != 0 {
		t.Fatalf("duplicate removal left change: %v %v", st.Changes, err)
	}
}
