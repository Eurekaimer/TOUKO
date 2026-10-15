package image

import (
	"context"
	"crypto/sha256"
	"fmt"
	stdimage "image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const MaxBytes int64 = 100_000_000

type Result struct {
	Object   domain.ContentObject
	TempPath string
}

// Ingest reads source once. On success the caller owns the returned temporary;
// all failures remove it. Source remains owned by the caller.
func Ingest(ctx context.Context, source io.Reader, tempDir string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		return Result{}, fmt.Errorf("create ingest directory: %w", err)
	}
	if err := os.Chmod(tempDir, 0700); err != nil {
		return Result{}, fmt.Errorf("secure ingest directory: %w", err)
	}
	file, err := os.CreateTemp(tempDir, "ingest-*")
	if err != nil {
		return Result{}, fmt.Errorf("create ingest temporary: %w", err)
	}
	filePath := file.Name()
	keep := false
	defer func() {
		if file != nil {
			file.Close()
		}
		if !keep {
			os.Remove(filePath)
		}
	}()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, source}, MaxBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("stream image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if size == 0 {
		return Result{}, fmt.Errorf("image is empty")
	}
	if size > MaxBytes {
		return Result{}, fmt.Errorf("image exceeds %d byte limit", MaxBytes)
	}
	if err := file.Sync(); err != nil {
		return Result{}, fmt.Errorf("sync image temporary: %w", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return Result{}, fmt.Errorf("close image temporary: %w", err)
	}
	file = nil
	// Decode only the persisted header; never rewind or re-read the source.
	header, err := os.Open(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("open image header: %w", err)
	}
	config, format, decodeErr := stdimage.DecodeConfig(contextReader{ctx, header})
	closeErr := header.Close()
	if decodeErr != nil {
		return Result{}, fmt.Errorf("decode image header: %w", decodeErr)
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close image header: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var ext, mime string
	switch format {
	case "png":
		ext, mime = "png", "image/png"
	case "jpeg":
		ext, mime = "jpg", "image/jpeg"
	case "gif":
		ext, mime = "gif", "image/gif"
	default:
		return Result{}, fmt.Errorf("unsupported image format %q", format)
	}
	object := domain.ContentObject{MIME: mime, Ext: ext, Size: size, Width: config.Width, Height: config.Height}
	hash.Sum(object.Hash[:0])
	if err := object.Validate(); err != nil {
		return Result{}, err
	}
	keep = true
	return Result{Object: object, TempPath: filePath}, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}
