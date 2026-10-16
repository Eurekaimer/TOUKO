package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"io"
	"path"
	"strings"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	ingest "github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

type ImportOptions struct {
	Directory         string
	Namespace         domain.Namespace
	Tags, Collections []string
}

type ImportResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// ImportLegacy stages metadata for existing image files without installing or
// moving their bytes. All reads belong to one immutable repository snapshot;
// the only durable write occurs after the entire batch has been validated.
func (s *Service) ImportLegacy(ctx context.Context, r repository.Git, opts ImportOptions) (ImportResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	defer unlock()
	st, err := s.Store.Load(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	if st.Publication != nil {
		return ImportResult{}, fmt.Errorf("%w: unresolved publication; run sync", domain.ErrConflict)
	}
	ns, err := domain.ParseNamespace(string(opts.Namespace))
	if err != nil {
		return ImportResult{}, err
	}
	if opts.Directory != "" {
		if err := domain.ValidateRepositoryPath(opts.Directory); err != nil {
			return ImportResult{}, err
		}
	}
	head, err := r.Head(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if errors.Is(err, domain.ErrRepositoryUninitialized) {
		return ImportResult{}, fmt.Errorf("initialize the repository explicitly before importing legacy images: %w", err)
	}
	if err != nil {
		return ImportResult{}, err
	}
	if err := sameRepository(st, snap); err != nil {
		return ImportResult{}, err
	}
	if len(st.Changes) > 0 && head != st.Head {
		return ImportResult{}, &domain.ConflictError{Base: st.BaseHead, Remote: head, Reason: "head_changed"}
	}
	st = snapshotState(st, snap, false)
	managed := make(map[string]bool, len(st.Assets)+len(st.Changes))
	for _, asset := range st.Assets {
		managed[repository.ObjectPath(asset)] = true
	}
	for _, change := range st.Changes {
		if change.Asset != nil {
			managed[repository.ObjectPath(*change.Asset)] = true
		}
	}
	result := ImportResult{}
	for _, entry := range snap.Entries {
		if err := ctx.Err(); err != nil {
			return ImportResult{}, err
		}
		if entry.Type == "tree" || entry.Path == ".touko" || strings.HasPrefix(entry.Path, ".touko/") {
			continue
		}
		if opts.Directory != "" && !strings.HasPrefix(entry.Path, opts.Directory+"/") {
			continue
		}
		if managed[entry.Path] {
			result.Skipped++
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
			result.Skipped++
			continue
		}
		if err := domain.ValidateRepositoryPath(entry.Path); err != nil {
			return ImportResult{}, err
		}
		object, err := inspectLegacy(ctx, r, entry)
		if errors.Is(err, image.ErrFormat) {
			result.Skipped++
			continue
		}
		if err != nil {
			return ImportResult{}, fmt.Errorf("import %s: %w", entry.Path, err)
		}
		id, err := domain.NewAssetID()
		if err != nil {
			return ImportResult{}, err
		}
		now := time.Now().UTC()
		name := path.Base(entry.Path)
		asset := domain.Asset{ID: id, Namespace: ns, Path: entry.Path, Object: object, OriginalName: name, Title: name, Tags: normalize(opts.Tags), Collections: normalize(opts.Collections), CreatedAt: now, UpdatedAt: now}
		if err := asset.Validate(); err != nil {
			return ImportResult{}, err
		}
		st.Changes[id] = domain.Change{Kind: domain.Add, AssetID: id, Asset: &asset}
		managed[entry.Path] = true
		result.Imported++
	}
	if result.Imported == 0 {
		return result, nil
	}
	bindBase(&st)
	if err := s.Store.Save(ctx, st); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

func inspectLegacy(ctx context.Context, r repository.Reader, entry repository.GitEntry) (object domain.ContentObject, err error) {
	body, err := r.OpenBlob(ctx, repository.BlobID(entry.SHA))
	if err != nil {
		return object, err
	}
	defer func() {
		if closeErr := body.Close(); closeErr != nil {
			err = closeErr
		}
	}()
	counted := &legacyReader{ctx: ctx, source: body}
	sum := sha256.New()
	source := io.TeeReader(io.LimitReader(counted, ingest.MaxBytes+1), sum)
	config, format, err := image.DecodeConfig(source)
	if counted.err != nil {
		return object, counted.err
	}
	if err != nil {
		return object, err
	}
	switch format {
	case "png":
		object.Ext, object.MIME = "png", "image/png"
	case "jpeg":
		object.Ext, object.MIME = "jpg", "image/jpeg"
	case "gif":
		object.Ext, object.MIME = "gif", "image/gif"
	default:
		return object, image.ErrFormat
	}
	if _, err := io.Copy(io.Discard, source); err != nil {
		return object, err
	}
	object.Size, object.Width, object.Height = counted.size, config.Width, config.Height
	sum.Sum(object.Hash[:0])
	if object.Size != entry.Size {
		return object, fmt.Errorf("image size differs from Git tree or exceeds size limit")
	}
	return object, object.Validate()
}

type legacyReader struct {
	ctx    context.Context
	source io.Reader
	size   int64
	err    error
}

func (r *legacyReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.source.Read(data)
	r.size += int64(n)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}
