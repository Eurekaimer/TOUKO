package webview

import (
	"embed"
	"io/fs"
	"net/http"
)

// All browser assets are embedded: opening the gallery never contacts a font,
// script or icon CDN. Only explicitly shipped files are exposed, not state files.
//
//go:embed static
var browserAssets embed.FS

func staticHandler() http.Handler {
	files, _ := fs.Sub(browserAssets, "static") // The embedded directory is checked at build time.
	server := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/", "/app.js", "/style.css", "/favicon.png",
			"/fonts/monaspace-radon-var.woff2",
			"/fonts/lxgw-wenkai-regular.woff2",
			"/fonts/lxgw-wenkai-medium.woff2",
			"/fonts/wordmark-quicksand.woff2",
			"/fonts/bodoni-italic-latin.woff2",
			"/fonts/Monaspace-OFL.txt", "/fonts/LXGW-OFL.txt",
			"/fonts/Quicksand-OFL.txt", "/fonts/BodoniModa-OFL.txt":
			server.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
