package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"testing"

	"github.com/gen2brain/webp"
)

func encodedImage(t *testing.T, format string) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 7, 5))
	img.Set(2, 3, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	case "webp":
		err = webp.Encode(&b, img, webp.Options{Quality: 90})
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodedPhoto(t *testing.T, format string) []byte {
	t.Helper()
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, 256, 192))
	state := uint32(0x9e3779b9)
	for y := range 192 {
		for x := range 256 {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(state), G: uint8(state >> 8), B: uint8(state >> 16), A: 255})
		}
	}
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 100})
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestIngestContentIdentityAndFormats(t *testing.T) {
	for _, tc := range []struct{ format, ext, mime string }{
		{"png", "png", "image/png"},
		{"jpeg", "jpg", "image/jpeg"},
		{"gif", "gif", "image/gif"},
		{"webp", "webp", "image/webp"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			data := encodedImage(t, tc.format)
			dir := t.TempDir()
			first, err := Ingest(context.Background(), bytes.NewReader(data), dir)
			if err != nil {
				t.Fatal(err)
			}
			second, err := Ingest(context.Background(), bytes.NewReader(data), dir)
			if err != nil {
				t.Fatal(err)
			}
			if first.Object != second.Object || first.TempPath == second.TempPath {
				t.Fatalf("repeated ingest did not preserve identity with independent temporaries: %+v %+v", first, second)
			}
			if first.Object.Hash != sha256.Sum256(data) || first.Object.Size != int64(len(data)) || first.Object.Ext != tc.ext || first.Object.MIME != tc.mime || first.Object.Width != 7 || first.Object.Height != 5 {
				t.Fatalf("unexpected object: %+v", first.Object)
			}
			persisted, err := os.ReadFile(first.TempPath)
			if err != nil || !bytes.Equal(persisted, data) {
				t.Fatalf("persisted bytes differ: %v", err)
			}
			info, err := os.Stat(first.TempPath)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("temporary permissions: %v %v", info, err)
			}
		})
	}
}

func TestIngestConvertsPNGAndJPEGToWebP(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			result, err := IngestWithOptions(
				context.Background(),
				bytes.NewReader(encodedPhoto(t, format)),
				t.TempDir(),
				Options{ConvertWebP: true, WebPQuality: 80},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Object.Ext != "webp" || result.Object.MIME != "image/webp" || result.Object.Width != 256 || result.Object.Height != 192 {
				t.Fatalf("conversion metadata: %+v", result.Object)
			}
			encoded, err := os.ReadFile(result.TempPath)
			if err != nil {
				t.Fatal(err)
			}
			if result.Object.Size != int64(len(encoded)) || result.Object.Hash != sha256.Sum256(encoded) {
				t.Fatal("converted bytes do not match their manifest")
			}
			config, decodedFormat, err := stdimage.DecodeConfig(bytes.NewReader(encoded))
			if err != nil || decodedFormat != "webp" || config.Width != 256 || config.Height != 192 {
				t.Fatalf("staged WebP invalid: %+v %q %v", config, decodedFormat, err)
			}
		})
	}
}

func TestIngestKeepsSmallerOriginal(t *testing.T) {
	data := encodedImage(t, "png")
	result, err := IngestWithOptions(
		context.Background(),
		bytes.NewReader(data),
		t.TempDir(),
		Options{ConvertWebP: true, WebPQuality: 80},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Object.Ext != "png" || result.Object.Hash != sha256.Sum256(data) {
		t.Fatalf("larger WebP replaced a smaller PNG: %+v", result.Object)
	}
	persisted, err := os.ReadFile(result.TempPath)
	if err != nil || !bytes.Equal(persisted, data) {
		t.Fatalf("original bytes not retained: %v", err)
	}
}

func TestIngestDoesNotReencodeGIFOrWebP(t *testing.T) {
	for _, format := range []string{"gif", "webp"} {
		t.Run(format, func(t *testing.T) {
			data := encodedImage(t, format)
			result, err := IngestWithOptions(
				context.Background(),
				bytes.NewReader(data),
				t.TempDir(),
				Options{ConvertWebP: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Object.Hash != sha256.Sum256(data) || result.Object.Ext != format {
				t.Fatalf("input was re-encoded: %+v", result.Object)
			}
		})
	}
}

func TestIngestRejectsInvalidWebPQuality(t *testing.T) {
	for _, quality := range []int{-1, 101} {
		if result, err := IngestWithOptions(context.Background(), bytes.NewReader(encodedImage(t, "png")), t.TempDir(), Options{ConvertWebP: true, WebPQuality: quality}); err == nil || result != (Result{}) {
			t.Fatalf("quality %d accepted: %+v %v", quality, result, err)
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

type cancelReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func TestIngestFailuresRemoveTemporary(t *testing.T) {
	readErr := errors.New("source failed")
	for _, name := range []string{"empty", "invalid", "read", "canceled", "cancel_during_read", "oversize"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var source io.Reader = bytes.NewReader(nil)
			var expected error
			switch name {
			case "invalid":
				source = bytes.NewBufferString("not an image")
			case "read":
				source, expected = failedReader{readErr}, readErr
			case "canceled":
				cancel()
				expected = context.Canceled
			case "cancel_during_read":
				source, expected = cancelReader{bytes.NewReader(encodedImage(t, "png")), cancel}, context.Canceled
			case "oversize":
				source = io.LimitReader(zeroReader{}, MaxBytes+1)
			}
			dir := t.TempDir()
			result, err := Ingest(ctx, source, dir)
			if err == nil || result != (Result{}) || (expected != nil && !errors.Is(err, expected)) {
				t.Fatalf("failure result=%+v err=%v expected=%v", result, err, expected)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed ingest left temporary: %v %v", entries, err)
			}
		})
	}
}

func TestIngestAcceptsExactByteLimit(t *testing.T) {
	data := encodedImage(t, "png")
	source := io.MultiReader(bytes.NewReader(data), io.LimitReader(zeroReader{}, MaxBytes-int64(len(data))))
	result, err := Ingest(context.Background(), source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.Object.Size != MaxBytes || result.Object.Width != 7 || result.Object.Ext != "png" {
		t.Fatalf("incorrect boundary object: %+v", result.Object)
	}
}

func TestIngestRejectsUnwritableTemporaryLocation(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-directory")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := Ingest(context.Background(), bytes.NewReader(encodedImage(t, "png")), file.Name())
	if err == nil || result != (Result{}) {
		t.Fatalf("invalid temporary location: %+v %v", result, err)
	}
}
