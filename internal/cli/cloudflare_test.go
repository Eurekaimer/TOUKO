package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestCloudflareCLIUnavailableAndArguments(t *testing.T) {
	dir := deliveryState(t, false)
	exit, out, stderr := deliveryRun(t, dir, deliveryPNG(t), "add", "-", "--output", "id")
	if exit != 0 {
		t.Fatal(exit, out, stderr)
	}
	id := strings.TrimSpace(out)
	for _, args := range [][]string{{"url", id, "--provider", "cloudflare", "--json"}, {"cloudflare", "sync", "--json"}} {
		exit, out, stderr = deliveryRun(t, dir, nil, args...)
		if exit != 1 || out != "" || !strings.Contains(stderr, `"code":"delivery_unavailable"`) {
			t.Fatal(args, exit, out, stderr)
		}
	}
	for _, args := range [][]string{{"cloudflare", "sync", "extra", "--json"}, {"cloudflare", "sync", "--dry-run=wrong", "--json"}} {
		exit, out, stderr = deliveryRun(t, dir, nil, args...)
		if exit != 2 || out != "" || !strings.Contains(stderr, `"code":"usage"`) {
			t.Fatal(args, exit, out, stderr)
		}
	}
}

type cloudflareCLITransport struct {
	git        repository.Git
	originHost string
	tls        http.RoundTripper
}

func (tr cloudflareCLITransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == tr.originHost {
		return tr.tls.RoundTrip(req)
	}
	if req.URL.Host != "api.github.com" {
		return nil, io.EOF
	}
	ctx := req.Context()
	path := strings.TrimPrefix(req.URL.Path, "/repos/example/gallery")
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/json")
	respond := func(value any, err error) (*http.Response, error) {
		if err != nil {
			return nil, err
		}
		_ = json.NewEncoder(recorder).Encode(value)
		return recorder.Result(), nil
	}
	switch {
	case path == "/git/ref/heads/main":
		head, err := tr.git.Head(ctx)
		return respond(map[string]any{"object": map[string]any{"sha": head, "type": "commit"}}, err)
	case strings.HasPrefix(path, "/git/commits/"):
		commit, err := tr.git.Commit(ctx, domain.RepositoryHead(strings.TrimPrefix(path, "/git/commits/")))
		return respond(map[string]any{"tree": map[string]any{"sha": commit.Tree}, "parents": []any{}}, err)
	case strings.HasPrefix(path, "/git/trees/"):
		entries, err := tr.git.Tree(ctx, repository.TreeID(strings.TrimPrefix(path, "/git/trees/")))
		tree := []any{}
		for _, entry := range entries {
			tree = append(tree, map[string]any{"path": entry.Path, "sha": entry.SHA, "mode": entry.Mode, "type": entry.Type, "size": entry.Size})
		}
		return respond(map[string]any{"tree": tree, "truncated": false}, err)
	case strings.HasPrefix(path, "/git/blobs/"):
		reader, err := tr.git.OpenBlob(ctx, repository.BlobID(strings.TrimPrefix(path, "/git/blobs/")))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader, Request: req}, nil
	default:
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
}

func TestCloudflareCLIPartialFailureEnvelope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	service, err := app.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	location := repository.Location{Backend: "github", Owner: "example", Name: "gallery", Branch: "main"}
	if err = service.Select(ctx, location); err != nil {
		t.Fatal(err)
	}
	git, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Initialize(ctx, git); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Add(ctx, []app.Input{{Reader: bytes.NewReader(deliveryPNG(t)), Name: "image.png"}}, app.AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Commit(ctx, git, "published without R2"); err != nil {
		t.Fatal(err)
	}
	if err = service.Close(); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer origin.Close()
	cfg := r2.Config{Enabled: true, Endpoint: origin.URL, Bucket: "images", PublicBaseURL: "https://img.example.test"}
	data, err := json.Marshal(map[string]any{"backend": "github", "owner": "example", "name": "gallery", "branch": "main", "cloudflare": cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "github-sentinel")
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "r2-access-sentinel")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "r2-secret-sentinel")
	old := http.DefaultTransport
	http.DefaultTransport = cloudflareCLITransport{git: git, originHost: strings.TrimPrefix(origin.URL, "https://"), tls: origin.Client().Transport}
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, dryRun := range []bool{false, true} {
		args := []string{"cloudflare", "sync", "--json"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		exit, out, stderr := deliveryRun(t, dir, nil, args...)
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			}
			Cloudflare *app.CloudflareResult `json:"cloudflare"`
		}
		if err = json.Unmarshal([]byte(stderr), &envelope); err != nil {
			t.Fatal(err, stderr)
		}
		if exit != 1 || out != "" || envelope.Error.Code != "cloudflare_sync_failed" || envelope.Cloudflare == nil || envelope.Cloudflare.Failed != 1 || envelope.Cloudflare.DryRun != dryRun {
			t.Fatal(strconv.FormatBool(dryRun), exit, out, stderr)
		}
		for _, secret := range []string{"github-sentinel", "r2-access-sentinel", "r2-secret-sentinel"} {
			if strings.Contains(stderr, secret) {
				t.Fatal("credential disclosed")
			}
		}
	}
}
