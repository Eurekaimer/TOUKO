package webview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

func (s *Server) preview(w http.ResponseWriter, r *http.Request, selector string) {
	ctx := r.Context()
	ifNoneMatch := r.Header.Get("If-None-Match")
	var tag string
	var notModified bool
	st, asset, source, err := s.imageSnapshot(ctx, selector, func(st index.State, asset *domain.Asset, staged bool) bool {
		switch blob := st.ImageBlobs[asset.ID]; {
		case !staged && blob != "":
			tag = `"git-` + string(blob) + `"`
		case staged && asset.Object.HasHash():
			tag = `"staged-` + asset.Object.Hash.String() + `"`
		case staged:
			tag = `"staged-` + selector + `"`
		case asset.Object.HasHash():
			tag = `"sha256-` + asset.Object.Hash.String() + `"`
		default:
			tag = `"asset-` + selector + "-" + string(st.Head) + `"`
		}
		notModified = matchesETag(ifNoneMatch, tag)
		return notModified
	})
	if err != nil {
		s.respondError(w, err)
		return
	}
	if !notModified {
		if source == nil {
			source, err = s.openCommittedImage(ctx, st, asset)
			if err != nil {
				s.respondError(w, err)
				return
			}
		}
		defer source.Close()
	}
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if notModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	object := asset.Object
	w.Header().Set("Content-Type", object.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	w.Header().Set("Content-Disposition", "inline")
	// Neither redirects nor live branch URLs are involved, including for private
	// GitHub repositories. Backpressure and cancellation reach the blob stream.
	if _, err = io.CopyN(w, contextReader{ctx, source}, object.Size); err != nil {
		// Headers already describe an image. Abort rather than append JSON to bytes
		// or turn a truncated upstream stream into an apparently successful image.
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) openCommittedImage(ctx context.Context, st index.State, asset *domain.Asset) (io.ReadCloser, error) {
	location, err := s.service.Location(ctx)
	if err != nil {
		return nil, err
	}
	if location.Backend == "github" {
		local, dir, localErr := s.localPreviewRepo(ctx, location)
		if localErr == nil {
			source, openErr := s.openLocalCommitted(ctx, local, dir, st, asset)
			if openErr == nil {
				return source, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	git, err := s.backend(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := s.indexedImageBlob(ctx, git, st, asset)
	if err != nil {
		return nil, err
	}
	return git.OpenBlob(ctx, blob)
}

// localPreviewRepo never fetches or creates a clone. Sync owns network refresh.
func (s *Server) localPreviewRepo(ctx context.Context, location repository.Location) (*localgit.Repository, string, error) {
	dir := location.Path
	if location.Backend == "github" {
		var err error
		dir, err = s.service.CloneDir()
		if err != nil {
			return nil, "", err
		}
	}
	local, err := localgit.Open(ctx, dir, location.Branch)
	return local, dir, err
}

func (s *Server) openLocalCommitted(ctx context.Context, local *localgit.Repository, dir string, st index.State, asset *domain.Asset) (io.ReadCloser, error) {
	blob, err := s.indexedImageBlob(ctx, local, st, asset)
	if err == nil && local.HasObject(ctx, string(blob)) {
		return local.OpenBlob(ctx, blob)
	}
	// Old indexes lack blob IDs; if the clone is behind, match an asset
	// recorded with the same digest in its immutable local snapshot.
	if st.ImageBlobs[asset.ID] == "" && asset.Object.HasHash() && st.Manifest != nil {
		if old := s.legacyImageBlob(ctx, local, dir, st.Manifest.ID, asset); old != "" {
			return local.OpenBlob(ctx, old)
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, os.ErrNotExist
}

type legacyImageCache struct {
	mu           sync.Mutex
	dir          string
	head         domain.RepositoryHead
	repositoryID domain.RepositoryID
	assets       map[domain.AssetID]domain.Asset
	blobs        map[domain.AssetID]repository.BlobID
}

func (s *Server) legacyImageBlob(ctx context.Context, local *localgit.Repository, dir string, repositoryID domain.RepositoryID, asset *domain.Asset) repository.BlobID {
	head, err := local.Head(ctx)
	if err != nil || head == "" {
		return ""
	}
	cache := &s.legacyImages
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.dir != dir || cache.head != head {
		snapshot, err := repository.ReadSnapshot(ctx, local, head)
		if err != nil {
			return ""
		}
		cache.dir, cache.head = dir, head
		cache.repositoryID, cache.assets, cache.blobs = snapshot.Manifest.ID, snapshot.Assets, snapshot.ImageBlobs()
	}
	if old, ok := cache.assets[asset.ID]; ok && cache.repositoryID == repositoryID && old.Object == asset.Object {
		return cache.blobs[asset.ID]
	}
	return ""
}

func (s *Server) indexedImageBlob(ctx context.Context, git repository.Reader, st index.State, asset *domain.Asset) (repository.BlobID, error) {
	if blob := st.ImageBlobs[asset.ID]; blob != "" {
		return blob, nil
	}
	entry, err := s.imageBlob(ctx, git, st, repository.ObjectPath(*asset))
	if err != nil {
		return "", err
	}
	if entry.Size != asset.Object.Size {
		return "", fmt.Errorf("%w: cached image size differs", domain.ErrRepositoryCorrupt)
	}
	return repository.BlobID(entry.SHA), nil
}

// imageSnapshot revalidates metadata before opening any staged payload. On a
// miss it pins that file under workMu so concurrent cleanup cannot unlink it
// before open. For committed assets, immutable Git IDs outlive the lock.
func (s *Server) imageSnapshot(ctx context.Context, selector string, revalidate func(index.State, *domain.Asset, bool) bool) (index.State, *domain.Asset, io.ReadCloser, error) {
	var empty index.State
	st, views, err := s.service.ReadView(ctx)
	if err != nil {
		return empty, nil, nil, err
	}
	found := false
	for i := range views {
		view := &views[i]
		if view.Asset.ID.String() == selector {
			staged := view.State == "added"
			if revalidate(st, &view.Asset, staged) || !staged {
				return st, &view.Asset, nil, nil
			}
			found = true
			break
		}
	}
	if !found {
		return empty, nil, nil, domain.ErrAssetNotFound
	}
	// Only staged payloads need protection from concurrent cleanup.
	s.workMu.Lock()
	defer s.workMu.Unlock()
	st, views, err = s.service.ReadView(ctx)
	if err != nil {
		return empty, nil, nil, err
	}
	for i := range views {
		view := &views[i]
		if view.Asset.ID.String() != selector {
			continue
		}
		staged := view.State == "added"
		if revalidate(st, &view.Asset, staged) || !staged {
			return st, &view.Asset, nil, nil
		}
		payload := staging.PayloadPath(s.service.Dir, view.Asset)
		file, err := os.Open(payload)
		if errors.Is(err, os.ErrNotExist) {
			// A publication may have consumed the payload between the snapshot and
			// this open. Re-read once: the asset is either committed now, in which
			// case its bytes come from Git, or it is genuinely gone.
			if latest, again, readErr := s.service.ReadView(ctx); readErr == nil {
				for j := range again {
					candidate := &again[j]
					if candidate.Asset.ID.String() != selector {
						continue
					}
					if candidate.State != "added" {
						revalidate(latest, &candidate.Asset, false)
						return latest, &candidate.Asset, nil, nil
					}
					break
				}
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && view.Asset.Path != "" {
				// A legacy import references committed bytes without a staging payload.
				return st, &view.Asset, nil, nil
			}
			return empty, nil, nil, fmt.Errorf("%w: staged image unavailable: %v", domain.ErrRepositoryCorrupt, err)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return empty, nil, nil, err
		}
		if !info.Mode().IsRegular() || info.Size() != view.Asset.Object.Size {
			file.Close()
			return empty, nil, nil, fmt.Errorf("%w: staged image size", domain.ErrHashMismatch)
		}
		return st, &view.Asset, file, nil
	}
	return empty, nil, nil, domain.ErrAssetNotFound
}

func (s *Server) imageBlob(ctx context.Context, git repository.Reader, st index.State, path string) (repository.GitEntry, error) {
	s.cacheMu.Lock()
	entry, known := s.cachedBlobs[path]
	hit := s.cachedTree == repository.TreeID(st.Tree) && known
	s.cacheMu.Unlock()
	if err := ctx.Err(); err != nil {
		return repository.GitEntry{}, err
	}
	tree := repository.TreeID(st.Tree)
	if tree == "" {
		return repository.GitEntry{}, domain.ErrRepositoryUninitialized
	}
	if !hit {
		entries, err := git.Tree(ctx, tree)
		if err != nil {
			return repository.GitEntry{}, err
		}
		wanted := make(map[string]repository.GitEntry, len(st.Assets)+len(st.Changes))
		for _, asset := range st.Assets {
			wanted[repository.ObjectPath(asset)] = repository.GitEntry{}
		}
		for _, change := range st.Changes {
			if change.Asset != nil {
				wanted[repository.ObjectPath(*change.Asset)] = repository.GitEntry{}
			}
		}
		for _, entry := range entries {
			if _, ok := wanted[entry.Path]; ok {
				wanted[entry.Path] = entry
			}
		}
		// Retain only one tree's referenced placements, never image payloads or
		// unrelated files from the repository. Replace the entire old cache.
		entry = wanted[path]
		s.cacheMu.Lock()
		s.cachedTree, s.cachedBlobs = tree, wanted
		s.cacheMu.Unlock()
	}
	if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.SHA == "" {
		return repository.GitEntry{}, fmt.Errorf("%w: immutable image placement missing", domain.ErrRepositoryCorrupt)
	}
	return entry, nil
}
