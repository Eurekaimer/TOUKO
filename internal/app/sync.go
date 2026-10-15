package app

import (
	"context"
	"fmt"

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
		return s.recoverPublication(ctx, r, st, false)
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
