package webview

import (
	"bytes"
	"encoding/json"
	"errors"
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

// A target that is already loaded must decide from its projected bytes; the
// same object path with identical bytes is reused even at the waterline, while
// a different object at that path is never admitted.
func TestUploadTargetAcceptance(t *testing.T) {
	ctx := t.Context()
	git, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	object := domain.ContentObject{Size: 10}
	target := &uploadTarget{reader: git, projected: libraryCapacityLimit - 10, paths: map[string]domain.ContentObject{}}
	if !target.tryAccept(ctx, app.PreparedInput{Asset: domain.Asset{Path: "a.webp", Object: object}}) {
		t.Fatal("object within the waterline was rejected")
	}
	object.Size = 11
	if target.tryAccept(ctx, app.PreparedInput{Asset: domain.Asset{Path: "b.webp", Object: object}}) {
		t.Fatal("object past the waterline was accepted")
	}
	object.Size = 10
	reuse := &uploadTarget{reader: git, projected: libraryCapacityLimit, paths: map[string]domain.ContentObject{"a.webp": object}}
	if !reuse.tryAccept(ctx, app.PreparedInput{Asset: domain.Asset{Path: "a.webp", Object: object}}) {
		t.Fatal("identical object at the same path was not reused")
	}
	other := domain.ContentObject{Size: 10, MIME: "image/webp"}
	if reuse.tryAccept(ctx, app.PreparedInput{Asset: domain.Asset{Path: "a.webp", Object: other}}) {
		t.Fatal("a different object at an occupied path was accepted")
	}
}

func TestLibraryUploadUsesAuthoritativeReaderWhenCloneMissingOrStale(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "stale"}[stale], func(t *testing.T) {
			if stale {
				// The fake GitHub API is in-process. Refuse external Git traffic
				// so this case deterministically exercises the API fallback.
				t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
				t.Setenv("https_proxy", "http://127.0.0.1:1")
				t.Setenv("NO_PROXY", "")
				t.Setenv("no_proxy", "")
			}
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

func TestGitCapacityReaderFetchesWithoutMovingCheckoutAndRechecksHead(t *testing.T) {
	ctx := t.Context()
	remote := filepath.Join(t.TempDir(), "remote.git")
	git, err := localgit.Init(ctx, remote, "main")
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
	old, err := git.CreateCommit(ctx, tree, "", "old")
	if err != nil {
		t.Fatal(err)
	}
	if err = git.UpdateRef(ctx, "", old); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if err = localgit.Clone(ctx, remote, clone, "main", ""); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(clone, "untracked")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := localgit.Open(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	next, err := git.CreateCommit(ctx, tree, old, "next")
	if err != nil {
		t.Fatal(err)
	}
	if err = git.UpdateRef(ctx, old, next); err != nil {
		t.Fatal(err)
	}
	reader, err := gitCapacityReader(ctx, local, clone, remote, "main", "", next)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Head(ctx); err != nil || got != next {
		t.Fatalf("initial head: %s %v", got, err)
	}
	if _, err := reader.Commit(ctx, next); err != nil {
		t.Fatalf("fetched commit: %v", err)
	}
	if got, err := local.Head(ctx); err != nil || got != old {
		t.Fatalf("checkout moved: %s %v", got, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("untracked file changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("fetch wrote FETCH_HEAD: %v", err)
	}
	latest, err := git.CreateCommit(ctx, tree, next, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err = git.UpdateRef(ctx, next, latest); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Head(ctx); err != nil || got != latest {
		t.Fatalf("head was not rechecked: %s %v", got, err)
	}
	if _, err := gitCapacityReader(ctx, local, clone, remote, "main", "", next); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale base was accepted: %v", err)
	}
}
