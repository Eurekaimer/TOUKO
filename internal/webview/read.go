package webview

import (
	"context"

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
