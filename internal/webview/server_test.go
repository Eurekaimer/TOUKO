package webview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const testOrigin = "http://127.0.0.1:45873"

type fixture struct {
	t          *testing.T
	service    *app.Service
	server     *Server
	root, csrf string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	service, err := app.Open(t.Context(), filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	server, err := New(service, "sentinel-private-token", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, service: service, server: server, root: root}
	state := f.request("GET", "/api/state", nil, 200)
	var value struct {
		CSRF        string               `json:"csrf"`
		Location    *repository.Location `json:"location"`
		Initialized bool                 `json:"initialized"`
		Assets      []app.AssetView      `json:"assets"`
		Changes     []domain.Change      `json:"changes"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Location != nil || value.Initialized || value.Assets == nil || value.Changes == nil {
		t.Fatalf("bad onboarding state: %s", state.Body.String())
	}
	if strings.Contains(state.Body.String(), "sentinel-private-token") {
		t.Fatal("secret exposed")
	}
	f.csrf = value.CSRF
	if f.csrf == "" {
		t.Fatal("missing csrf")
	}
	return f
}
func (f *fixture) request(method, path string, body any, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		data = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, testOrigin+path, data)
	if method != "GET" {
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("X-Touko-CSRF", f.csrf)
		req.Header.Set("Content-Type", "application/json")
	}
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code != status {
		f.t.Fatalf("%s %s: status %d, want %d: %s", method, path, out.Code, status, out.Body.String())
	}
	return out
}
func (f *fixture) setup() {
	f.t.Helper()
	f.request("POST", "/api/repo", map[string]any{"backend": "localgit", "path": filepath.Join(f.root, "gallery.git"), "branch": "main", "create": true}, 200)
}
func imageBytes(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 7, 5))
	im.Set(2, 3, color.RGBA{R: 180, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func photoBytes(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 128, 96))
	state := uint32(0x9e3779b9)
	for y := range 96 {
		for x := range 128 {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			im.SetNRGBA(x, y, color.NRGBA{R: uint8(state), G: uint8(state >> 8), B: uint8(state >> 16), A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (f *fixture) upload(files [][]byte, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.uploadOptions(files, nil, status)
}
func (f *fixture) uploadOptions(files [][]byte, extra map[string]string, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, b := range files {
		p, err := writer.CreateFormFile("files", "剪贴板.png")
		if err != nil {
			f.t.Fatal(err)
		}
		p.Write(b)
	}
	writer.WriteField("namespace", "notes")
	writer.WriteField("tags", `[" sky ","sky"]`)
	writer.WriteField("collections", `["日常"]`)
	for name, value := range extra {
		writer.WriteField(name, value)
	}
	writer.Close()
	req := httptest.NewRequest("POST", testOrigin+"/api/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-Touko-CSRF", f.csrf)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code != status {
		f.t.Fatalf("upload: %d want %d: %s", out.Code, status, out.Body.String())
	}
	return out
}
func (f *fixture) add() (string, []byte) {
	f.t.Helper()
	b := imageBytes(f.t)
	out := f.upload([][]byte{b}, 200)
	var value struct {
		Assets []app.AssetView `json:"assets"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		f.t.Fatal(err)
	}
	if len(value.Assets) != 1 || value.Assets[0].Asset.Tags[0] != "sky" || value.Assets[0].Asset.Collections[0] != "日常" {
		f.t.Fatalf("wrong uploaded metadata: %s", out.Body.String())
	}
	return value.Assets[0].Asset.ID.String(), b
}

func TestGalleryLifecycleAndImmutablePreview(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, b := f.add()
	preview := f.request("GET", "/api/assets/"+id+"/image", nil, 200)
	if !bytes.Equal(preview.Body.Bytes(), b) || preview.Header().Get("Content-Type") != "image/png" {
		t.Fatal("staged image differs")
	}
	f.request("POST", "/api/assets/"+id+"/edit", map[string]any{"title": "夕空", "tags": []string{"warm", "warm"}, "collections": []string{"手帖"}}, 200)
	f.request("POST", "/api/commit", map[string]string{"message": "publish image"}, 200)
	fresh, err := app.Open(t.Context(), filepath.Join(f.root, "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	loc, err := f.service.Location(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = fresh.Select(t.Context(), loc); err != nil {
		t.Fatal(err)
	}
	git, err := localgit.Open(t.Context(), loc.Path, loc.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.Sync(t.Context(), git); err != nil {
		t.Fatal(err)
	}
	view, err := fresh.Show(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Asset.Title != "夕空" || len(view.Asset.Tags) != 1 || view.Asset.Tags[0] != "warm" || view.Asset.Collections[0] != "手帖" {
		t.Fatalf("fresh metadata wrong: %+v", view)
	}
	// Advance the live branch without changing the browser's committed snapshot.
	if err = fresh.Remove(t.Context(), []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.Commit(t.Context(), git, "external removal"); err != nil {
		t.Fatal(err)
	}
	preview = f.request("GET", "/api/assets/"+id+"/image", nil, 200)
	if !bytes.Equal(preview.Body.Bytes(), b) {
		t.Fatal("preview followed live branch")
	}
	f.request("POST", "/api/remove", map[string]any{"selectors": []string{id}}, 200)
	f.request("GET", "/api/assets/"+id+"/image", nil, 404)
	f.request("POST", "/api/reset", map[string]bool{"confirm": true}, 200)
	f.request("POST", "/api/sync", map[string]any{}, 200)
	f.request("GET", "/api/assets/"+id+"/image", nil, 404)
	f.request("POST", "/api/verify", map[string]any{}, 200)
}

func TestDeliveryDisabledCompatibility(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "invalid key")
			t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "invalid\nsecret")
			f := newFixture(t)
			f.setup()
			if disabled {
				location, err := f.service.Location(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(location)
				if err != nil {
					t.Fatal(err)
				}
				data = append(bytes.TrimSuffix(data, []byte("}")), []byte(`,"cloudflare":{"enabled":false}}`)...)
				if err = os.WriteFile(filepath.Join(f.service.Dir, "config.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			id, image := f.add()
			f.request("GET", "/api/assets/"+id+"/url", nil, 400)
			f.request("GET", "/api/assets/"+id+"/url?provider=jsdelivr", nil, 400)
			commit := f.request("POST", "/api/commit", map[string]string{"message": "compatibility"}, 200)
			state := f.request("GET", "/api/state", nil, 200)
			for _, response := range []*httptest.ResponseRecorder{commit, state} {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
					t.Fatal(err)
				}
				if _, exists := fields["cloudflare"]; exists {
					t.Fatal("disabled mirror changed old JSON", response.Body.String())
				}
			}
			if got := f.request("GET", "/api/assets/"+id+"/image", nil, 200); !bytes.Equal(got.Body.Bytes(), image) {
				t.Fatal("committed preview changed")
			}
			f.request("POST", "/api/sync", map[string]any{}, 200)
			f.request("POST", "/api/remove", map[string]any{"selectors": []string{id}}, 200)
			f.request("POST", "/api/commit", map[string]string{"message": "remove"}, 200)
			f.request("GET", "/api/assets/"+id+"/image", nil, 404)
			if _, err := os.Stat(filepath.Join(f.service.Dir, "cloudflare")); !os.IsNotExist(err) {
				t.Fatalf("disabled mirror created ledger: %v", err)
			}
		})
	}
}

func TestPreviewRevalidatesWithoutReadingBytes(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, data := f.add()
	first := f.request("GET", "/api/assets/"+id+"/image", nil, 200)
	if !bytes.Equal(first.Body.Bytes(), data) {
		t.Fatal("initial preview bytes differ")
	}
	tag := first.Header().Get("ETag")
	if tag == "" || !strings.Contains(first.Header().Get("Cache-Control"), "no-cache") {
		t.Fatalf("preview caching headers: %v", first.Header())
	}
	req := httptest.NewRequest("GET", testOrigin+"/api/assets/"+id+"/image", nil)
	req.Header.Set("If-None-Match", tag)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code != http.StatusNotModified || out.Body.Len() != 0 {
		t.Fatalf("revalidation: %d %q", out.Code, out.Body.String())
	}
	f.request("POST", "/api/remove", map[string]any{"selectors": []string{id}}, 200)
	f.request("GET", "/api/assets/"+id+"/image", nil, 404)
}

func TestPreviewRevalidatesAfterRemoteContentChanges(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, _ := f.add()
	f.request("POST", "/api/commit", map[string]string{"message": "publish"}, http.StatusOK)
	first := f.request("GET", "/api/assets/"+id+"/image", nil, http.StatusOK)
	oldTag := first.Header().Get("ETag")
	if oldTag == "" {
		t.Fatal("missing preview ETag")
	}

	replacement := photoBytes(t)
	uploaded := f.upload([][]byte{replacement}, http.StatusOK)
	var added struct {
		Assets []app.AssetView `json:"assets"`
	}
	if err := json.Unmarshal(uploaded.Body.Bytes(), &added); err != nil || len(added.Assets) != 1 {
		t.Fatalf("replacement upload: %v", err)
	}
	f.request("POST", "/api/reset", map[string]bool{"confirm": true}, http.StatusOK)
	current, err := f.service.Show(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	updated := current.Asset
	updated.Object = added.Assets[0].Asset.Object
	metadata, err := repository.EncodeAsset(updated)
	if err != nil {
		t.Fatal(err)
	}
	location, err := f.service.Location(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	git, err := localgit.Open(t.Context(), location.Path, location.Branch)
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	base, err := git.Commit(t.Context(), head)
	if err != nil {
		t.Fatal(err)
	}
	imageBlob, err := git.CreateBlob(t.Context(), bytes.NewReader(replacement), int64(len(replacement)))
	if err != nil {
		t.Fatal(err)
	}
	metadataBlob, err := git.CreateBlob(t.Context(), bytes.NewReader(metadata), int64(len(metadata)))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := git.CreateTree(t.Context(), base.Tree, []repository.TreeChange{
		{Path: repository.ObjectPath(updated), Blob: &imageBlob},
		{Path: repository.AssetPath(updated.ID), Blob: &metadataBlob},
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := git.CreateCommit(t.Context(), tree, head, "replace image externally")
	if err != nil {
		t.Fatal(err)
	}
	if err := git.UpdateRef(t.Context(), head, next); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/api/sync", map[string]any{}, http.StatusOK)

	req := httptest.NewRequest(http.MethodGet, testOrigin+"/api/assets/"+id+"/image", nil)
	req.Header.Set("If-None-Match", oldTag)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code != http.StatusOK || !bytes.Equal(out.Body.Bytes(), replacement) {
		t.Fatalf("changed image served stale cache: status %d", out.Code)
	}
	if out.Header().Get("ETag") == oldTag {
		t.Fatal("changed bytes reused the old ETag")
	}
}

func TestUploadWebPAndOptionalConversion(t *testing.T) {
	f := newFixture(t)
	f.setup()
	f.uploadOptions([][]byte{photoBytes(t)}, map[string]string{
		"convert_webp": "true", "webp_quality": "80",
	}, 200)
	state := f.request("GET", "/api/state", nil, 200)
	var value struct {
		Assets []app.AssetView `json:"assets"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Assets) != 1 {
		t.Fatalf("uploaded assets: %+v", value.Assets)
	}
	asset := value.Assets[0].Asset
	if asset.Object.Ext != "webp" || asset.Object.MIME != "image/webp" {
		t.Fatalf("conversion metadata: %+v", asset)
	}
	preview := f.request("GET", "/api/assets/"+asset.ID.String()+"/image", nil, 200)
	if preview.Header().Get("Content-Type") != "image/webp" || int64(preview.Body.Len()) != asset.Object.Size {
		t.Fatalf("converted preview headers or size differ: %s", preview.Header())
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(preview.Body.Bytes()))
	if err != nil || format != "webp" || config.Width != 128 || config.Height != 96 {
		t.Fatalf("preview is not WebP: %+v %q %v", config, format, err)
	}
}

func TestUploadFailureIsAtomicAndCleansTemporaryFiles(t *testing.T) {
	f := newFixture(t)
	f.setup()
	f.upload([][]byte{imageBytes(t), []byte("not an image")}, 400)
	state, err := f.service.Store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Changes) != 0 {
		t.Fatal("partial upload staged")
	}
	err = filepath.WalkDir(f.service.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.Contains(path, string(filepath.Separator)+"stage"+string(filepath.Separator)) || strings.Contains(d.Name(), "upload-")) {
			t.Errorf("temporary payload retained: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequestSecurityBoundaries(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, _ := f.add()
	for _, path := range []string{"/api/state", "/api/assets/" + id + "/image", "/"} {
		for _, kind := range []string{"host", "origin", "fetch"} {
			t.Run(path+kind, func(t *testing.T) {
				req := httptest.NewRequest("GET", testOrigin+path, nil)
				switch kind {
				case "host":
					req.Host = "evil.example"
				case "origin":
					req.Header.Set("Origin", "https://evil.example")
				case "fetch":
					req.Header.Set("Sec-Fetch-Site", "cross-site")
				}
				out := httptest.NewRecorder()
				f.server.ServeHTTP(out, req)
				if out.Code != 403 {
					t.Fatalf("read allowed: %d", out.Code)
				}
			})
		}
	}
	for _, kind := range []string{"csrf", "origin", "host"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest("POST", testOrigin+"/api/remove", strings.NewReader(`{"selectors":["`+id+`"]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", testOrigin)
			req.Header.Set("X-Touko-CSRF", f.csrf)
			switch kind {
			case "csrf":
				req.Header.Del("X-Touko-CSRF")
			case "origin":
				req.Header.Del("Origin")
			case "host":
				req.Host = "evil.example"
			}
			out := httptest.NewRecorder()
			f.server.ServeHTTP(out, req)
			if out.Code != 403 {
				t.Fatalf("mutation allowed: %d", out.Code)
			}
		})
	}
	f.request("GET", "/api/assets/"+id+"/image", nil, 200)
	out := f.request("GET", "/api/state", nil, 200)
	if out.Header().Get("Access-Control-Allow-Origin") != "" || !strings.Contains(out.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || out.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing security headers")
	}
}

func TestStrictRequestsAndOfflineReset(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, _ := f.add()
	f.request("PUT", "/api/state", map[string]any{}, 405)
	f.request("POST", "/api/reset", map[string]any{}, 400)
	f.request("POST", "/api/commit", map[string]string{"message": " "}, 400)
	f.request("POST", "/api/remove", map[string]any{"selectors": []string{id[:8]}}, 400)
	f.request("POST", "/api/assets/"+id+"/edit", map[string]any{"unknown": true}, 400)
	f.request("POST", "/api/commit", map[string]string{"message": strings.Repeat("x", 2<<20)}, 413)
	if err := os.Rename(filepath.Join(f.root, "gallery.git"), filepath.Join(f.root, "offline.git")); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/api/reset", map[string]bool{"confirm": true}, 200)
	st, err := f.service.Store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 0 {
		t.Fatal("offline reset retained changes")
	}
}

func TestRejectNonLoopbackOriginsAndCorruptConfiguration(t *testing.T) {
	f := newFixture(t)
	for _, origin := range []string{"http://example.com:80", "http://0.0.0.0:80", "https://127.0.0.1:80", "http://127.0.0.1:80/path", "http://user@127.0.0.1:80"} {
		if _, err := New(f.service, "", origin); err == nil {
			t.Errorf("accepted origin %s", origin)
		}
	}
	if err := os.WriteFile(filepath.Join(f.service.Dir, "config.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	f.request("GET", "/api/state", nil, 500)
}

func TestCanceledUploadDoesNotStage(t *testing.T) {
	f := newFixture(t)
	f.setup()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequest("POST", testOrigin+"/api/upload", strings.NewReader("body")).WithContext(ctx)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-Touko-CSRF", f.csrf)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, req)
	if out.Code < 400 {
		t.Fatal("canceled upload succeeded")
	}
	st, err := f.service.Store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 0 {
		t.Fatal("canceled upload staged")
	}
}

func TestConcurrentGalleryPreviews(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, expected := f.add()
	for _, committed := range []bool{false, true} {
		if committed {
			f.request("POST", "/api/commit", map[string]string{"message": "publish concurrent previews"}, 200)
		}
		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 16)
		for range cap(results) {
			go func() {
				<-start
				request := httptest.NewRequest(http.MethodGet, testOrigin+"/api/assets/"+id+"/image", nil)
				response := httptest.NewRecorder()
				f.server.ServeHTTP(response, request)
				results <- response
			}()
		}
		close(start)
		for range cap(results) {
			response := <-results
			if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), expected) {
				t.Errorf("committed=%t preview status=%d body=%q", committed, response.Code, response.Body.String())
			}
		}
	}
}

// Static assets are revalidated rather than re-sent. Embedded files have no
// modification time, so the content tag is the only validator available; a
// rebuilt binary must therefore produce a different tag, or a browser could
// keep serving a stale interface after an upgrade.
func TestStaticAssetsRevalidateAndStayPrivate(t *testing.T) {
	f := newFixture(t)

	first := f.request("GET", "/style.css", nil, http.StatusOK)
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("stylesheet served without an ETag, so it can never revalidate")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("stylesheet Cache-Control = %q, want no-cache", got)
	}

	// A second load with the tag must cost headers only.
	revalidated := httptest.NewRequest(http.MethodGet, testOrigin+"/style.css", nil)
	revalidated.Header.Set("If-None-Match", tag)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, revalidated)
	if out.Code != http.StatusNotModified || out.Body.Len() != 0 {
		t.Fatalf("revalidation = %d with %d bytes, want 304 with none", out.Code, out.Body.Len())
	}

	// The weak form and a list are both valid request syntax.
	for _, header := range []string{"W/" + tag, `"stale", ` + tag} {
		attempt := httptest.NewRequest(http.MethodGet, testOrigin+"/style.css", nil)
		attempt.Header.Set("If-None-Match", header)
		weak := httptest.NewRecorder()
		f.server.ServeHTTP(weak, attempt)
		if weak.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q = %d, want 304", header, weak.Code)
		}
	}

	// A different tag must be answered with the full body.
	stale := httptest.NewRequest(http.MethodGet, testOrigin+"/style.css", nil)
	stale.Header.Set("If-None-Match", `"not-the-current-tag"`)
	fresh := httptest.NewRecorder()
	f.server.ServeHTTP(fresh, stale)
	if fresh.Code != http.StatusOK || !bytes.Equal(fresh.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("stale tag = %d with %d bytes, want the full asset", fresh.Code, fresh.Body.Len())
	}

	// State responses must never be cacheable, whatever the static rule is.
	state := f.request("GET", "/api/state", nil, http.StatusOK)
	if got := state.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("state Cache-Control = %q, want no-store", got)
	}
	if got := state.Header().Get("ETag"); got != "" {
		t.Fatalf("state response carried an ETag %q", got)
	}

	// Assets outside the whitelist stay unreachable even though they are embedded.
	if code := f.raw("GET", "/style.css.bak"); code != http.StatusNotFound {
		t.Fatalf("unlisted asset = %d, want 404", code)
	}
}

// raw performs a request and returns only the status for negative cases.
func (f *fixture) raw(method, path string) int {
	f.t.Helper()
	request := httptest.NewRequest(method, testOrigin+path, nil)
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	return out.Code
}

func TestSingleWorkspaceUploadDoesNotNeedCloneAndCommitChecksHead(t *testing.T) {
	f := newFixture(t)
	f.setup()
	ctx := t.Context()
	configPath := filepath.Join(f.service.Dir, "config.json")
	localConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	git, err := localgit.Open(ctx, filepath.Join(f.root, "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	base, err := git.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"backend":"github","owner":"owner","name":"gallery","branch":"main"}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	f.upload([][]byte{imageBytes(t)}, 200)
	if err := os.Setenv("PATH", path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repository.CloneDir(f.service.Dir)); !os.IsNotExist(err) {
		t.Fatalf("upload created a clone: %v", err)
	}
	if err := os.WriteFile(configPath, localConfig, 0600); err != nil {
		t.Fatal(err)
	}
	head, err := git.Head(ctx)
	if err != nil || head != base {
		t.Fatalf("staging changed HEAD: %s, %v", head, err)
	}
	commit, err := git.Commit(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := git.CreateCommit(ctx, commit.Tree, base, "external change")
	if err != nil {
		t.Fatal(err)
	}
	if err := git.UpdateRef(ctx, base, advanced); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/api/commit", map[string]string{"message": "publish"}, 409)
	logData, err := os.ReadFile(filepath.Join(f.service.Dir, "operation-errors.jsonl"))
	if err != nil || !bytes.Contains(logData, []byte(`"code":"conflict"`)) || !bytes.Contains(logData, []byte("head_changed")) {
		t.Fatalf("conflict was not recorded: %q %v", logData, err)
	}
	st, err := f.service.Store.Load(ctx)
	if err != nil || len(st.Changes) != 1 {
		t.Fatalf("conflicting commit lost staged image: %v", err)
	}
}
