package repository

import (
	"strings"
	"testing"

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
	if got := ObjectPath(asset); !strings.HasPrefix(got, "objects/blog/sha256/") {
		t.Fatalf("legacy fallback changed: %q", got)
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
