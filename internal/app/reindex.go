package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// Reindex can recover a deleted or physically corrupt cache without app.Open.
// It never discards unpublished work or uses a clone to resolve a publication
// journal. After rebuilding, Sync must still recover that journal via its API backend.
func Reindex(ctx context.Context, dir string, r repository.Git) (SyncResult, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return SyncResult{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return SyncResult{}, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return SyncResult{}, err
	}
	s := &Service{Dir: dir}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	st, err := index.LoadWorkspace(ctx, dir)
	if errors.Is(err, os.ErrNotExist) {
		// A healthy schema1 database must migrate before it is disposable. If
		// its only copy of staged work is unreadable, refuse rather than guess.
		store, openErr := index.Open(ctx, dir)
		if openErr != nil {
			return SyncResult{}, fmt.Errorf("cannot recover durable work from legacy index; preserve index.db: %w", openErr)
		}
		if err := store.Close(); err != nil {
			return SyncResult{}, err
		}
		st, err = index.LoadWorkspace(ctx, dir)
	}
	if err != nil {
		return SyncResult{}, err
	}
	next, result, err := reindexState(ctx, r, st)
	if err != nil {
		return SyncResult{}, err
	}
	if err := index.Rebuild(ctx, dir, next); err != nil {
		return SyncResult{}, err
	}
	return result, nil
}

// Reindex rebuilds committed metadata inside this service's existing SQLite
// connection. Running servers must use this method, not replace an open DB file.
func (s *Service) Reindex(ctx context.Context, r repository.Git) (SyncResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	st, err := index.LoadWorkspace(ctx, s.Dir)
	if err != nil {
		return SyncResult{}, err
	}
	next, result, err := reindexState(ctx, r, st)
	if err != nil {
		return SyncResult{}, err
	}
	if err := s.Store.Save(ctx, next); err != nil {
		return SyncResult{}, err
	}
	return result, nil
}

func reindexState(ctx context.Context, r repository.Git, st index.State) (index.State, SyncResult, error) {
	pendingInitialization := st.Publication != nil && st.Manifest == nil && len(st.Changes) == 0
	head, err := r.Head(ctx)
	if err != nil {
		return index.State{}, SyncResult{}, err
	}
	if st.Publication == nil && len(st.Changes) > 0 && head != st.BaseHead {
		return index.State{}, SyncResult{}, &domain.ConflictError{Base: st.BaseHead, Remote: head, Reason: "head_changed"}
	}
	// All reads below are pinned to this immutable HEAD. Validate the complete
	// snapshot, including payload hashes, before replacing any cached metadata.
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if pendingInitialization && errors.Is(err, domain.ErrRepositoryUninitialized) {
		return pendingInitializationState(st), SyncResult{}, nil
	}
	if err != nil {
		return index.State{}, SyncResult{}, err
	}
	if err := sameRepository(st, snap); err != nil {
		return index.State{}, SyncResult{}, err
	}
	if _, err := repository.VerifySnapshot(ctx, r, snap); err != nil {
		return index.State{}, SyncResult{}, err
	}
	if pendingInitialization {
		// The journal, not the visible branch, decides whether initialization
		// finished. Keep the original uninitialized cache representation so
		// validateCandidate still follows its existing initialization rules.
		return pendingInitializationState(st), SyncResult{}, nil
	}
	return snapshotState(st, snap, false), SyncResult{Head: head, AssetCount: len(snap.Assets)}, nil
}

func pendingInitializationState(st index.State) index.State {
	st.Head, st.Tree, st.Manifest = "", "", nil
	st.Assets = make(map[domain.AssetID]domain.Asset)
	return st
}
