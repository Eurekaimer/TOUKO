package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func TestCloudflareFailureDoesNotRepeatGitHubPublication(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "lost_ref_response"}[lostResponse], func(t *testing.T) {
			api, git := newAPI(t)
			svc := service(t)
			ctx := context.Background()
			location := repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}
			if err := svc.Select(ctx, location); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Initialize(ctx, git); err != nil {
				t.Fatal(err)
			}
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
			defer origin.Close()
			cfg := r2.Config{Enabled: true, Endpoint: origin.URL, Bucket: "images", PublicBaseURL: "https://img.example.test"}
			config := map[string]any{"backend": "github", "owner": "owner", "name": "gallery", "branch": "main", "cloudflare": cfg}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(svc.Dir, "config.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			svc.NewCloudflare = func(_ context.Context, c r2.Config) (r2.Mirror, error) {
				return r2.NewWithClient(c, "fixture-access", "fixture-secret", origin.Client())
			}
			asset := stageImage(t, svc)
			api.mu.Lock()
			api.loseRef = lostResponse
			api.mu.Unlock()
			result, err := svc.Commit(ctx, git, "GitHub stays authoritative")
			if err != nil {
				t.Fatal(err)
			}
			if result.Cloudflare == nil || result.Cloudflare.Status != "failed" {
				t.Fatalf("mirror failure missing: %+v", result)
			}
			head, err := api.git.Head(ctx)
			if err != nil || head != result.Head {
				t.Fatal(head, result.Head, err)
			}
			snapshot, err := repository.ReadSnapshot(ctx, api.git, head)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Assets[asset.ID].Object != asset.Object {
				t.Fatal("GitHub image lost after R2 failure")
			}
			if _, err = repository.VerifySnapshot(ctx, api.git, snapshot); err != nil {
				t.Fatal(err)
			}
			state, err := svc.Store.Load(ctx)
			if err != nil || len(state.Changes) != 0 || state.Publication != nil {
				t.Fatal(state, err)
			}
			fresh := service(t)
			if _, err = fresh.Sync(ctx, git); err != nil {
				t.Fatal(err)
			}
			view, err := fresh.Show(ctx, asset.ID.String())
			if err != nil || view.Asset.Object != asset.Object {
				t.Fatal(view, err)
			}
			if _, err = svc.CloudflareSync(ctx, git, app.CloudflareOptions{}); err == nil {
				t.Fatal("R2 outage reported as sync success")
			}
			after, err := api.git.Head(ctx)
			if err != nil || after != head {
				t.Fatal("mirror retry repeated GitHub publication", after, head, err)
			}
		})
	}
}
