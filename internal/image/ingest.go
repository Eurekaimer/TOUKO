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
	"github.com/gen2brain/webp"
)

const MaxBytes int64 = 100_000_000

// DefaultWebPQuality balances size and visible loss for converted uploads.
const DefaultWebPQuality = 85

// MaxConvertPixels bounds the decoded image used for an opt-in WebP conversion.
// Decoding is the expensive step; a 50-megapixel RGBA frame is already 200 MB.
const MaxConvertPixels int64 = 50_000_000

type Result struct {
	Object   domain.ContentObject
	TempPath string
}

type Options struct {
	// ConvertWebP re-encodes PNG and JPEG uploads as WebP. GIF and WebP inputs
	// stay byte-for-byte unchanged, so animation and already-compressed data are
	// never silently degraded.
	ConvertWebP bool
	// WebPQuality is 1..100; zero selects DefaultWebPQuality.
	WebPQuality int
}

// Ingest reads source once. On success the caller owns the returned temporary;
// all failures remove it. Source remains owned by the caller.
func Ingest(ctx context.Context, source io.Reader, tempDir string) (Result, error) {
	return IngestWithOptions(ctx, source, tempDir, Options{})
}

func IngestWithOptions(ctx context.Context, source io.Reader, tempDir string, opts Options) (Result, error) {
	if opts.WebPQuality == 0 {
		opts.WebPQuality = DefaultWebPQuality
	}
	if opts.WebPQuality < 1 || opts.WebPQuality > 100 {
		return Result{}, fmt.Errorf("WebP quality must be between 1 and 100")
	}
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
	case "webp":
		ext, mime = "webp", "image/webp"
	default:
		return Result{}, fmt.Errorf("unsupported image format %q", format)
	}
	if opts.ConvertWebP && (format == "png" || format == "jpeg") {
		if int64(config.Width)*int64(config.Height) > MaxConvertPixels {
			return Result{}, fmt.Errorf("image is too large to convert (%d×%d; limit %d pixels); upload without WebP conversion", config.Width, config.Height, MaxConvertPixels)
		}
		converted, err := convertToWebP(ctx, filePath, tempDir, opts.WebPQuality)
		if err != nil {
			return Result{}, err
		}
		// The option exists to save bytes. A synthetic, tiny or already tiny image
		// can grow after WebP framing, so keep the original in that case.
		if converted.Object.Size < size {
			if err := os.Remove(filePath); err != nil {
				_ = os.Remove(converted.TempPath)
				return Result{}, fmt.Errorf("remove converted source: %w", err)
			}
			keep = true
			return converted, nil
		}
		_ = os.Remove(converted.TempPath)
	}
	object := domain.ContentObject{MIME: mime, Ext: ext, Size: size, Width: config.Width, Height: config.Height}
	hash.Sum(object.Hash[:0])
	if err := object.Validate(); err != nil {
		return Result{}, err
	}
	keep = true
	return Result{Object: object, TempPath: filePath}, nil
}

func convertToWebP(ctx context.Context, sourcePath, tempDir string, quality int) (Result, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return Result{}, fmt.Errorf("open image for WebP conversion: %w", err)
	}
	decoded, _, decodeErr := stdimage.Decode(contextReader{ctx, source})
	closeErr := source.Close()
	if decodeErr != nil {
		return Result{}, fmt.Errorf("decode image for WebP conversion: %w", decodeErr)
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close image for WebP conversion: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	bounds := decoded.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return Result{}, fmt.Errorf("decoded image has invalid dimensions")
	}
	output, err := os.CreateTemp(tempDir, "webp-*")
	if err != nil {
		return Result{}, fmt.Errorf("create WebP temporary: %w", err)
	}
	outputPath := output.Name()
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(outputPath)
		}
	}()
	digest := sha256.New()
	if err := webp.Encode(io.MultiWriter(output, digest), decoded, webp.Options{Quality: quality}); err != nil {
		return Result{}, fmt.Errorf("encode WebP: %w", err)
	}
	if err := output.Sync(); err != nil {
		return Result{}, fmt.Errorf("sync WebP temporary: %w", err)
	}
	info, err := output.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("stat WebP temporary: %w", err)
	}
	if err := output.Close(); err != nil {
		return Result{}, fmt.Errorf("close WebP temporary: %w", err)
	}
	object := domain.ContentObject{
		MIME: "image/webp", Ext: "webp", Size: info.Size(),
		Width: bounds.Dx(), Height: bounds.Dy(),
	}
	digest.Sum(object.Hash[:0])
	if err := object.Validate(); err != nil {
		return Result{}, err
	}
	keep = true
	return Result{Object: object, TempPath: outputPath}, nil
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
