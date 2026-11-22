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
	return verifyStream(ctx, source, object)
}

// VerifyDescription checks the bytes against the object description. A hashless
// object has no exact-byte proof, so only the declared size, format, and
// dimensions are enforced; callers must already have established that the path
// is immutable (an explicit per-upload name). A present hash is still compared.
func VerifyDescription(ctx context.Context, source io.Reader, object domain.ContentObject) error {
	if err := object.Validate(); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
	}
	return verifyStream(ctx, source, object)
}

// VerifyHeader checks only that source starts with an image matching object's
// format and dimensions. It never hashes and never drains the stream, so the
// caller must verify the file length separately (for example with Lstat on a
// temporary it just ingested).
func VerifyHeader(ctx context.Context, source io.Reader, object domain.ContentObject) error {
	if err := object.Validate(); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrRepositoryCorrupt, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	config, format, err := stdimage.DecodeConfig(contextReader{ctx, source})
	if err != nil {
		return fmt.Errorf("%w: decode image header: %v", domain.ErrRepositoryCorrupt, err)
	}
	if !headerMatches(object, format, config.Width, config.Height) {
		return fmt.Errorf("%w: image header disagrees with metadata", domain.ErrRepositoryCorrupt)
	}
	return nil
}

func verifyStream(ctx context.Context, source io.Reader, object domain.ContentObject) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var digest hash.Hash
	if object.HasHash() {
		digest = sha256.New()
	}
	counter := &countingWriter{digest: digest}
	reader := io.TeeReader(io.LimitReader(contextReader{ctx, source}, object.Size+1), counter)
	config, format, headerErr := stdimage.DecodeConfig(reader)
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return err
	}
	if counter.size != object.Size {
		return fmt.Errorf("%w: expected %d bytes, read %d", domain.ErrHashMismatch, object.Size, counter.size)
	}
	if digest != nil {
		var actual domain.ObjectHash
		digest.Sum(actual[:0])
		if actual != object.Hash {
			return fmt.Errorf("%w: bytes do not match %s", domain.ErrHashMismatch, object.Hash)
		}
	}
	if headerErr != nil || !headerMatches(object, format, config.Width, config.Height) {
		return fmt.Errorf("%w: image header disagrees with metadata", domain.ErrRepositoryCorrupt)
	}
	return nil
}

func headerMatches(object domain.ContentObject, format string, width, height int) bool {
	ext := format
	if ext == "jpeg" {
		ext = "jpg"
	}
	return ext == object.Ext && width == object.Width && height == object.Height
}

type countingWriter struct {
	digest hash.Hash
	size   int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	if w.digest != nil {
		if _, err := w.digest.Write(data); err != nil {
			return 0, err
		}
	}
	w.size += int64(len(data))
	return len(data), nil
}
