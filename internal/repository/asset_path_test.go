package repository

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

func TestAssetObjectPathUsesIdentityNotContent(t *testing.T) {
	id, err := domain.NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	path := AssetObjectPath("blog", id, "webp")
	if path != "objects/blog/"+id.String()+".webp" {
		t.Fatalf("path = %q", path)
	}
	if strings.Contains(path, "sha256") {
		t.Fatalf("path still mentions hashing: %q", path)
	}
	if AssetObjectPath("notes", id, "webp") == path {
		t.Fatal("namespace no longer changes the path")
	}
	asset := domain.Asset{ID: id, Namespace: "blog", Path: path}
	if ObjectPath(asset) != path {
		t.Fatal("explicit path is not authoritative")
	}
	asset.Path = ""
	asset.Object.Hash = domain.ObjectHash{1}
	asset.Object.Ext = "webp"
	if got := ObjectPath(asset); !strings.HasPrefix(got, "objects/blog/sha256/") {
		t.Fatalf("legacy fallback changed: %q", got)
	}
}

func TestTimestampObjectPathUsesStagingClock(t *testing.T) {
	at := time.Date(2026, 11, 22, 10, 23, 45, 123456789, time.UTC)
	stamp := strconv.FormatInt(at.UnixNano(), 10)
	if got := TimestampObjectPath(at, "webp", 0); got != stamp+".webp" {
		t.Fatalf("path = %q", got)
	}
	if got := TimestampObjectPath(at, "png", 3); got != stamp+"-3.png" {
		t.Fatalf("collision path = %q", got)
	}
	for _, value := range []string{stamp + ".webp", stamp + "-1.webp", "images/" + stamp + "-12.webp"} {
		if !IsTimestampObjectPath(value, "webp") {
			t.Fatalf("valid timestamp path rejected: %q", value)
		}
	}
	for _, tc := range []struct{ value, ext string }{
		{stamp, "webp"},
		{stamp + ".png", "webp"},
		{"0" + stamp + ".webp", "webp"},
		{stamp + "-.webp", "webp"},
		{stamp + "-0.webp", "webp"},
		{stamp + "-01.webp", "webp"},
		{stamp + ".webp", ""},
		{"img/photo.webp", "webp"},
	} {
		if IsTimestampObjectPath(tc.value, tc.ext) {
			t.Fatalf("invalid timestamp path accepted: %q (%q)", tc.value, tc.ext)
		}
	}
}

func TestObjectPathNamesUnallocatedHashlessUploadByID(t *testing.T) {
	id, err := domain.NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	object := domain.ContentObject{MIME: "image/webp", Ext: "webp", Size: 12, Width: 2, Height: 2}
	asset := domain.Asset{ID: id, Namespace: "blog", Object: object}
	if got := ObjectPath(asset); got != AssetObjectPath("blog", id, "webp") {
		t.Fatalf("planning path = %q", got)
	}
	other, _ := domain.NewAssetID()
	if ObjectPath(domain.Asset{ID: other, Namespace: "blog", Object: object}) == ObjectPath(asset) {
		t.Fatal("planning paths must stay per-asset")
	}
	asset.Path = "1789000000000000000.webp"
	if ObjectPath(asset) != asset.Path {
		t.Fatal("explicit path is not authoritative")
	}
}

func TestDirectoryPathReplacesSpaces(t *testing.T) {
	if got := DirectoryPath("my images/blog posts"); got != "my-images/blog-posts" {
		t.Fatalf("directory = %q", got)
	}
	if got := DirectoryPath("my\u00a0images/blog\u2003posts"); got != "my-images/blog-posts" {
		t.Fatalf("non-ASCII spaces = %q", got)
	}
	if got := DirectoryPath(""); got != "" {
		t.Fatalf("empty directory = %q", got)
	}
}
