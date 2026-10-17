package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
)

type invocation struct {
	code int
	out  string
	err  string
}

func invoke(dir string, input []byte, args ...string) invocation {
	var out, err bytes.Buffer
	argv := append([]string{"--state-dir", dir}, args...)
	code := Run(context.Background(), argv, bytes.NewReader(input), &out, &err)
	return invocation{code, out.String(), err.String()}
}

func succeed(t *testing.T, got invocation) string {
	t.Helper()
	if got.code != 0 || got.err != "" {
		t.Fatalf("command failed: exit=%d stdout=%q stderr=%q", got.code, got.out, got.err)
	}
	return got.out
}

func picture(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	im.SetNRGBA(1, 1, color.NRGBA{R: 220, G: 30, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func setupCLI(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir, repo := filepath.Join(root, "state"), filepath.Join(root, "gallery.git")
	var result app.SyncResult
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "repo", "init", "--local", repo, "--json"))), &result); err != nil {
		t.Fatal(err)
	}
	if result.Head == "" || result.AssetCount != 0 {
		t.Fatalf("invalid initialized snapshot: %+v", result)
	}
	return dir, repo
}

func assetList(t *testing.T, result invocation) []app.AssetView {
	t.Helper()
	var response struct {
		Assets []app.AssetView `json:"assets"`
	}
	if err := json.Unmarshal([]byte(succeed(t, result)), &response); err != nil {
		t.Fatal(err)
	}
	if response.Assets == nil {
		t.Fatal("assets must be a JSON array")
	}
	return response.Assets
}

func TestRunStagedAssetLifecycle(t *testing.T) {
	dir, _ := setupCLI(t)
	data := picture(t)
	name := filepath.Join(t.TempDir(), "夏の画像.png")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	added := assetList(t, invoke(dir, nil, "add", name, "--namespace", "blog", "--title", "Summer Demo", "--tag", "Forest", "--tag", "Forest", "--collection", "Travel", "--json"))
	if len(added) != 1 {
		t.Fatalf("added=%+v", added)
	}
	a := added[0]
	if a.State != "added" || a.Asset.OriginalName != "夏の画像.png" || a.Asset.Title != "Summer Demo" || a.Asset.Namespace != "blog" || len(a.Asset.Tags) != 1 || a.Asset.Tags[0] != "Forest" || a.Asset.Collections[0] != "Travel" {
		t.Fatalf("metadata lost: %+v", a)
	}
	id := a.Asset.ID.String()
	for _, query := range []string{"sUMMer", "夏の画像", "forest", "TRAVEL"} {
		found := assetList(t, invoke(dir, nil, "find", query, "--json"))
		if len(found) != 1 || found[0].Asset.ID != a.Asset.ID {
			t.Fatalf("query %q: %+v", query, found)
		}
	}
	if got := assetList(t, invoke(dir, nil, "ls", "--namespace", "notes", "--json")); len(got) != 0 {
		t.Fatalf("namespace filter: %+v", got)
	}
	var shown struct {
		Asset app.AssetView `json:"asset"`
	}
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "show", id[:8], "--json"))), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Asset.Asset.ID != a.Asset.ID || shown.Asset.State != "added" {
		t.Fatalf("show=%+v", shown)
	}
	succeed(t, invoke(dir, nil, "edit", id, "--title", "Edited", "--tag", "new", "--tag", "final", "--clear-collections"))
	views := assetList(t, invoke(dir, nil, "ls", "--json"))
	if len(views) != 1 || views[0].Asset.Title != "Edited" || strings.Join(views[0].Asset.Tags, ",") != "final,new" || len(views[0].Asset.Collections) != 0 || !views[0].Asset.CreatedAt.Equal(a.Asset.CreatedAt) {
		t.Fatalf("edit=%+v", views)
	}
	var status struct {
		Head    domain.RepositoryHead `json:"head"`
		Base    domain.RepositoryHead `json:"base_head"`
		Changes []domain.Change       `json:"changes"`
	}
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "status", "--json"))), &status); err != nil {
		t.Fatal(err)
	}
	if status.Head == "" || status.Base != status.Head || len(status.Changes) != 1 || status.Changes[0].Kind != domain.Add {
		t.Fatalf("status=%+v", status)
	}
	succeed(t, invoke(dir, nil, "rm", id))
	if views := assetList(t, invoke(dir, nil, "ls", "--json")); len(views) != 0 {
		t.Fatalf("rm overlay=%+v", views)
	}
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "status", "--json"))), &status); err != nil {
		t.Fatal(err)
	}
	if status.Base != "" || status.Changes == nil || len(status.Changes) != 0 {
		t.Fatalf("clean status=%+v", status)
	}
}

func TestRunFindMatchesContentHash(t *testing.T) {
	dir, _ := setupCLI(t)
	encode := func(colorValue color.NRGBA) []byte {
		im := image.NewNRGBA(image.Rect(0, 0, 2, 2))
		im.SetNRGBA(0, 0, colorValue)
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	files := filepath.Join(t.TempDir(), "hash-search")
	if err := os.MkdirAll(files, 0700); err != nil {
		t.Fatal(err)
	}
	var added []app.AssetView
	for i, data := range [][]byte{encode(color.NRGBA{R: 10, G: 200, B: 30, A: 255}), encode(color.NRGBA{R: 200, G: 20, B: 90, A: 255})} {
		path := filepath.Join(files, string(rune('a'+i))+".png")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		added = append(added, assetList(t, invoke(dir, nil, "add", path, "--json"))...)
	}
	if len(added) != 2 || added[0].Asset.Object.Hash == added[1].Asset.Object.Hash {
		t.Fatalf("added=%+v", added)
	}
	hash := added[0].Asset.Object.Hash.String()
	if strings.Contains(strings.ToLower(added[0].Asset.Title+added[0].Asset.OriginalName+added[1].Asset.Title+added[1].Asset.OriginalName), hash) {
		t.Fatal("titles and names must not contain the hash")
	}
	found := assetList(t, invoke(dir, nil, "find", hash, "--json"))
	if len(found) != 1 || found[0].Asset.ID != added[0].Asset.ID {
		t.Fatalf("full hash=%+v", found)
	}
	prefix := assetList(t, invoke(dir, nil, "find", strings.ToUpper(hash[:12]), "--json"))
	if len(prefix) != 1 || prefix[0].Asset.ID != added[0].Asset.ID {
		t.Fatalf("upper prefix=%+v", prefix)
	}
}

func TestRunInputValidationPrecedesMutation(t *testing.T) {
	dir, _ := setupCLI(t)
	pngData := picture(t)
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, pngData, 0600); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"add", path, "--output", "id", "--json"},
		{"add", path, "--output", "markdown", "--json"},
		{"add", path, "--output", "unknown"},
		{"add", path, "--name", "wrong.png"},
		{"add", "-", path},
		{"add", "-", "-"},
		{"add", path, "--namespace", "../bad"},
		{"edit", "12345678", "--tag", "x", "--clear-tags"},
		{"edit", "12345678", "--collection", "x", "--clear-collections"},
		{"reset"},
		{"find", ""},
		{"find", "  "},
		{"show"},
		{"unknown"},
		{"add", path, "--unknown", "--json"},
	}
	for _, args := range cases {
		got := invoke(dir, pngData, args...)
		if got.code != 2 || got.out != "" || got.err == "" {
			t.Fatalf("%v: %+v", args, got)
		}
		if strings.Contains(strings.Join(args, " "), "--json") {
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(got.err), &body); err != nil {
				t.Fatalf("%v: invalid JSON error: %v", args, err)
			}
			if body.Error.Code != "usage" {
				t.Fatalf("%v: code=%q", args, body.Error.Code)
			}
		}
	}
	if views := assetList(t, invoke(dir, nil, "ls", "--json")); len(views) != 0 {
		t.Fatalf("validation staged assets=%+v", views)
	}
}

func TestRunStdinBatchAndReset(t *testing.T) {
	dir, _ := setupCLI(t)
	data := picture(t)
	id := strings.TrimSpace(succeed(t, invoke(dir, data, "add", "-", "--output", "id")))
	if _, err := domain.ParseAssetID(id); err != nil {
		t.Fatalf("id-only output %q: %v", id, err)
	}
	views := assetList(t, invoke(dir, nil, "ls", "--json"))
	if len(views) != 1 || views[0].Asset.OriginalName != "stdin.png" || views[0].Asset.Title != "stdin.png" {
		t.Fatalf("stdin metadata=%+v", views)
	}
	succeed(t, invoke(dir, nil, "reset", "--staged"))
	views = assetList(t, invoke(dir, data, "add", "-", "--name", "剪贴板.png", "--json"))
	if len(views) != 1 || views[0].Asset.OriginalName != "剪贴板.png" {
		t.Fatalf("named stdin=%+v", views)
	}
	succeed(t, invoke(dir, nil, "reset", "--staged"))
	root := t.TempDir()
	good, bad := filepath.Join(root, "good.png"), filepath.Join(root, "bad.png")
	if err := os.WriteFile(good, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, second := range []string{bad, filepath.Join(root, "missing.png")} {
		got := invoke(dir, nil, "add", good, second, "--json")
		if got.code != 1 || got.out != "" {
			t.Fatalf("batch failure=%+v", got)
		}
		if views := assetList(t, invoke(dir, nil, "ls", "--json")); len(views) != 0 {
			t.Fatalf("partial batch=%+v", views)
		}
	}
	batch := assetList(t, invoke(dir, nil, "add", good, good, "--json"))
	if len(batch) != 2 || batch[0].Asset.ID == batch[1].Asset.ID || batch[0].Asset.Object.Hash != batch[1].Asset.Object.Hash {
		t.Fatalf("duplicate bytes=%+v", batch)
	}
	var status struct {
		Changes []domain.Change `json:"changes"`
	}
	if err := json.Unmarshal([]byte(succeed(t, invoke(dir, nil, "status", "--json"))), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Changes) != 2 || status.Changes[0].AssetID.String() >= status.Changes[1].AssetID.String() {
		t.Fatalf("unstable status ordering=%+v", status)
	}
}

func TestRunRepositorySelectionSyncAndErrorStreams(t *testing.T) {
	dir, repo := setupCLI(t)
	ctx := context.Background()
	git, err := localgit.Open(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	succeed(t, invoke(fresh, nil, "repo", "use", "--local", repo, "--branch", "main"))
	after, err := git.Head(ctx)
	if err != nil || after != head {
		t.Fatalf("use wrote remote head=%s want=%s err=%v", after, head, err)
	}
	var synced app.SyncResult
	if err := json.Unmarshal([]byte(succeed(t, invoke(fresh, nil, "sync", "--json"))), &synced); err != nil {
		t.Fatal(err)
	}
	if synced.Head != head || synced.AssetCount != 0 {
		t.Fatalf("sync=%+v", synced)
	}
	succeed(t, invoke(fresh, nil, "repo", "init"))
	after, err = git.Head(ctx)
	if err != nil || after != head {
		t.Fatalf("idempotent init head=%s want=%s err=%v", after, head, err)
	}
	got := invoke(dir, nil, "show", "12345678", "--json")
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(got.err), &response); err != nil {
		t.Fatal(err)
	}
	if got.code != 1 || got.out != "" || response.Error.Code != "asset_not_found" {
		t.Fatalf("not-found=%+v body=%+v", got, response)
	}
	other := filepath.Join(t.TempDir(), "other.git")
	if _, err := localgit.Init(ctx, other, "main"); err != nil {
		t.Fatal(err)
	}
	got = invoke(dir, nil, "repo", "use", "--local", other, "--json")
	if err := json.Unmarshal([]byte(got.err), &response); err != nil {
		t.Fatal(err)
	}
	if got.code != 3 || got.out != "" || response.Error.Code != "conflict" {
		t.Fatalf("selection conflict=%+v", got)
	}
}

func TestRunHumanOutputAndEditFieldPresence(t *testing.T) {
	dir, _ := setupCLI(t)
	views := assetList(t, invoke(dir, picture(t), "add", "-", "--namespace", "notes", "--title", "Visible title", "--tag", "keep", "--collection", "keep collection", "--json"))
	if len(views) != 1 {
		t.Fatalf("added=%+v", views)
	}
	a := views[0].Asset
	for _, args := range [][]string{{"ls"}, {"show", a.ID.String()}, {"find", "visible"}} {
		out := succeed(t, invoke(dir, nil, args...))
		for _, value := range []string{a.ID.String(), "added", "notes", "Visible title", a.Object.Hash.String()} {
			if !strings.Contains(out, value) {
				t.Fatalf("%v omitted %q: %q", args, value, out)
			}
		}
	}
	succeed(t, invoke(dir, nil, "edit", a.ID.String(), "--title", ""))
	views = assetList(t, invoke(dir, nil, "ls", "--json"))
	if len(views) != 1 || views[0].Asset.Title != "" || strings.Join(views[0].Asset.Tags, ",") != "keep" || strings.Join(views[0].Asset.Collections, ",") != "keep collection" {
		t.Fatalf("unspecified fields changed: %+v", views)
	}
	succeed(t, invoke(dir, nil, "edit", a.ID.String(), "--clear-tags"))
	views = assetList(t, invoke(dir, nil, "ls", "--json"))
	if len(views) != 1 || views[0].Asset.Tags == nil || len(views[0].Asset.Tags) != 0 || strings.Join(views[0].Asset.Collections, ",") != "keep collection" {
		t.Fatalf("explicit clear did not preserve other fields: %+v", views)
	}
}
