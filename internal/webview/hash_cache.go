package webview

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/atomicfile"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/staging"
	"github.com/gofrs/flock"
)

type hashScan struct {
	mu      sync.Mutex
	running bool
	pending *hashBatch
	err     error
}

type hashBatch struct {
	state index.State
	views []app.AssetView
}

type hashRow struct {
	ID    domain.AssetID    `json:"id"`
	Title string            `json:"title"`
	Hash  domain.ObjectHash `json:"hash"`
}

type hashReport struct {
	Enabled bool      `json:"enabled"`
	Running bool      `json:"running"`
	Total   int       `json:"total"`
	Assets  []hashRow `json:"assets"`
	Error   string    `json:"error,omitempty"`
}

func (s *Server) hashOptInPath() string {
	dir := s.service.Dir
	if s.spaces != nil {
		dir = s.spaces.root.service.Dir
	}
	return filepath.Join(dir, "hash-cache.enabled")
}

func (s *Server) hashOptedIn() (bool, error) {
	_, err := os.Stat(s.hashOptInPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *Server) hashCachePath() string { return filepath.Join(s.service.Dir, "hash-cache.json") }

func (s *Server) loadHashes() (map[string]domain.ObjectHash, error) {
	data, err := os.ReadFile(s.hashCachePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries map[string]domain.ObjectHash
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func hashKey(st index.State, view *app.AssetView) string {
	asset := &view.Asset
	if view.State == "added" {
		return "stage:" + asset.ID.String()
	}
	if blob := st.ImageBlobs[asset.ID]; blob != "" {
		return "git:" + string(blob)
	}
	return "snapshot:" + st.Tree + ":" + asset.ID.String()
}

func (s *Server) queueHashes(st index.State, views []app.AssetView) {
	if enabled, _ := s.hashOptedIn(); !enabled || len(views) == 0 {
		return
	}
	job := &s.hashes
	job.mu.Lock()
	defer job.mu.Unlock()
	job.pending = &hashBatch{st, views}
	if job.running {
		return
	}
	job.running, job.err = true, nil
	go s.runHashQueue()
}

func (s *Server) runHashQueue() {
	job := &s.hashes
	job.mu.Lock()
	for job.pending != nil {
		batch := job.pending
		job.pending = nil
		job.mu.Unlock()
		err := s.archiveHashes(context.Background(), batch)
		job.mu.Lock()
		job.err = err
	}
	job.running = false
	job.mu.Unlock()
}

func (s *Server) archiveHashes(ctx context.Context, batch *hashBatch) error {
	entries, err := s.loadHashes()
	if err != nil {
		return err
	}
	var updates map[string]domain.ObjectHash
	var local *localgit.Repository
	var dir string
	openedLocal := false
	for i := range batch.views {
		if enabled, err := s.hashOptedIn(); err != nil || !enabled {
			return err
		}
		view := &batch.views[i]
		asset := &view.Asset
		key := hashKey(batch.state, view)
		if entries[key] != (domain.ObjectHash{}) || updates[key] != (domain.ObjectHash{}) {
			continue
		}
		sum := asset.Object.Hash
		if !asset.Object.HasHash() {
			var source io.ReadCloser
			if view.State == "added" {
				source, err = os.Open(staging.PayloadPath(s.service.Dir, *asset))
			} else {
				if !openedLocal {
					openedLocal = true
					if location, err := s.service.Location(ctx); err == nil {
						local, dir, _ = s.localPreviewRepo(ctx, location)
					}
				}
				if local == nil {
					continue
				}
				source, err = s.openLocalCommitted(ctx, local, dir, batch.state, asset)
			}
			if err != nil {
				continue
			}
			digest := sha256.New()
			n, err := io.Copy(digest, io.LimitReader(source, asset.Object.Size+1))
			if closeErr := source.Close(); err != nil || closeErr != nil || n != asset.Object.Size {
				continue
			}
			digest.Sum(sum[:0])
		}
		if updates == nil {
			updates = make(map[string]domain.ObjectHash)
		}
		updates[key] = sum
	}
	if len(updates) == 0 {
		return nil
	}
	// Other desktop windows can share the same state directory.
	lock := flock.New(s.hashCachePath() + ".lock")
	if _, err := lock.TryLockContext(ctx, 25*time.Millisecond); err != nil {
		return err
	}
	defer lock.Unlock()
	if enabled, err := s.hashOptedIn(); err != nil || !enabled {
		return err
	}
	entries, err = s.loadHashes()
	if err != nil {
		return err
	}
	if entries == nil {
		entries = updates
	} else {
		for key, sum := range updates {
			entries[key] = sum
		}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return atomicfile.Write(ctx, s.hashCachePath(), data, 0600)
}

func (s *Server) hashCacheAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scan := r.Method == http.MethodPost
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			s.respondError(w, err)
			return
		}
		if input.Enabled != nil {
			var err error
			if *input.Enabled {
				err = atomicfile.Write(ctx, s.hashOptInPath(), []byte("enabled\n"), 0600)
			} else if err = os.Remove(s.hashOptInPath()); errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err != nil {
				s.respondError(w, err)
				return
			}
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		s.fail(w, http.StatusMethodNotAllowed, "usage", "method not allowed")
		return
	}
	enabled, err := s.hashOptedIn()
	if err != nil {
		s.respondError(w, err)
		return
	}
	report := hashReport{Enabled: enabled}
	if enabled {
		st, views, err := s.service.ReadView(ctx)
		if err != nil {
			s.respondError(w, err)
			return
		}
		if scan {
			s.queueHashes(st, views)
		}
		job := &s.hashes
		job.mu.Lock()
		report.Running = job.running
		if job.err != nil {
			report.Error = job.err.Error()
		}
		job.mu.Unlock()
		entries, err := s.loadHashes()
		if err != nil {
			s.respondError(w, err)
			return
		}
		report.Total = len(views)
		for i := range views {
			v := &views[i]
			if sum := entries[hashKey(st, v)]; sum != (domain.ObjectHash{}) {
				report.Assets = append(report.Assets, hashRow{v.Asset.ID, v.Asset.Title, sum})
			}
		}
	}
	s.json(w, report)
}
