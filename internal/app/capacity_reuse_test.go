package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

type capacityCountingGit struct {
	repository.Git
	blobs atomic.Int64
	heads atomic.Int64
}

type capacityValueReader struct {
	repository.Git
	marker any
}

func (g *capacityCountingGit) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	g.blobs.Add(1)
	return g.Git.OpenBlob(ctx, id)
}
func (g *capacityCountingGit) Head(ctx context.Context) (domain.RepositoryHead, error) {
	g.heads.Add(1)
	return g.Git.Head(ctx)
}
func TestPreparedCapacityReusesValidatedSnapshot(t *testing.T) {
	ctx, git, s := realRepository(t)
	stageImage(t, ctx, s)
	mustCommit(t, ctx, s, git)
	reader := &capacityCountingGit{Git: git}
	preview, err := s.PreviewPrepared(ctx, reader, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := reader.blobs.Load()
	if before < 2 {
		t.Fatalf("preview did not read metadata: %d", before)
	}
	item := preparedPng(t, "new.png")
	if _, err = s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: reader, Preview: &preview}); err != nil {
		t.Fatal(err)
	}
	if got := reader.blobs.Load(); got != before {
		t.Fatalf("unchanged metadata fetched again: %d -> %d", before, got)
	}
	if reader.heads.Load() != 2 {
		t.Fatalf("expected a fresh HEAD check before staging, got %d", reader.heads.Load())
	}
}
func TestPreparedCapacityPreviewRechecksStateAndHead(t *testing.T) {
	t.Run("changed staging still obeys capacity", func(t *testing.T) {
		ctx, git, s := realRepository(t)
		item := preparedPng(t, "second.png")
		preview, err := s.PreviewPrepared(ctx, git, []PreparedInput{item}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "first.png"}}, AddOptions{Namespace: "other"}); err != nil {
			t.Fatal(err)
		}
		_, err = s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: git, MaxBytes: preview.ProjectedBytes, Preview: &preview})
		if !errors.Is(err, ErrCapacityFull) {
			t.Fatalf("capacity was not recalculated: %v", err)
		}
		st, e := s.Store.Load(ctx)
		if e != nil || len(st.Changes) != 1 {
			t.Fatalf("earlier staging changed: %v", e)
		}
	})
	t.Run("changed head retains staging", func(t *testing.T) {
		ctx, git, s := realRepository(t)
		reader := &capacityCountingGit{Git: git}
		preview, err := s.PreviewPrepared(ctx, reader, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		item := preparedPng(t, "second.png")
		if _, err = s.Add(ctx, []Input{{Reader: bytes.NewReader(pngBytes(t)), Name: "first.png"}}, AddOptions{Namespace: "default"}); err != nil {
			t.Fatal(err)
		}
		publishRaw(t, ctx, git, nil)
		_, err = s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: reader, Preview: &preview})
		var conflict *domain.ConflictError
		if !errors.As(err, &conflict) || conflict.Reason != "head_changed" {
			t.Fatalf("changed HEAD accepted: %v", err)
		}
		st, e := s.Store.Load(ctx)
		if e != nil || len(st.Changes) != 1 {
			t.Fatalf("earlier staging lost: %v", e)
		}
		if _, e = os.Stat(item.Payload.TempPath); e != nil {
			t.Fatalf("rejected payload lost: %v", e)
		}
	})
	t.Run("different reader does not reuse preview", func(t *testing.T) {
		ctx, git, s := realRepository(t)
		one := &capacityCountingGit{Git: git}
		preview, err := s.PreviewPrepared(ctx, one, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		two := &capacityCountingGit{Git: git}
		item := preparedPng(t, "new.png")
		if _, err = s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: two, Preview: &preview}); err != nil {
			t.Fatal(err)
		}
		if two.blobs.Load() == 0 {
			t.Fatal("snapshot from a different reader was trusted")
		}
	})
	t.Run("reader with noncomparable fields is not cached", func(t *testing.T) {
		ctx, git, s := realRepository(t)
		counted := &capacityCountingGit{Git: git}
		reader := capacityValueReader{Git: counted, marker: []string{"not-comparable"}}
		preview, err := s.PreviewPrepared(ctx, reader, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		before := counted.blobs.Load()
		item := preparedPng(t, "new.png")
		if _, err = s.AddPrepared(ctx, []PreparedInput{item}, PreparedOptions{Reader: reader, Preview: &preview}); err != nil {
			t.Fatal(err)
		}
		if counted.blobs.Load() <= before {
			t.Fatal("noncomparable reader was incorrectly reused")
		}
	})
}
