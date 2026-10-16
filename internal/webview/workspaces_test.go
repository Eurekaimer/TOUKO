package webview

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestWorkspaceSwitchPreservesUnpublishedChanges(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() {
		if err := f.server.spaces.close(); err != nil {
			t.Error(err)
		}
	})
	f.setup()
	firstID, _ := f.add()
	response := f.request("POST", "/api/repo", map[string]any{"backend": "localgit", "path": filepath.Join(f.root, "other.git"), "create": true}, http.StatusOK)
	var selected struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	var second struct {
		CSRF    string          `json:"csrf"`
		Assets  []app.AssetView `json:"assets"`
		Changes []domain.Change `json:"changes"`
	}
	if err := json.Unmarshal(f.request("GET", selected.URL+"api/state", nil, http.StatusOK).Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Assets) != 0 || len(second.Changes) != 0 || second.CSRF == f.csrf {
		t.Fatalf("new workspace inherited gallery or mutation credentials: %+v", second)
	}
	// A valid mutation token from another workspace must not authorize this one.
	f.request("POST", selected.URL+"api/reset", map[string]bool{"confirm": true}, http.StatusForbidden)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "second.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(imageBytes(t)); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("title", "second workspace only"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", testOrigin+selected.URL+"api/upload", &body)
	request.Header.Set("Origin", testOrigin)
	request.Header.Set("X-Touko-CSRF", second.CSRF)
	request.Header.Set("Content-Type", form.FormDataContentType())
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	if out.Code != http.StatusOK {
		t.Fatalf("second upload: %d %s", out.Code, out.Body.String())
	}
	if err := json.Unmarshal(f.request("GET", selected.URL+"api/state", nil, http.StatusOK).Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Assets) != 1 || second.Assets[0].Asset.Title != "second workspace only" || len(second.Changes) != 1 {
		t.Fatalf("second workspace did not retain its own pending image: %+v", second)
	}
	var first struct {
		Assets  []app.AssetView `json:"assets"`
		Changes []domain.Change `json:"changes"`
	}
	if err := json.Unmarshal(f.request("GET", "/w/default/api/state", nil, http.StatusOK).Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Assets) != 1 || first.Assets[0].Asset.ID.String() != firstID || len(first.Changes) != 1 {
		t.Fatalf("switch changed the first workspace's unpublished work: %+v", first)
	}
	f.request("GET", "/w/not-a-workspace/api/state", nil, http.StatusNotFound)
	f.request("GET", "/w/../../config.json", nil, http.StatusNotFound)
}

func TestImportedPreviewUsesImmutableLocalGit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	remote, err := localgit.Init(ctx, filepath.Join(f.root, "picgo.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	location := repository.Location{Backend: "github", Owner: "example", Name: "picgo", Branch: "main"}
	if err := f.service.Select(ctx, location); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Initialize(ctx, remote); err != nil {
		t.Fatal(err)
	}
	imagePath := "img/旧图 没有后缀"
	original := imageBytes(t)
	publish := func(data []byte) {
		t.Helper()
		base, err := remote.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		commit, err := remote.Commit(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		blob, err := remote.CreateBlob(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		tree, err := remote.CreateTree(ctx, commit.Tree, []repository.TreeChange{{Path: imagePath, Blob: &blob}})
		if err != nil {
			t.Fatal(err)
		}
		next, err := remote.CreateCommit(ctx, tree, base, "external PicGo upload")
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.UpdateRef(ctx, base, next); err != nil {
			t.Fatal(err)
		}
	}
	publish(original)
	if _, err := f.service.Sync(ctx, remote); err != nil {
		t.Fatal(err)
	}
	cloneDir := repository.CloneDir(f.service.Dir)
	if err := localgit.Clone(ctx, (&url.URL{Scheme: "file", Path: remotePath(f.root)}).String(), cloneDir, "main", ""); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/api/import", map[string]any{"directory": "img", "namespace": "legacy", "tags": []string{}, "collections": []string{}}, http.StatusOK)
	views, err := f.service.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Asset.Path != imagePath {
		t.Fatalf("wrong imported image: %+v", views)
	}
	previewPath := "/api/assets/" + views[0].Asset.ID.String() + "/image"
	if got := f.request("GET", previewPath, nil, http.StatusOK).Body.Bytes(); !bytes.Equal(got, original) {
		t.Fatal("metadata-only import could not preview its original local Git bytes")
	}
	// A same-sized replacement at a mutable path must not change a gallery's
	// immutable preview until that gallery actually synchronizes its metadata.
	replacement := bytes.Clone(original)
	replacement[len(replacement)-1] ^= 1
	publish(replacement)
	if err := localgit.Update(ctx, cloneDir, "main", ""); err != nil {
		t.Fatal(err)
	}
	checkout, err := os.ReadFile(filepath.Join(cloneDir, filepath.FromSlash(imagePath)))
	if err != nil || !bytes.Equal(checkout, replacement) {
		t.Fatalf("checkout did not advance: %v", err)
	}
	if got := f.request("GET", previewPath, nil, http.StatusOK).Body.Bytes(); !bytes.Equal(got, original) {
		t.Fatal("preview followed a mutable PicGo path instead of the indexed Git snapshot")
	}
}

func remotePath(root string) string { return filepath.Join(root, "picgo.git") }
