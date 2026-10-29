package image

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	stdimage "image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

// Verify checks source bytes against object. A content hash is required: it is
// the only check that catches a same-size, same-header corruption between
// staging and publication. The caller owns source and closes it.
func Verify(ctx context.Context, source io.Reader, object domain.ContentObject) error {
	if err := object.Validate(); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
	}
	if !object.HasHash() {
		return fmt.Errorf("%w: object has no content hash", domain.ErrRepositoryCorrupt)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	digest := sha256.New()
	counter := &countingWriter{digest: digest}
	reader := io.TeeReader(io.LimitReader(contextReader{ctx, source}, object.Size+1), counter)
	config, format, headerErr := stdimage.DecodeConfig(reader)
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return err
	}
	if counter.size != object.Size {
		return fmt.Errorf("%w: expected %d bytes, read %d", domain.ErrHashMismatch, object.Size, counter.size)
	}
	var actual domain.ObjectHash
	digest.Sum(actual[:0])
	if actual != object.Hash {
		return fmt.Errorf("%w: bytes do not match %s", domain.ErrHashMismatch, object.Hash)
	}
	ext := format
	if ext == "jpeg" {
		ext = "jpg"
	}
	if headerErr != nil || ext != object.Ext || config.Width != object.Width || config.Height != object.Height {
		return fmt.Errorf("%w: image header disagrees with metadata", domain.ErrRepositoryCorrupt)
	}
	return nil
}

type countingWriter struct {
	digest hash.Hash
	size   int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	if _, err := w.digest.Write(data); err != nil {
		return 0, err
	}
	w.size += int64(len(data))
	return len(data), nil
}
