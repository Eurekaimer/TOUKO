package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

type snapshotFixture struct {
	mu           sync.Mutex
	entries      []GitEntry
	blobs        map[BlobID][]byte
	active, peak int
	calls        map[BlobID]int
	delay        time.Duration
	barrier      chan struct{}
}

func (f *snapshotFixture) Head(context.Context) (domain.RepositoryHead, error) { return "head", nil }
func (f *snapshotFixture) Commit(context.Context, domain.RepositoryHead) (GitCommit, error) {
	return GitCommit{Tree: "tree"}, nil
}
func (f *snapshotFixture) Tree(context.Context, TreeID) ([]GitEntry, error) {
	return append([]GitEntry(nil), f.entries...), nil
}
func (f *snapshotFixture) IsAncestor(context.Context, domain.RepositoryHead, domain.RepositoryHead) (bool, error) {
	return false, nil
}
func (f *snapshotFixture) OpenBlob(ctx context.Context, id BlobID) (io.ReadCloser, error) {
	f.mu.Lock()
	f.calls[id]++
	f.active++
	if f.active > f.peak {
		f.peak = f.active
	}
	var barrier chan struct{}
	if id != "manifest" && f.barrier != nil {
		barrier = f.barrier
		if f.active == 8 {
			close(f.barrier)
			f.barrier = nil
		}
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if barrier != nil {
		select {
		case <-barrier:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.delay):
	}
	b, ok := f.blobs[id]
	if !ok {
		return nil, fmt.Errorf("missing blob %s", id)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func newSnapshotFixture(t *testing.T, n int) *snapshotFixture {
	t.Helper()
	f := &snapshotFixture{blobs: map[BlobID][]byte{}, calls: map[BlobID]int{}, delay: 10 * time.Millisecond}
	id, e := domain.NewRepositoryID()
	if e != nil {
		t.Fatal(e)
	}
	manifest, e := EncodeManifest(domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"})
	if e != nil {
		t.Fatal(e)
	}
	f.blobs["manifest"] = manifest
	f.entries = append(f.entries, GitEntry{Path: ManifestPath, Type: "blob", Mode: "100644", Size: int64(len(manifest)), SHA: "manifest"})
	for i := range n {
		a := sampleAsset(t)
		b, e := EncodeAsset(a)
		if e != nil {
			t.Fatal(e)
		}
		sha := fmt.Sprintf("asset-%d", i)
		f.blobs[BlobID(sha)] = b
		f.entries = append(f.entries, GitEntry{Path: AssetPath(a.ID), Type: "blob", Mode: "100644", Size: int64(len(b)), SHA: sha})
		if i == 0 {
			f.entries = append(f.entries, GitEntry{Path: ObjectPath(a), Type: "blob", Mode: "100644", Size: a.Object.Size, SHA: "object"})
		}
	}
	return f
}
func TestSnapshotParallelReuseAndValidation(t *testing.T) {
	f := newSnapshotFixture(t, 16)
	f.barrier = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base, err := ReadSnapshot(ctx, f, "base")
	if err != nil {
		t.Fatal(err)
	}
	if f.peak != 8 {
		t.Fatalf("metadata peak concurrency %d, want 8", f.peak)
	}
	before := 0
	for _, v := range f.calls {
		before += v
	}
	snap, err := ReadSnapshotReusing(ctx, f, "candidate", base)
	if err != nil || len(snap.Assets) != 16 {
		t.Fatalf("reuse: %v", err)
	}
	after := 0
	for _, v := range f.calls {
		after += v
	}
	if before != after {
		t.Fatalf("unchanged metadata re-read: %d to %d", before, after)
	}
	f.entries = append([]GitEntry(nil), f.entries...)
	for i, e := range f.entries {
		if e.SHA == "object" {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			break
		}
	}
	if _, err = ReadSnapshotReusing(ctx, f, "candidate", base); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("missing object accepted: %v", err)
	}
}
func TestSnapshotCancellation(t *testing.T) {
	f := newSnapshotFixture(t, 16)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadSnapshot(ctx, f, "head"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSnapshotRejectsCorruptMetadataAndConflictingObjects(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		f := newSnapshotFixture(t, 3)
		f.blobs["asset-1"] = bytes.Repeat([]byte("x"), len(f.blobs["asset-1"]))
		if _, err := ReadSnapshot(context.Background(), f, "head"); !errors.Is(err, domain.ErrRepositoryCorrupt) {
			t.Fatalf("corrupt metadata accepted: %v", err)
		}
	})
	t.Run("conflicting object", func(t *testing.T) {
		f := newSnapshotFixture(t, 2)
		asset, err := DecodeAsset(f.blobs["asset-1"], strings.TrimPrefix(f.entries[3].Path, ".touko/assets/"))
		if err != nil {
			t.Fatal(err)
		}
		asset.Object.Width++
		data, err := EncodeAsset(asset)
		if err != nil {
			t.Fatal(err)
		}
		f.blobs["asset-1"] = data
		f.entries[3].Size = int64(len(data))
		if _, err := ReadSnapshot(context.Background(), f, "head"); !errors.Is(err, domain.ErrRepositoryCorrupt) {
			t.Fatalf("conflicting object accepted: %v", err)
		}
	})
}
