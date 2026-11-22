package repository

import (
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const ManifestPath = ".touko/repository.json"

func AssetPath(id domain.AssetID) string { return ".touko/assets/" + id.String() + ".json" }

// AssetObjectPath is the asset-ID layout used as the canonical migration target
// and as a per-asset planning identity before a staging timestamp is known. New
// uploads publish at TimestampObjectPath instead; nothing hashes content to
// build a path.
func AssetObjectPath(namespace domain.Namespace, id domain.AssetID, ext string) string {
	return "objects/" + string(namespace) + "/" + id.String() + "." + ext
}

// TimestampObjectPath names a new upload after the moment it was staged. The
// decimal Unix nanosecond clock keeps the name human-readable and sortable
// without hashing the bytes; collision > 0 appends the shared "-N" suffix that
// resolves uploads staged in the same nanosecond. The name is relative to the
// caller's upload directory, so an unprefixed asset publishes at the repository
// root and its R2 key is the basename alone.
func TimestampObjectPath(at time.Time, ext string, collision int) string {
	name := strconv.FormatInt(at.UnixNano(), 10)
	if collision > 0 {
		name += "-" + strconv.Itoa(collision)
	}
	return name + "." + ext
}

// IsTimestampObjectPath reports whether value's basename is a name produced by
// TimestampObjectPath for ext. A directory prefix (an explicit upload
// directory) is allowed; the timestamp digits themselves must be canonical, so
// a leading zero or an empty collision suffix is rejected.
func IsTimestampObjectPath(value, ext string) bool {
	if value == "" || ext == "" {
		return false
	}
	suffix := "." + ext
	name := path.Base(value)
	if !strings.HasSuffix(name, suffix) {
		return false
	}
	stamp, collision, hasCollision := strings.Cut(strings.TrimSuffix(name, suffix), "-")
	if !decimalDigits(stamp) {
		return false
	}
	// The generator only appends a suffix for collision > 0.
	return !hasCollision || (collision != "0" && decimalDigits(collision))
}

// decimalDigits accepts a canonical non-negative decimal integer: digits only,
// with no redundant leading zero.
func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(value) == 1 || value[0] != '0'
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
// content-addressed location derived from metadata. A hashless asset only ever
// exists with an explicit path (domain.Asset.Validate rejects it otherwise), so
// the last branch only names an unallocated upload for routing and capacity
// planning; it is never published under this path.
func ObjectPath(asset domain.Asset) string {
	if asset.Path != "" {
		return asset.Path
	}
	if !asset.Object.HasHash() {
		return AssetObjectPath(asset.Namespace, asset.ID, asset.Object.Ext)
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
