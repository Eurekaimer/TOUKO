package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func deliveryState(t *testing.T, private bool) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, e := app.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	l := repository.Location{Backend: "github", Owner: "example", Name: "gallery", Branch: "feature/demo", Private: private}
	if e = s.Select(ctx, l); e != nil {
		t.Fatal(e)
	}
	r, e := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Initialize(ctx, r); e != nil {
		t.Fatal(e)
	}
	return dir
}
func deliveryRun(t *testing.T, dir string, input []byte, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	argv := append([]string{"--state-dir", dir}, args...)
	exit := Run(context.Background(), argv, bytes.NewReader(input), &out, &stderr)
	return exit, out.String(), stderr.String()
}
func deliveryPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if e := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestMarkdownAndProspectiveURL(t *testing.T) {
	dir := deliveryState(t, false)
	exit, out, stderr := deliveryRun(t, dir, deliveryPNG(t), "add", "-", "--name", `a[b]\c.png`, "--namespace", "blog", "--output", "markdown")
	if exit != 0 || !strings.HasPrefix(out, `![a\[b\]\\c.png](https://cdn.jsdelivr.net/gh/example/gallery@feature%2Fdemo/objects/blog/`) || !strings.Contains(stderr, "not published") {
		t.Fatal(exit, out, stderr)
	}
	s, e := app.Open(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	views, e := s.List(context.Background())
	s.Close()
	if e != nil || len(views) != 1 {
		t.Fatal(views, e)
	}
	exit, out, stderr = deliveryRun(t, dir, nil, "url", views[0].Asset.ID.String(), "--provider", "github", "--json")
	var got struct {
		URL       string `json:"url"`
		Committed bool   `json:"committed"`
	}
	if e = json.Unmarshal([]byte(out), &got); e != nil || exit != 0 || got.Committed || !strings.HasPrefix(got.URL, "https://raw.githubusercontent.com/example/gallery/refs/heads/feature%2Fdemo/") || stderr != "" {
		t.Fatal(exit, out, stderr, e)
	}
}
func TestMarkdownPreflightPreservesStage(t *testing.T) {
	dir := deliveryState(t, true)
	for _, args := range [][]string{{"add", "-", "--output", "markdown"}, {"add", "-", "--output", "markdown", "--json"}, {"add", "-", "--output", "id", "--json"}} {
		exit, _, _ := deliveryRun(t, dir, deliveryPNG(t), args...)
		if exit == 0 {
			t.Fatal("invalid request accepted", args)
		}
	}
	s, e := app.Open(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	st, e := s.Store.Load(context.Background())
	if e != nil || len(st.Changes) != 0 {
		t.Fatal(st.Changes, e)
	}
}
func TestGitHubUsageAndMissingAuth(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	for _, args := range [][]string{{"repo", "use"}, {"repo", "use", "example/gallery", "--local", "/tmp/not-used"}, {"repo", "use", "bad/name/extra"}} {
		exit, _, _ := deliveryRun(t, t.TempDir(), nil, args...)
		if exit != 2 {
			t.Fatal(args, exit)
		}
	}
	exit, out, stderr := deliveryRun(t, t.TempDir(), nil, "auth", "status", "--json")
	if exit != 1 || out != "" || !strings.Contains(stderr, `"code":"unauthorized"`) {
		t.Fatal(exit, out, stderr)
	}
}
