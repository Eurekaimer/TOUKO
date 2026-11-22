package webview

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/localgit"
)

// cloneOnce guards the one-time clone per process so concurrent previews cannot
// start competing clones of the same repository.
type cloneOnce struct {
	mu   sync.Mutex
	done map[string]bool
}

// ensureClone gives the interface a working copy to read from, cloning only when
// there is none yet.
//
// It deliberately does NOT fetch. A fetch here would put the network back on the
// path of every single preview, and a machine with no connection would fail to
// show pictures it already holds. Updating the copy belongs to `sync`, which is
// the user's explicit "talk to the remote" action; previews stay local.
func (s *Server) ensureClone(ctx context.Context) error {
	location, err := s.service.Location(ctx)
	if err != nil {
		return err
	}
	if location.Backend != "github" {
		return nil
	}
	key := location.Owner + "/" + location.Name + "@" + location.Branch
	s.clone.mu.Lock()
	defer s.clone.mu.Unlock()
	if s.clone.done == nil {
		s.clone.done = map[string]bool{}
	}
	if s.clone.done[key] {
		return nil
	}
	dir, err := s.service.CloneDir()
	if err != nil {
		return err
	}
	// An existing working copy is enough to serve previews, whatever the network
	// is doing; only a missing one needs the remote.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		s.clone.done[key] = true
		return nil
	}
	token, _, err := s.credentials(ctx, false)
	if err != nil {
		return err
	}
	if err := localgit.Clone(ctx, location.RemoteURL(), dir, location.Branch, token); err != nil {
		return err
	}
	s.clone.done[key] = true
	return nil
}

// updateClone refreshes the working copy from the remote. Callers use it where
// talking to the remote is already expected, such as sync.
func (s *Server) updateClone(ctx context.Context) error {
	location, err := s.service.Location(ctx)
	if err != nil || location.Backend != "github" {
		return err
	}
	s.clone.mu.Lock()
	defer s.clone.mu.Unlock()
	dir, err := s.service.CloneDir()
	if err != nil {
		return err
	}
	token, _, err := s.credentials(ctx, false)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, fs.ErrNotExist) {
		return localgit.Clone(ctx, location.RemoteURL(), dir, location.Branch, token)
	} else if err != nil {
		return err
	}
	return localgit.Update(ctx, dir, location.Branch, token)
}

// CloneStatus reports where the working copy lives, for the interface to explain
// that pictures are read from disk.
func (s *Server) CloneStatus(ctx context.Context) (bool, string) {
	info, err := s.cloneInfo(ctx)
	if err != nil {
		return false, ""
	}
	return info.Available, info.Path
}
