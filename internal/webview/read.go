package webview

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func (s *Server) reader(ctx context.Context) (repository.Git, error) {
	location, err := s.service.Location(ctx)
	if err != nil {
		return nil, err
	}
	if location.Backend != "github" {
		return s.backend(ctx)
	}
	if err := s.ensureClone(ctx); err != nil {
		return nil, err
	}
	return localgit.Open(ctx, repository.CloneDir(s.service.Dir), location.Branch)
}

// capacityReader uses the clone only when it contains the indexed HEAD.
// A missing or stale clone must not block routing against authoritative Git.
func (s *Server) capacityReader(ctx context.Context) (repository.Git, error) {
	location, err := s.service.Location(ctx)
	if err != nil {
		return nil, err
	}
	if location.Backend != "github" {
		return s.backend(ctx)
	}
	dir := repository.CloneDir(s.service.Dir)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.backend(ctx)
		}
		return nil, err
	}
	local, err := localgit.Open(ctx, dir, location.Branch)
	if err != nil {
		return nil, err
	}
	head, err := local.Head(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.service.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if head != state.Head {
		return s.backend(ctx)
	}
	return local, nil
}

func (s *Server) syncRepository(ctx context.Context) (app.SyncResult, error) {
	return s.service.SyncWith(ctx, func(ctx context.Context, recovering bool) (repository.Git, error) {
		if recovering {
			return s.backend(ctx)
		}
		if err := s.updateClone(ctx); err != nil {
			return nil, err
		}
		return s.reader(ctx)
	})
}
