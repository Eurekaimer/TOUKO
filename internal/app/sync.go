package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

type SyncResult struct {
	Head       domain.RepositoryHead `json:"head"`
	AssetCount int                   `json:"asset_count"`
}

func sameRepository(st index.State, snap repository.Snapshot) error {
	if st.Manifest != nil && st.Manifest.ID != snap.Manifest.ID {
		return fmt.Errorf("%w: remote repository identity differs from cache; use another state-dir", domain.ErrConflict)
	}
	return nil
}
func snapshotState(st index.State, snap repository.Snapshot, clear bool) index.State {
	st.Head = snap.Head
	st.Tree = string(snap.Tree)
	m := snap.Manifest
	st.Manifest = &m
	st.Assets = snap.Assets
	if clear {
		st.Changes = make(map[domain.AssetID]domain.Change)
		st.BaseHead = ""
		st.Publication = nil
	}
	return st
}
func (s *Service) Sync(ctx context.Context, r repository.Git) (SyncResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	return s.syncLocked(ctx, r)
}
func (s *Service) syncLocked(ctx context.Context, r repository.Git) (SyncResult, error) {
	st, err := s.Store.Load(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if st.Publication != nil {
		return SyncResult{}, fmt.Errorf("%w: unresolved publication", domain.ErrConflict)
	}
	head, err := r.Head(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if len(st.Changes) > 0 && head != st.Head {
		return SyncResult{}, &domain.ConflictError{Base: st.BaseHead, Remote: head, Reason: "head_changed"}
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		return SyncResult{}, err
	}
	if err = sameRepository(st, snap); err != nil {
		return SyncResult{}, err
	}
	if err = s.Store.Save(ctx, snapshotState(st, snap, false)); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Head: head, AssetCount: len(snap.Assets)}, nil
}
func (s *Service) Initialize(ctx context.Context, r repository.Git) (SyncResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if st.Publication != nil {
		return SyncResult{}, fmt.Errorf("%w: unresolved publication", domain.ErrConflict)
	}
	head, err := r.Head(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if head != "" {
		_, err = repository.ReadSnapshot(ctx, r, head)
		if err == nil {
			return s.syncLocked(ctx, r)
		}
		if !errors.Is(err, domain.ErrRepositoryUninitialized) {
			return SyncResult{}, err
		}
	}
	if st.Manifest != nil || len(st.Changes) > 0 {
		return SyncResult{}, fmt.Errorf("%w: cannot initialize over existing local snapshot", domain.ErrConflict)
	}
	var base repository.TreeID
	if head != "" {
		commit, err := r.Commit(ctx, head)
		if err != nil {
			return SyncResult{}, err
		}
		base = commit.Tree
		entries, err := r.Tree(ctx, base)
		if err != nil {
			return SyncResult{}, err
		}
		for _, e := range entries {
			if e.Path == ".touko" || strings.HasPrefix(e.Path, ".touko/") || e.Path == "objects" || strings.HasPrefix(e.Path, "objects/") {
				return SyncResult{}, fmt.Errorf("%w: reserved .touko/ or objects/ path already occupied", domain.ErrRepositoryCorrupt)
			}
		}
	}
	id, err := domain.NewRepositoryID()
	if err != nil {
		return SyncResult{}, err
	}
	data, err := repository.EncodeManifest(domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"})
	if err != nil {
		return SyncResult{}, err
	}
	blob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return SyncResult{}, err
	}
	tree, err := r.CreateTree(ctx, base, []repository.TreeChange{{Path: repository.ManifestPath, Blob: &blob}})
	if err != nil {
		return SyncResult{}, err
	}
	candidate, err := r.CreateCommit(ctx, tree, head, "touko: initialize repository format v1")
	if err != nil {
		return SyncResult{}, err
	}
	if err = r.UpdateRef(ctx, head, candidate); err != nil {
		return SyncResult{}, err
	}
	return s.syncLocked(ctx, r)
}
