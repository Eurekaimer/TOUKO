package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/gofrs/flock"
)

type Service struct {
	Dir   string
	Store *index.Store
	// NewCloudflare builds the mirror from an already resolved workspace
	// credential pair. Tests inject a fake; production uses NewCloudflareMirror.
	NewCloudflare func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error)
	// VerifyURL reads a published public target and proves its bytes. Tests
	// inject a fake; production uses an HTTP GET with size/hash or header checks.
	VerifyURL func(context.Context, string, domain.ContentObject) error
}

func StateDir(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if p := os.Getenv("TOUKO_HOME"); p != "" {
		return filepath.Abs(p)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Abs(filepath.Join(base, "touko"))
}
func Open(ctx context.Context, dir string) (*Service, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	store, err := index.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	return &Service{Dir: dir, Store: store}, nil
}
func (s *Service) Close() error { return s.Store.Close() }
func (s *Service) Lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	l := flock.New(path)
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, domain.ErrBusy
	}
	return func() { _ = l.Unlock() }, nil
}
func (s *Service) Location(ctx context.Context) (repository.Location, error) {
	var l repository.Location
	if err := ctx.Err(); err != nil {
		return l, err
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return l, fmt.Errorf("no repository selected; run repo init --local PATH or repo use")
	}
	if err != nil {
		return l, err
	}
	if err = json.Unmarshal(b, &l); err != nil {
		return l, err
	}
	return l, l.Validate()
}
func (s *Service) Select(ctx context.Context, l repository.Location) error {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.selectLocked(ctx, l)
}
func (s *Service) selectLocked(ctx context.Context, l repository.Location) error {
	if err := l.Validate(); err != nil {
		return err
	}
	old, err := s.Location(ctx)
	if err == nil {
		if old == l {
			return nil
		}
		return fmt.Errorf("%w: different repository requires another state-dir", domain.ErrConflict)
	}
	if _, e := os.Stat(filepath.Join(s.Dir, "config.json")); !errors.Is(e, os.ErrNotExist) {
		return err
	}
	st, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	if st.Manifest != nil || len(st.Changes) > 0 || st.Publication != nil {
		return fmt.Errorf("%w: existing cache requires another state-dir", domain.ErrConflict)
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, "config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, filepath.Join(s.Dir, "config.json"))
}
