package webview

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestPreviewServesLocalFileWhenCloneHeadMissing(t *testing.T) {
	f := newFixture(t)
	f.setup()
	ctx := t.Context()
	f.add()
	f.request("POST", "/api/commit", map[string]string{"message": "first"}, http.StatusOK)

	clone := repository.CloneDir(f.service.Dir)
	remote := (&url.URL{Scheme: "file", Path: filepath.Join(f.root, "gallery.git")}).String()
	if err := localgit.Clone(ctx, remote, clone, "main", ""); err != nil {
		t.Fatal(err)
	}

	id, data := f.add()
	f.request("POST", "/api/commit", map[string]string{"message": "second"}, http.StatusOK)
	view, err := f.service.Show(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	asset := view.Asset
	path := filepath.Join(clone, filepath.FromSlash(repository.ObjectPath(asset)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.service.Dir, "config.json"), []byte(`{"backend":"github","owner":"owner","name":"gallery","branch":"main"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := f.request("GET", "/api/assets/"+id+"/image", nil, http.StatusOK).Body.Bytes()
	if !bytes.Equal(got, data) {
		t.Fatal("preview did not use the verified working-copy file")
	}
}

func TestPreviewServesLocalBlobWhenCloneHeadMissing(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "indexed Git blob without SHA-256"
		if legacy {
			name = "older index with recorded SHA-256"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.setup()
			data := imageBytes(t)
			var options map[string]string
			if legacy {
				options = map[string]string{"compute_hash": "true"}
			}
			f.uploadOptions([][]byte{data}, options, http.StatusOK)
			ctx := t.Context()
			f.request("POST", "/api/commit", map[string]string{"message": "publish"}, http.StatusOK)
			_, views, err := f.service.ReadView(ctx)
			if err != nil {
				t.Fatal(err)
			}
			asset := views[0].Asset
			if asset.Object.HasHash() != legacy {
				t.Fatalf("upload SHA-256 enabled = %v, want %v", asset.Object.HasHash(), legacy)
			}
			id := asset.ID.String()
			clone := repository.CloneDir(f.service.Dir)
			remote := (&url.URL{Scheme: "file", Path: filepath.Join(f.root, "gallery.git")}).String()
			if err := localgit.Clone(ctx, remote, clone, "main", ""); err != nil {
				t.Fatal(err)
			}
			f.request("POST", "/api/assets/"+id+"/edit", map[string]string{"title": "new metadata"}, http.StatusOK)
			f.request("POST", "/api/commit", map[string]string{"message": "edit title"}, http.StatusOK)
			st, err := f.service.Store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			local, err := localgit.Open(ctx, clone, "main")
			if err != nil {
				t.Fatal(err)
			}
			if local.HasObject(ctx, string(st.Head)) {
				t.Fatal("clone unexpectedly contains the newer indexed commit")
			}
			if legacy {
				if _, err := f.service.Store.DB.ExecContext(ctx, "DELETE FROM image_blobs"); err != nil {
					t.Fatal(err)
				}
			}
			wrong := bytes.Clone(data)
			wrong[len(wrong)-1] ^= 1
			if err := os.WriteFile(filepath.Join(clone, filepath.FromSlash(repository.ObjectPath(asset))), wrong, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.service.Dir, "config.json"), []byte(`{"backend":"github","owner":"owner","name":"gallery","branch":"main"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if got := f.request("GET", "/api/assets/"+id+"/image", nil, http.StatusOK).Body.Bytes(); !bytes.Equal(got, data) {
				t.Fatal("preview did not use the immutable local image")
			}
		})
	}
}
