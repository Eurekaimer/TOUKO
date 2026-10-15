package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"sort"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

type VerifyResult struct {
	Head    domain.RepositoryHead `json:"head"`
	Assets  int                   `json:"assets"`
	Objects int                   `json:"objects"`
}
type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(b)
}

type countedHash struct {
	hash.Hash
	size int64
}

func (w *countedHash) Write(b []byte) (int, error) {
	n, err := w.Hash.Write(b)
	w.size += int64(n)
	return n, err
}
func VerifyObject(ctx context.Context, r Reader, blob BlobID, expected domain.ContentObject) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = expected.Validate(); err != nil {
		return corrupt(err)
	}
	body, err := r.OpenBlob(ctx, blob)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := body.Close(); err == nil {
			err = closeErr
		}
	}()
	sum := countedHash{Hash: sha256.New()}
	source := io.TeeReader(io.LimitReader(contextReader{ctx: ctx, source: body}, expected.Size+1), &sum)
	config, format, headerErr := image.DecodeConfig(source)
	if _, err = io.Copy(io.Discard, source); err != nil {
		return err
	}
	var got domain.ObjectHash
	sum.Sum(got[:0])
	if sum.size != expected.Size || got != expected.Hash {
		return fmt.Errorf("%w: blob %s expected %s (%d bytes), got %s (%d bytes)", domain.ErrHashMismatch, blob, expected.Hash, expected.Size, got, sum.size)
	}
	ext := format
	if ext == "jpeg" {
		ext = "jpg"
	}
	if headerErr != nil || ext != expected.Ext || config.Width != expected.Width || config.Height != expected.Height {
		return corrupt(fmt.Errorf("image header disagrees with metadata for blob %s", blob))
	}
	return nil
}
func VerifySnapshot(ctx context.Context, r Reader, s Snapshot) (VerifyResult, error) {
	placements, err := placements(s.Assets)
	if err != nil {
		return VerifyResult{}, err
	}
	entries := make(map[string]GitEntry, len(s.Entries))
	for _, e := range s.Entries {
		entries[e.Path] = e
	}
	paths := make([]string, 0, len(placements))
	for path := range placements {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	verified := make(map[BlobID]domain.ContentObject)
	for _, path := range paths {
		placement := placements[path]
		entry, ok := entries[path]
		if !ok || entry.Type != "blob" || entry.Mode != "100644" || entry.Size != placement.Object.Size {
			return VerifyResult{}, corrupt(fmt.Errorf("missing or invalid object %s", path))
		}
		blob := BlobID(entry.SHA)
		if previous, ok := verified[blob]; ok {
			if previous != placement.Object {
				return VerifyResult{}, corrupt(fmt.Errorf("one Git blob has contradictory descriptions"))
			}
			continue
		}
		if err = VerifyObject(ctx, r, blob, placement.Object); err != nil {
			return VerifyResult{}, fmt.Errorf("%s: %w", path, err)
		}
		verified[blob] = placement.Object
	}
	return VerifyResult{Head: s.Head, Assets: len(s.Assets), Objects: len(placements)}, nil
}
