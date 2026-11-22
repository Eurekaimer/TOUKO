package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/png"
	"os"
	"strings"
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
func TestStagingNormalizationAndPayloadIdentity(t *testing.T) {
	s, ctx := newStagedApp(t)
	data := pngBytes(t)
	a, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "图片.png"}, {Reader: bytes.NewReader(data), Name: "same.png"}}, AddOptions{Namespace: "blog", Tags: []string{" z ", "a", "a", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Asset.ID == a[1].Asset.ID || a[0].Asset.OriginalName != "图片.png" || a[0].Asset.Object.HasHash() || a[1].Asset.Object.HasHash() {
		t.Fatal(a)
	}
	// A hashless upload is named by its staging path, so identical bytes stay
	// two independent staged payloads instead of being merged by content.
	firstPayload := staging.PayloadPath(s.Dir, a[0].Asset)
	secondPayload := staging.PayloadPath(s.Dir, a[1].Asset)
	if firstPayload == secondPayload || !strings.HasSuffix(firstPayload, a[0].Asset.ID.String()+".png") {
		t.Fatalf("hashless payloads were conflated: %q %q", firstPayload, secondPayload)
	}
	title := "edited"
	if err = s.Edit(ctx, a[0].Asset.ID.String(), EditOptions{Title: &title}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Store.Load(ctx)
	if st.Changes[a[0].Asset.ID].Kind != domain.Add {
		t.Fatal(st.Changes)
	}
	if err = s.Remove(ctx, []string{a[0].Asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(secondPayload); err != nil {
		t.Fatal("unrelated staged payload removed", err)
	}
	if _, err = os.Stat(firstPayload); !os.IsNotExist(err) {
		t.Fatalf("removed asset kept its own payload: %v", err)
	}
	if err = s.Remove(ctx, []string{a[1].Asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Store.Load(ctx)
	if len(st.Changes) != 0 || st.BaseHead != "" {
		t.Fatal(st)
	}
	if _, err = os.Stat(secondPayload); !os.IsNotExist(err) {
		t.Fatalf("unreferenced payload retained: %v", err)
	}
	if err = s.Remove(ctx, []string{a[0].Asset.ID.String()}); !errors.Is(err, domain.ErrAssetNotFound) {
		t.Fatal(err)
	}
	// Opting into the digest restores content-addressed sharing: identical
	// bytes ingest to one staged payload while remaining two assets.
	hashed, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "one.png"}, {Reader: bytes.NewReader(data), Name: "two.png"}}, AddOptions{Namespace: "blog", ComputeHash: true})
	if err != nil {
		t.Fatal(err)
	}
	if !hashed[0].Asset.Object.HasHash() || staging.PayloadPath(s.Dir, hashed[0].Asset) != staging.PayloadPath(s.Dir, hashed[1].Asset) {
		t.Fatalf("opt-in payload is not content addressed: %#v", hashed)
	}
	shared := staging.PayloadPath(s.Dir, hashed[0].Asset)
	if err = s.Remove(ctx, []string{hashed[0].Asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(shared); err != nil {
		t.Fatal("shared payload removed with one reference", err)
	}
}
func TestAddComputeHashCoversConvertedBytes(t *testing.T) {
	s, ctx := newStagedApp(t)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 3)), Name: "photo.png"}}, AddOptions{Namespace: "blog", ConvertWebP: true, ComputeHash: true})
	if err != nil {
		t.Fatal(err)
	}
	asset := views[0].Asset
	if !asset.Object.HasHash() {
		t.Fatal("opt-in digest missing")
	}
	published, err := os.ReadFile(staging.PayloadPath(s.Dir, asset))
	if err != nil {
		t.Fatal(err)
	}
	if asset.Object.Size != int64(len(published)) || asset.Object.Hash != domain.ObjectHash(sha256.Sum256(published)) {
		t.Fatalf("digest does not cover staged bytes: %+v", asset.Object)
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
