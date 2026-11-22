package repository

import (
	"context"
	"fmt"
	"sort"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	toukoimage "github.com/Eurekaimer/TOUKO/internal/image"
)

type VerifyResult struct {
	Head    domain.RepositoryHead `json:"head"`
	Assets  int                   `json:"assets"`
	Objects int                   `json:"objects"`
}
// VerifyObject checks the image description against a blob and, when the
// expected object carries one, its exact SHA-256. A hashless object is accepted
// only because the caller reached it through an explicit asset path; its bytes
// are checked by size, format and dimensions, which cannot detect a same-size
// replacement. Legacy content-addressed placements always carry a hash.
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
	if expected.HasHash() {
		err = toukoimage.Verify(ctx, body, expected)
	} else {
		err = toukoimage.VerifyDescription(ctx, body, expected)
	}
	if err != nil {
		return fmt.Errorf("blob %s: %w", blob, err)
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
		if !ok || entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || entry.Size != placement.Object.Size {
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
