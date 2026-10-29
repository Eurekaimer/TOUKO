package repository

import (
	"strings"
	"unicode"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const ManifestPath = ".touko/repository.json"

func AssetPath(id domain.AssetID) string { return ".touko/assets/" + id.String() + ".json" }

// AssetObjectPath is the canonical location for a new asset. The Asset ID names
// the file, so building a path never hashes anything and each asset owns exactly
// one object; identical bytes uploaded twice stay two objects.
func AssetObjectPath(namespace domain.Namespace, id domain.AssetID, ext string) string {
	return "objects/" + string(namespace) + "/" + id.String() + "." + ext
}

// LegacyObjectPath is the content-addressed layout written before asset-id
// paths. Repositories that still hold such assets stay readable; nothing new
// uses it.
func LegacyObjectPath(namespace domain.Namespace, object domain.ContentObject) string {
	h := object.Hash.String()
	return "objects/" + string(namespace) + "/sha256/" + h[:2] + "/" + h[2:4] + "/" + h + "." + object.Ext
}

// ObjectPath is the actual repository location of an asset's image. Assets
// written with the asset-id layout carry the path; older ones fall back to the
// content-addressed location derived from metadata.
func ObjectPath(asset domain.Asset) string {
	if asset.Path != "" {
		return asset.Path
	}
	return LegacyObjectPath(asset.Namespace, asset.Object)
}

// DirectoryPath replaces spaces in an upload directory; other characters are URL-escaped.
func DirectoryPath(directory string) string {
	parts := strings.Split(directory, "/")
	for i, part := range parts {
		parts[i] = strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return '-'
			}
			return r
		}, part)
	}
	return strings.Join(parts, "/")
}
