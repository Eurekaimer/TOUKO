package webview

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// All browser assets are embedded: opening the gallery never contacts a font,
// script or icon CDN. Only explicitly shipped files are exposed, not state files.
//
//go:embed static
var browserAssets embed.FS

// assets resolves the embedded tree once.
var assets = sync.OnceValue(func() fs.FS {
	tree, err := fs.Sub(browserAssets, "static")
	if err != nil {
		panic(err) // The embedded directory is checked at build time.
	}
	return tree
})

// assetTags hashes every embedded file once at first use. Embedded files carry no
// modification time, so Last-Modified is impossible; a content tag is the only
// validator available. That is what makes caching safe here: the browser may keep
// the fonts, but each load revalidates, and a rebuilt binary changes the tag
// immediately, so a stale preview cannot survive a reload.
var assetTags = sync.OnceValue(func() map[string]string {
	tree := assets()
	tags := make(map[string]string)
	_ = fs.WalkDir(tree, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		body, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			return nil
		}
		sum := sha256.Sum256(body)
		tags["/"+strings.TrimPrefix(path, "./")] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return tags
})

// allowedAssets is the explicit whitelist: only shipped browser files are ever
// exposed, never state files that happen to sit in the tree.
var allowedAssets = map[string]bool{
	"/":       true,
	"/app.js": true, "/style.css": true, "/favicon.png": true, "/favicon.svg": true,
	"/fonts/monaspace-radon-var.woff2":   true,
	"/fonts/lxgw-wenkai-regular.woff2":   true,
	"/fonts/lxgw-wenkai-medium.woff2":    true,
	"/fonts/wordmark-quicksand.woff2":    true,
	"/fonts/playfair-italic-latin.woff2": true,
	"/fonts/Monaspace-OFL.txt":           true,
	"/fonts/LXGW-OFL.txt":                true,
	"/fonts/Quicksand-OFL.txt":           true,
	"/fonts/Playfair-OFL.txt":            true,
}

// serveStatic answers one whitelisted asset. Unchanged assets answer 304 so the
// fonts cost headers only, while Cache-Control stays "no-cache" rather than
// "no-store": the response may be stored but is always revalidated.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	if !allowedAssets[r.URL.Path] {
		http.NotFound(w, r)
		return
	}
	if tag, ok := assetTags()[r.URL.Path]; ok {
		w.Header().Set("ETag", tag)
		w.Header().Set("Cache-Control", "no-cache")
		if matchesETag(r.Header.Get("If-None-Match"), tag) {
			w.Header().Del("Content-Type")
			w.Header().Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	http.FileServer(http.FS(assets())).ServeHTTP(w, r)
}

// matchesETag implements the If-None-Match comparison for our exact tags,
// accepting the weak prefix and the "*" form.
func matchesETag(header, tag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == tag || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}
