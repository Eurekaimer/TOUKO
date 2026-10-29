package webview

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
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

// capacityReader uses local immutable objects when the indexed HEAD is present.
// For a stale clone it verifies the remote branch and fetches missing objects
// without changing the checkout. If Git transport is unavailable, the existing
// GitHub reader remains the authoritative fallback.
func (s *Server) capacityReader(ctx context.Context) (repository.Reader, error) {
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
	if head == state.Head {
		return local, nil
	}
	token, _, err := s.credentials(ctx, false)
	if err != nil {
		return nil, err
	}
	// A depth-1 fetch transfers only objects the clone does not already have,
	// usually one commit plus the new images, but a slow link must still be
	// abandoned for the authoritative API rather than wait forever. A parent
	// cancellation is always returned rather than retried.
	gitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	reader, err := gitCapacityReader(gitCtx, local, dir, location.RemoteURL(), location.Branch, token, state.Head)
	cancel()
	if err == nil {
		return reader, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, domain.ErrConflict) {
		return nil, err
	}
	return s.backend(ctx)
}

// gitCapacityReader keeps the originally verified HEAD for the preview, then
// checks the remote again under the staging lock before installing payloads.
func gitCapacityReader(ctx context.Context, local repository.Reader, dir, remote, branch, token string, expected domain.RepositoryHead) (repository.Reader, error) {
	head, err := localgit.RemoteHead(ctx, dir, remote, branch, token)
	if err != nil {
		return nil, err
	}
	if head != expected {
		return nil, &domain.ConflictError{Base: expected, Remote: head, Reason: "head_changed"}
	}
	if _, err := local.Commit(ctx, expected); err != nil {
		if err := localgit.FetchCommit(ctx, dir, remote, expected, token); err != nil {
			return nil, err
		}
	}
	return &remoteCapacityReader{Reader: local, dir: dir, remote: remote, branch: branch, token: token, initial: head}, nil
}

type remoteCapacityReader struct {
	repository.Reader
	dir, remote, branch, token string
	mu                         sync.Mutex
	initial                    domain.RepositoryHead
}

func (r *remoteCapacityReader) Head(ctx context.Context) (domain.RepositoryHead, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	head := r.initial
	r.initial = ""
	r.mu.Unlock()
	if head != "" {
		return head, nil
	}
	remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return localgit.RemoteHead(remoteCtx, r.dir, r.remote, r.branch, r.token)
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
