package image

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Estimate reports what an upload with WebP conversion enabled would store for
// one input image. It is exact, not a heuristic: the image is encoded with the
// same options the upload path uses, including the rule that keeps the original
// when WebP would be larger.
type Estimate struct {
	OriginalSize  int64  `json:"original_bytes"`
	EstimatedSize int64  `json:"estimated_bytes"`
	Ext           string `json:"ext"`
	MIME          string `json:"mime"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
}

// Saved reports how many bytes the stored object would be smaller than the
// input; negative when conversion keeps the original.
func (e Estimate) Saved() int64 { return e.OriginalSize - e.EstimatedSize }

// EstimateConversion runs the real ingest path with conversion enabled and
// removes the produced temporary. Callers own source and tempDir; tempDir must
// be on the same filesystem as the eventual staging directory.
func EstimateConversion(ctx context.Context, source io.Reader, tempDir string, quality int) (Estimate, error) {
	counted := &countingReader{source: source}
	result, err := IngestWithOptions(ctx, counted, tempDir, Options{ConvertWebP: true, WebPQuality: quality})
	if err != nil {
		return Estimate{}, err
	}
	if err := os.Remove(result.TempPath); err != nil {
		return Estimate{}, fmt.Errorf("remove estimate payload: %w", err)
	}
	return Estimate{
		OriginalSize:  counted.n,
		EstimatedSize: result.Object.Size,
		Ext:           result.Object.Ext,
		MIME:          result.Object.MIME,
		Width:         result.Object.Width,
		Height:        result.Object.Height,
	}, nil
}

type countingReader struct {
	source io.Reader
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	r.n += int64(n)
	return n, err
}
