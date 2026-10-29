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

func TestLibraryUploadUsesAuthoritativeReaderWhenCloneMissingOrStale(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "stale"}[stale], func(t *testing.T) {
			f := newCloudflareFixture(t, false)
			ctx := t.Context()
			clone := repository.CloneDir(f.service.Dir)
			if stale {
				path := filepath.Join(t.TempDir(), "other.git")
				git, err := localgit.Init(ctx, path, "main")
				if err != nil {
					t.Fatal(err)
				}
				blob, err := git.CreateBlob(ctx, strings.NewReader("old"), 3)
				if err != nil {
					t.Fatal(err)
				}
				tree, err := git.CreateTree(ctx, "", []repository.TreeChange{{Path: "README", Blob: &blob}})
				if err != nil {
					t.Fatal(err)
				}
				head, err := git.CreateCommit(ctx, tree, "", "old")
				if err != nil {
					t.Fatal(err)
				}
				if err = git.UpdateRef(ctx, "", head); err != nil {
					t.Fatal(err)
				}
				if err = localgit.Clone(ctx, path, clone, "main", ""); err != nil {
					t.Fatal(err)
				}
			}
			library := libraryView{ID: "0123456789abcdef0123456789abcdef", Name: "test", Workspaces: []string{"default"}}
			if err := f.server.spaces.saveViews(viewConfig{Version: viewsVersion, Active: activeView{Kind: "library", ID: library.ID}, Libraries: []libraryView{library}}); err != nil {
				t.Fatal(err)
			}
			csrf, err := f.server.spaces.libraryCSRF(library.ID)
			if err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			file, err := form.CreateFormFile("files", "new.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = file.Write(imageBytes(t)); err != nil {
				t.Fatal(err)
			}
			if err = form.WriteField("namespace", "default"); err != nil {
				t.Fatal(err)
			}
			if err = form.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", testOrigin+libraryURL(library.ID)+"api/upload", &body)
			request.Header.Set("Origin", testOrigin)
			request.Header.Set("X-Touko-CSRF", csrf)
			request.Header.Set("Content-Type", form.FormDataContentType())
			out := httptest.NewRecorder()
			f.server.ServeHTTP(out, request)
			if out.Code != http.StatusOK {
				t.Fatalf("upload: %d %s", out.Code, out.Body.String())
			}
			var result struct {
				Results []json.RawMessage `json:"results"`
				Failed  []json.RawMessage `json:"failed"`
			}
			if err = json.Unmarshal(out.Body.Bytes(), &result); err != nil || len(result.Results) != 1 || len(result.Failed) != 0 {
				t.Fatalf("upload response: %s %v", out.Body.String(), err)
			}
			if !stale {
				if _, err := os.Stat(clone); !os.IsNotExist(err) {
					t.Fatalf("upload created clone: %v", err)
				}
			}
			state, err := f.service.Store.Load(ctx)
			if err != nil || len(state.Changes) != 1 {
				t.Fatalf("staging: %v", err)
			}
		})
	}
}
