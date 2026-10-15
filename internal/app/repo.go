package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func reservedPath(path string) bool {
	return path == ".touko" || strings.HasPrefix(path, ".touko/") || path == "objects" || strings.HasPrefix(path, "objects/")
}
func (s *Service) Initialize(ctx context.Context, r repository.Git) (SyncResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if st.Publication != nil {
		return s.recoverPublication(ctx, r, st, true)
	}
	head, err := r.Head(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if head != "" {
		_, err = repository.ReadSnapshot(ctx, r, head)
		if err == nil {
			return s.syncLocked(ctx, r)
		}
		if !errors.Is(err, domain.ErrRepositoryUninitialized) {
			return SyncResult{}, err
		}
	}
	if st.Manifest != nil || len(st.Changes) > 0 {
		return SyncResult{}, fmt.Errorf("%w: cannot initialize over existing local snapshot", domain.ErrConflict)
	}
	var base repository.TreeID
	if head != "" {
		commit, err := r.Commit(ctx, head)
		if err != nil {
			return SyncResult{}, err
		}
		base = commit.Tree
		entries, err := r.Tree(ctx, base)
		if err != nil {
			return SyncResult{}, err
		}
		for _, entry := range entries {
			if reservedPath(entry.Path) {
				return SyncResult{}, fmt.Errorf("%w: reserved .touko/ or objects/ path already occupied", domain.ErrRepositoryCorrupt)
			}
		}
	}
	id, err := domain.NewRepositoryID()
	if err != nil {
		return SyncResult{}, err
	}
	data, err := repository.EncodeManifest(domain.Repository{ID: id, FormatVersion: 1, HashAlgorithm: "sha256"})
	if err != nil {
		return SyncResult{}, err
	}
	blob, err := r.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return SyncResult{}, err
	}
	tree, err := r.CreateTree(ctx, base, []repository.TreeChange{{Path: repository.ManifestPath, Blob: &blob}})
	if err != nil {
		return SyncResult{}, err
	}
	candidate, err := r.CreateCommit(ctx, tree, head, "touko: initialize repository format v1")
	if err != nil {
		return SyncResult{}, err
	}
	st.Publication = &index.Publication{BaseHead: head, CandidateHead: candidate, CandidateTree: string(tree)}
	if err = s.Store.SavePublication(ctx, st.Publication); err != nil {
		return SyncResult{}, err
	}
	return s.publishJournal(ctx, r, st)
}

// Reset discards local unpublished work only after remote reads establish whether
// its journaled candidate was published. Published work is finalized, not lost.
func (s *Service) Reset(ctx context.Context, r repository.Git) error {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	if st.Publication != nil {
		_, published, err := publicationRelation(ctx, r, st.Publication)
		if err != nil {
			return unknownPublication(st.Publication, err)
		}
		if published {
			_, err = s.finishPublication(ctx, r, st)
			return err
		}
	}
	st.Changes = make(map[domain.AssetID]domain.Change)
	st.BaseHead = ""
	st.Publication = nil
	if err = s.Store.Save(ctx, st); err != nil {
		return err
	}
	return s.cleanup(ctx, st)
}
