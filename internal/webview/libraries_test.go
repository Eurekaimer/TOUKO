package webview

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// createLibraryFixture builds a library over two local repositories without
// touching GitHub: membership is written through the same views.json shape the
// production connection path persists.
func createLibraryFixture(t *testing.T) (*fixture, string, []string, string) {
	t.Helper()
	f := newFixture(t)
	t.Cleanup(func() {
		if err := f.server.spaces.close(); err != nil {
			t.Error(err)
		}
	})
	f.setup()
	ctx := t.Context()
	firstID := "default"
	secondID := ""
	for _, name := range []string{"second.git"} {
		remote, err := localgit.Init(ctx, filepath.Join(f.root, name), "main")
		if err != nil {
			t.Fatal(err)
		}
		location := localGitLocation(f.root, name)
		server, err := f.server.spaces.selectLocation(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.service.Initialize(ctx, remote); err != nil {
			t.Fatal(err)
		}
		secondID = server.workspaceID
	}
	config, err := f.server.spaces.loadViews()
	if err != nil {
		t.Fatal(err)
	}
	library := libraryView{ID: "0123456789abcdef0123456789abcdef", Name: "测试图库", Workspaces: []string{firstID, secondID}}
	config.Libraries = append(config.Libraries, library)
	config.Active = activeView{Kind: "library", ID: library.ID}
	if err := f.server.spaces.saveViews(config); err != nil {
		t.Fatal(err)
	}
	csrf, err := f.server.spaces.libraryCSRF(library.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, libraryURL(library.ID), []string{firstID, secondID}, csrf
}

func localGitLocation(root, name string) repository.Location {
	return repository.Location{Backend: "localgit", Path: filepath.Join(root, name), Branch: "main"}
}

func libraryCall(t *testing.T, f *fixture, method, path string, body any, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var data *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.NewReader(encoded)
	} else {
		data = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, testOrigin+path, data)
	request.Header.Set("Origin", testOrigin)
	if csrf != "" {
		request.Header.Set("X-Touko-CSRF", csrf)
	}
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	return out
}

func TestLibraryStateRequiresItsOwnToken(t *testing.T) {
	f, url, members, csrf := createLibraryFixture(t)
	state := libraryCall(t, f, "GET", url+"api/state", nil, "")
	if state.Code != http.StatusOK {
		t.Fatalf("state: %d %s", state.Code, state.Body.String())
	}
	var value struct {
		Kind    string `json:"kind"`
		CSRF    string `json:"csrf"`
		Members []struct {
			ID string `json:"id"`
		} `json:"members"`
		Assets []struct {
			Workspace string `json:"workspace"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Kind != "library" || value.CSRF != csrf || len(value.Members) != len(members) {
		t.Fatalf("library state: %+v", value)
	}
	if value.CSRF == f.csrf {
		t.Fatal("library inherited the root mutation token")
	}
	// A root or member token must not authorize a library write.
	for _, token := range []string{f.csrf, "", "bogus"} {
		out := libraryCall(t, f, "POST", url+"api/upload", map[string]bool{"confirm": true}, token)
		if out.Code != http.StatusForbidden {
			t.Fatalf("token %q authorized a library write: %d", token, out.Code)
		}
	}
	// The library token must not authorize a member write either.
	request := httptest.NewRequest("POST", testOrigin+"/api/reset", bytes.NewReader([]byte(`{"confirm":true}`)))
	request.Header.Set("Origin", testOrigin)
	request.Header.Set("X-Touko-CSRF", csrf)
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	if out.Code != http.StatusForbidden {
		t.Fatalf("library token crossed into the root workspace: %d", out.Code)
	}
}

func TestUnknownLibraryAndCorruptViewsAreReported(t *testing.T) {
	f, _, _, _ := createLibraryFixture(t)
	libraryCall(t, f, "GET", "/l/"+strings.Repeat("f", 32)+"/api/state", nil, "")
	out := libraryCall(t, f, "GET", "/l/"+strings.Repeat("f", 32)+"/api/state", nil, "")
	if out.Code != http.StatusNotFound {
		t.Fatalf("unknown library: %d", out.Code)
	}
	path := filepath.Join(f.service.Dir, viewsFile)
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := libraryCall(t, f, "GET", "/api/libraries", nil, "")
	if broken.Code == http.StatusOK {
		t.Fatal("corrupt library configuration was silently ignored")
	}
	// The root redirect must also fail loudly instead of showing an empty gallery.
	root := httptest.NewRequest("GET", testOrigin+"/", nil)
	out = httptest.NewRecorder()
	f.server.ServeHTTP(out, root)
	if out.Code == http.StatusOK || out.Code == http.StatusSeeOther {
		t.Fatalf("corrupt configuration produced a gallery: %d", out.Code)
	}
}

func TestLibraryUploadIsAtomicAndRouted(t *testing.T) {
	f, url, members, csrf := createLibraryFixture(t)
	good := imageBytes(t)
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("files", "first.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(good); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("namespace", "default"); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("tags", `["picgo"]`); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", testOrigin+url+"api/upload", body)
	request.Header.Set("Origin", testOrigin)
	request.Header.Set("X-Touko-CSRF", csrf)
	request.Header.Set("Content-Type", form.FormDataContentType())
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	if out.Code != http.StatusOK {
		t.Fatalf("library upload: %d %s", out.Code, out.Body.String())
	}
	var uploaded struct {
		Results []struct {
			Index     int    `json:"index"`
			Workspace string `json:"workspace"`
			AssetID   string `json:"asset_id"`
		} `json:"results"`
		Failed []map[string]any `json:"failed"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if len(uploaded.Results) != 1 || len(uploaded.Failed) != 0 || uploaded.Results[0].Workspace != members[0] {
		t.Fatalf("first image did not land in the first member: %+v", uploaded)
	}
	// A malformed second part must reject the whole batch, including the valid
	// image that preceded it.
	body = &bytes.Buffer{}
	form = multipart.NewWriter(body)
	file, _ = form.CreateFormFile("files", "ok.png")
	if _, err := file.Write(good); err != nil {
		t.Fatal(err)
	}
	bad, _ := form.CreateFormFile("files", "bad.png")
	if _, err := bad.Write([]byte("not an image")); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("namespace", "default"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", testOrigin+url+"api/upload", body)
	request.Header.Set("Origin", testOrigin)
	request.Header.Set("X-Touko-CSRF", csrf)
	request.Header.Set("Content-Type", form.FormDataContentType())
	out = httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	if out.Code == http.StatusOK {
		t.Fatalf("malformed batch was accepted: %s", out.Body.String())
	}
	for _, member := range members[:1] {
		server, err := f.server.spaces.open(t.Context(), member, false)
		if err != nil {
			t.Fatal(err)
		}
		views, err := server.service.List(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 {
			t.Fatalf("rejected batch changed staged state: %d assets", len(views))
		}
	}
}

// TestLibraryUploadReusesIdenticalObject checks the routing rule that matters
// for capacity: the same bytes at the same object path reuse one object, while a
// different namespace stays a separate asset.
func TestLibraryUploadReusesIdenticalObject(t *testing.T) {
	f, url, members, csrf := createLibraryFixture(t)
	upload := func(namespace string) {
		t.Helper()
		body := &bytes.Buffer{}
		form := multipart.NewWriter(body)
		file, err := form.CreateFormFile("files", "same.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(imageBytes(t)); err != nil {
			t.Fatal(err)
		}
		if err := form.WriteField("namespace", namespace); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", testOrigin+url+"api/upload", body)
		request.Header.Set("Origin", testOrigin)
		request.Header.Set("X-Touko-CSRF", csrf)
		request.Header.Set("Content-Type", form.FormDataContentType())
		out := httptest.NewRecorder()
		f.server.ServeHTTP(out, request)
		if out.Code != http.StatusOK {
			t.Fatalf("upload %s: %d %s", namespace, out.Code, out.Body.String())
		}
	}
	upload("default")
	upload("default")
	server, err := f.server.spaces.open(t.Context(), members[0], false)
	if err != nil {
		t.Fatal(err)
	}
	views, err := server.service.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("identical bytes must remain two assets, got %d", len(views))
	}
	if views[0].Asset.Object.Hash != views[1].Asset.Object.Hash {
		t.Fatal("identical bytes produced different hashes")
	}
	upload("blog")
	views, err = server.service.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 {
		t.Fatalf("a new namespace must add an asset, got %d", len(views))
	}
}
