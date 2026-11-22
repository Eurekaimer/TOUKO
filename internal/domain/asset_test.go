package domain_test

import (
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

func validObject() domain.ContentObject {
	return domain.ContentObject{MIME: "image/png", Ext: "png", Size: 12, Width: 2, Height: 2}
}

func TestAssetValidateRequiresIdentityForHashlessContent(t *testing.T) {
	id, err := domain.NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 10, 23, 45, 0, time.UTC)
	base := domain.Asset{ID: id, Namespace: "blog", Object: validObject(), CreatedAt: now, UpdatedAt: now}
	if err := base.Validate(); err == nil {
		t.Fatal("hashless asset without an explicit path was accepted")
	}
	explicit := base
	explicit.Path = "1789000000000000000.png"
	if err := explicit.Validate(); err != nil {
		t.Fatalf("hashless explicit path rejected: %v", err)
	}
	unsafe := base
	unsafe.Path = "../escape.png"
	if err := unsafe.Validate(); err == nil {
		t.Fatal("unsafe explicit path was accepted")
	}
	hashed := base
	hashed.Object.Hash[0] = 1
	hashed.Path = ""
	if err := hashed.Validate(); err != nil {
		t.Fatalf("legacy hashed asset rejected: %v", err)
	}
	if err := (domain.ContentObject{MIME: "image/png", Ext: "png", Size: -1, Width: 2, Height: 2}).Validate(); err == nil {
		t.Fatal("invalid content description accepted")
	}
}
