package app

import (
	"context"

	"github.com/Eurekaimer/TOUKO/internal/index"
)

// ReadView derives the visible gallery and its staging information from one
// transactional database snapshot. Callers coordinating external files should
// hold Service.Lock until they have opened those files.
func (s *Service) ReadView(ctx context.Context) (index.State, []AssetView, error) {
	state, err := s.Store.Load(ctx)
	if err != nil {
		return index.State{}, nil, err
	}
	return state, overlay(state), nil
}
