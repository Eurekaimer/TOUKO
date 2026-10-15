package app

import (
	"context"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func (s *Service) Verify(ctx context.Context, r repository.Git) (repository.VerifyResult, error) {
	head, err := r.Head(ctx)
	if err != nil {
		return repository.VerifyResult{}, err
	}
	snapshot, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		return repository.VerifyResult{}, err
	}
	return repository.VerifySnapshot(ctx, r, snapshot)
}
