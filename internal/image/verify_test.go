package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

func TestVerifyContentWithAndWithoutDigest(t *testing.T) {
	data := encodedImage(t, "png")
	object := domain.ContentObject{MIME: "image/png", Ext: "png", Size: int64(len(data)), Width: 7, Height: 5}
	if err := Verify(context.Background(), bytes.NewReader(data), object); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("hashless verification was not refused: %v", err)
	}

	hashed := object
	hashed.Hash = domain.ObjectHash(sha256.Sum256(data))
	if err := Verify(context.Background(), bytes.NewReader(data), hashed); err != nil {
		t.Fatalf("hashed verification: %v", err)
	}
	wrongHash := hashed
	wrongHash.Hash[0] ^= 1
	if err := Verify(context.Background(), bytes.NewReader(data), wrongHash); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("wrong digest accepted: %v", err)
	}

	wrongSize := hashed
	wrongSize.Size++
	if err := Verify(context.Background(), bytes.NewReader(data), wrongSize); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("wrong size accepted: %v", err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[0] ^= 1
	corruptObject := hashed
	corruptObject.Hash = domain.ObjectHash(sha256.Sum256(corrupt))
	if err := Verify(context.Background(), bytes.NewReader(corrupt), corruptObject); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("corrupt header accepted: %v", err)
	}
	wrongDimensions := hashed
	wrongDimensions.Width++
	if err := Verify(context.Background(), bytes.NewReader(data), wrongDimensions); !errors.Is(err, domain.ErrRepositoryCorrupt) {
		t.Fatalf("wrong dimensions accepted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Verify(ctx, bytes.NewReader(data), hashed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
