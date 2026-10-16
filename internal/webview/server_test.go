package webview

import (
	"bytes"
	"context"
	"encoding/json"
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
func (f *fixture) upload(files [][]byte, status int) *httptest.ResponseRecorder {
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
