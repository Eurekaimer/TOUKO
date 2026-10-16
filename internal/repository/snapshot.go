package repository

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

type Snapshot struct {
	Manifest domain.Repository
	Head     domain.RepositoryHead
	Tree     TreeID
	Assets   map[domain.AssetID]domain.Asset
	Entries  []GitEntry
}

func readMetadata(ctx context.Context, r Reader, entry GitEntry, limit int) ([]byte, error) {
	if entry.Type != "blob" || entry.Mode != "100644" || entry.Size < 0 || entry.Size > int64(limit) {
		return nil, corrupt(fmt.Errorf("invalid metadata entry %s", entry.Path))
	}
	body, err := r.OpenBlob(ctx, BlobID(entry.SHA))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	closeErr := body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(b) > limit || int64(len(b)) != entry.Size {
		return nil, corrupt(fmt.Errorf("invalid metadata length %s", entry.Path))
	}
	return b, nil
}
func ReadSnapshot(ctx context.Context, r Reader, head domain.RepositoryHead) (Snapshot, error) {
	s := Snapshot{Head: head, Assets: make(map[domain.AssetID]domain.Asset)}
	if head == "" {
		return s, domain.ErrRepositoryUninitialized
	}
	commit, err := r.Commit(ctx, head)
	if err != nil {
		return s, err
	}
	s.Tree = commit.Tree
	s.Entries, err = r.Tree(ctx, commit.Tree)
	if err != nil {
		return s, err
	}
	entries := make(map[string]GitEntry, len(s.Entries))
	partial := false
	for _, e := range s.Entries {
		if e.Path == "" || path.Clean(e.Path) != e.Path || strings.HasPrefix(e.Path, "/") || e.Path == ".." || strings.HasPrefix(e.Path, "../") || strings.ContainsRune(e.Path, 0) {
			return s, corrupt(fmt.Errorf("unsafe tree path %q", e.Path))
		}
		if _, ok := entries[e.Path]; ok {
			return s, corrupt(fmt.Errorf("duplicate tree path %s", e.Path))
		}
		entries[e.Path] = e
		if e.Path == ".touko" || strings.HasPrefix(e.Path, ".touko/") {
			partial = true
		}
	}
	manifest, ok := entries[ManifestPath]
	if !ok {
		if partial {
			return s, corrupt(fmt.Errorf("partial .touko without manifest"))
		}
		return s, domain.ErrRepositoryUninitialized
	}
	data, err := readMetadata(ctx, r, manifest, MaxManifestBytes)
	if err != nil {
		return s, err
	}
	s.Manifest, err = DecodeManifest(data)
	if err != nil {
		return s, err
	}
	descriptions := make(map[domain.ObjectHash]domain.ContentObject)
	pathDescriptions := make(map[string]domain.ContentObject)
	for _, entry := range s.Entries {
		if !strings.HasPrefix(entry.Path, ".touko/assets/") {
			continue
		}
		if entry.Type == "tree" {
			return s, corrupt(fmt.Errorf("nested metadata directory %s", entry.Path))
		}
		data, err := readMetadata(ctx, r, entry, MaxMetadataBytes)
		if err != nil {
			return s, err
		}
		a, err := DecodeAsset(data, strings.TrimPrefix(entry.Path, ".touko/assets/"))
		if err != nil {
			return s, err
		}
		if s.Manifest.FormatVersion == 1 && a.Path != "" {
			return s, corrupt(fmt.Errorf("explicit asset path requires repository format v2"))
		}
		if _, ok := s.Assets[a.ID]; ok {
			return s, corrupt(fmt.Errorf("duplicate asset %s", a.ID))
		}
		if previous, ok := descriptions[a.Object.Hash]; ok && previous != a.Object {
			return s, corrupt(fmt.Errorf("contradictory object description %s", a.Object.Hash))
		}
		descriptions[a.Object.Hash] = a.Object
		objectPath := ObjectPath(a)
		if previous, ok := pathDescriptions[objectPath]; ok && previous != a.Object {
			return s, corrupt(fmt.Errorf("contradictory object descriptions for path %s", objectPath))
		}
		pathDescriptions[objectPath] = a.Object
		e, ok := entries[objectPath]
		if !ok || e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") || e.Size != a.Object.Size {
			return s, corrupt(fmt.Errorf("missing or invalid referenced object %s", objectPath))
		}
		s.Assets[a.ID] = a
	}
	return s, nil
}
