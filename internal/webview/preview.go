package webview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

func (s *Server) preview(w http.ResponseWriter, r *http.Request, selector string) {
	source, object, err := s.openImage(r.Context(), selector)
	if err != nil {
		s.respondError(w, err)
		return
	}
	defer source.Close()
	w.Header().Set("Content-Type", object.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	w.Header().Set("Content-Disposition", "inline")
	// Neither redirects nor live branch URLs are involved, including for private
	// GitHub repositories. Backpressure and cancellation reach the blob stream.
	if _, err = io.CopyN(w, contextReader{r.Context(), source}, object.Size); err != nil {
		// Headers already describe an image. Abort rather than append JSON to bytes
		// or turn a truncated upstream stream into an apparently successful image.
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) openImage(ctx context.Context, selector string) (io.ReadCloser, domain.ContentObject, error) {
	st, asset, staged, err := s.imageSnapshot(ctx, selector)
	if err != nil {
		return nil, domain.ContentObject{}, err
	}
	if staged != nil {
		return staged, asset.Object, nil
	}
	location, err := s.service.Location(ctx)
	if err != nil {
		return nil, domain.ContentObject{}, err
	}
	var git repository.Git
	if location.Backend == "github" {
		if err := s.ensureClone(ctx); err != nil {
			return nil, domain.ContentObject{}, err
		}
		if file, ok, err := localImage(s.service.Dir, asset); err != nil {
			return nil, domain.ContentObject{}, err
		} else if ok {
			return file, asset.Object, nil
		}
		local, err := localgit.Open(ctx, repository.CloneDir(s.service.Dir), location.Branch)
		if err != nil {
			return nil, domain.ContentObject{}, err
		}
		// Explicit PicGo paths can be replaced in a later checkout. Use immutable
		// Git bytes from the gallery snapshot, not a same-sized live path.
		if _, err := local.Commit(ctx, st.Head); err == nil {
			git = local
		} else if ctx.Err() != nil {
			return nil, domain.ContentObject{}, ctx.Err()
		}
	}
	if git == nil {
		// Only a newly API-published snapshot missing from the checkout needs
		// remote bytes. Ordinary synced galleries, including PicGo, stay local.
		git, err = s.backend(ctx)
		if err != nil {
			return nil, domain.ContentObject{}, err
		}
	}
	blob, err := s.imageBlob(ctx, git, st, repository.ObjectPath(*asset))
	if err != nil {
		return nil, domain.ContentObject{}, err
	}
	if blob.Size != asset.Object.Size {
		return nil, domain.ContentObject{}, fmt.Errorf("%w: cached image size differs", domain.ErrRepositoryCorrupt)
	}
	source, err := git.OpenBlob(ctx, repository.BlobID(blob.SHA))
	if err != nil {
		return nil, domain.ContentObject{}, err
	}
	return source, asset.Object, nil
}

// imageSnapshot opens a staged file before a concurrent commit/reset can unlink
// it. For committed assets, immutable Git IDs outlive the lock and live branch.
func (s *Server) imageSnapshot(ctx context.Context, selector string) (index.State, *domain.Asset, io.ReadCloser, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	var empty index.State
	unlock, err := s.service.Lock(ctx)
	if err != nil {
		return empty, nil, nil, err
	}
	defer unlock()
	st, views, err := s.service.ReadView(ctx)
	if err != nil {
		return empty, nil, nil, err
	}
	for i := range views {
		view := &views[i]
		if view.Asset.ID.String() != selector {
			continue
		}
		if view.State != "added" {
			return st, &view.Asset, nil, nil
		}
		file, err := os.Open(staging.PayloadPath(s.service.Dir, view.Asset.Namespace, view.Asset.Object))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && view.Asset.Path != "" {
				// Imported metadata references an already committed image and has
				// no newly uploaded staging payload.
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
	defer s.cacheMu.Unlock()
	if err := ctx.Err(); err != nil {
		return repository.GitEntry{}, err
	}
	tree := repository.TreeID(st.Tree)
	if tree == "" {
		return repository.GitEntry{}, domain.ErrRepositoryUninitialized
	}
	_, known := s.cachedBlobs[path]
	if s.cachedTree != tree || !known {
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
		s.cachedTree, s.cachedBlobs = tree, wanted
	}
	entry, ok := s.cachedBlobs[path]
	if !ok || entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.SHA == "" {
		return repository.GitEntry{}, fmt.Errorf("%w: immutable image placement missing", domain.ErrRepositoryCorrupt)
	}
	return entry, nil
}
