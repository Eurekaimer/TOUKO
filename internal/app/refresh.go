package app

import (
	"context"

	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// SyncWith holds the workspace lock while a transport refreshes its local read
// source. The callback must use the authoritative backend for recovery: a stale
// or shallow clone cannot establish whether a journaled publication succeeded.
func (s *Service) SyncWith(ctx context.Context, prepare func(context.Context, bool) (repository.Git, error)) (SyncResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	state, err := s.Store.Load(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	git, err := prepare(ctx, state.Publication != nil)
	if err != nil {
		return SyncResult{}, err
	}
	result, err := s.syncLocked(ctx, git)
	if err != nil || state.Publication == nil {
		return result, err
	}
	// Recovery is now durable. Refresh the checkout and committed cache while
	// still holding the same lock, so a second machine cannot interleave staging
	// between recovery and the normal pull.
	git, err = prepare(ctx, false)
	if err != nil {
		return result, err
	}
	refreshed, err := s.syncLocked(ctx, git)
	refreshed.Cloudflare = result.Cloudflare
	return refreshed, err
}
