package webview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	gh "github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const cloudflareTestConfig = `{"enabled":true,"endpoint":"https://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.test"}`

// cloudflareObject stores exactly the metadata the application must verify.
type cloudflareObject struct {
	etag         string
	sha256       string
	repositoryID string
	sourceID     string
	size         int64
	data         []byte
}

type cloudflareMirror struct {
	mu       sync.Mutex
	objects  map[string]cloudflareObject
	failPuts map[string]error
	seq      int
}

func newCloudflareMirror() *cloudflareMirror {
	return &cloudflareMirror{objects: make(map[string]cloudflareObject), failPuts: make(map[string]error)}
}

func (m *cloudflareMirror) HeadObject(ctx context.Context, key string) (r2.ObjectInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return r2.ObjectInfo{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok {
		return r2.ObjectInfo{}, false, nil
	}
	return r2.ObjectInfo{
		ETag: object.etag, RepositoryID: object.repositoryID, SourceID: object.sourceID,
		SHA256: object.sha256, Size: object.size,
	}, true, nil
}

func (m *cloudflareMirror) PutObject(ctx context.Context, key string, body io.ReadSeeker, opts r2.PutOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = m.failPuts[key]; err != nil {
		return "", err
	}
	m.seq++
	etag := fmt.Sprintf("etag-%d", m.seq)
	m.objects[key] = cloudflareObject{
		etag: etag, sha256: opts.Object.Hash.String(),
		repositoryID: opts.RepositoryID.String(), sourceID: opts.SourceID,
		size: opts.Object.Size, data: append([]byte(nil), data...),
	}
	return etag, nil
}

func (m *cloudflareMirror) DeleteObject(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *cloudflareMirror) remove(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
}

func (m *cloudflareMirror) failPut(key string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failPuts[key] = err
}

// cloudflareGitHub serves the read-only Git Data API from a real bare
// repository, so snapshots and image bytes come from actual Git objects.
type cloudflareGitHub struct {
	t   *testing.T
	git *localgit.Repository
}

func (a *cloudflareGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	respond := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	fail := func(err error) {
		w.WriteHeader(http.StatusInternalServerError)
		respond(map[string]string{"message": err.Error()})
	}
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/repos/owner/gallery")
	switch {
	case r.Method == http.MethodGet && path == "/git/ref/heads/main":
		head, err := a.git.Head(ctx)
		if err != nil {
			fail(err)
			return
		}
		respond(map[string]any{"object": map[string]any{"sha": head, "type": "commit"}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/git/commits/"):
		commit, err := a.git.Commit(ctx, domain.RepositoryHead(strings.TrimPrefix(path, "/git/commits/")))
		if err != nil {
			fail(err)
			return
		}
		parents := make([]any, 0, len(commit.Parents))
		for _, parent := range commit.Parents {
			parents = append(parents, map[string]any{"sha": parent})
		}
		respond(map[string]any{"tree": map[string]any{"sha": commit.Tree}, "parents": parents})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/git/trees/"):
		entries, err := a.git.Tree(ctx, repository.TreeID(strings.TrimPrefix(path, "/git/trees/")))
		if err != nil {
			fail(err)
			return
		}
		tree := make([]any, 0, len(entries))
		for _, entry := range entries {
			tree = append(tree, map[string]any{"path": entry.Path, "mode": entry.Mode, "type": entry.Type, "sha": entry.SHA, "size": entry.Size})
		}
		respond(map[string]any{"tree": tree, "truncated": false})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/git/blobs/"):
		body, err := a.git.OpenBlob(ctx, repository.BlobID(strings.TrimPrefix(path, "/git/blobs/")))
		if err != nil {
			fail(err)
			return
		}
		defer body.Close()
		if _, err = io.Copy(w, body); err != nil {
			a.t.Errorf("serve blob: %v", err)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		respond(map[string]string{"message": "unexpected test GitHub request " + r.Method + " " + r.URL.Path})
	}
}

type cloudflareTransport struct {
	handler  http.Handler
	fallback http.RoundTripper
}

func (c cloudflareTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "api.github.com" {
		return c.fallback.RoundTrip(request)
	}
	recorder := httptest.NewRecorder()
	c.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

type cloudflareFixture struct {
	*fixture
	mirror   *cloudflareMirror
	assets   []app.AssetView
	location repository.Location
}

// buildCloudflareGallery commits a real repository through the application so
// the fake GitHub snapshot contains production-shaped manifest and asset blobs.
func buildCloudflareGallery(t *testing.T) (string, []app.AssetView) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "gallery.git")
	git, err := localgit.Init(t.Context(), path, "main")
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.Open(t.Context(), filepath.Join(root, "builder"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	location := repository.Location{Backend: "localgit", Path: path, Branch: "main"}
	if err = service.Select(t.Context(), location); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Initialize(t.Context(), git); err != nil {
		t.Fatal(err)
	}
	inputs := []app.Input{
		{Reader: bytes.NewReader(cloudflareImageBytes(t, 1)), Name: "夕空.png"},
		{Reader: bytes.NewReader(cloudflareImageBytes(t, 2)), Name: "photo.png"},
	}
	if _, err = service.Add(t.Context(), inputs, app.AddOptions{Namespace: "images"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Commit(t.Context(), git, "publish mirror fixture"); err != nil {
		t.Fatal(err)
	}
	_, views, err := service.ReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("fixture assets = %d, want 2", len(views))
	}
	return path, views
}

func cloudflareImageBytes(t *testing.T, seed int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 9+seed, 7+seed))
	im.Set(seed, seed, color.RGBA{R: uint8(40 * seed), G: uint8(20 * seed), B: uint8(80 + seed), A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newCloudflareFixture(t *testing.T, private bool) *cloudflareFixture {
	t.Helper()
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "r2-test-access")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "r2-test-secret")
	repositoryPath, views := buildCloudflareGallery(t)
	f := newFixture(t)
	location := repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main", Private: private}
	if err := f.service.Select(t.Context(), location); err != nil {
		t.Fatal(err)
	}
	git, err := localgit.Open(t.Context(), repositoryPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = cloudflareTransport{handler: &cloudflareGitHub{t: t, git: git}, fallback: original}
	t.Cleanup(func() { http.DefaultTransport = original })
	client, err := gh.New(location, "sentinel-private-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Sync(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	writeCloudflareConfig(t, f.service, location, cloudflareTestConfig)
	mirror := newCloudflareMirror()
	f.service.NewCloudflare = func(context.Context, r2.Config) (r2.Mirror, error) { return mirror, nil }
	return &cloudflareFixture{fixture: f, mirror: mirror, assets: views, location: location}
}

func writeCloudflareConfig(t *testing.T, service *app.Service, location repository.Location, block string) {
	t.Helper()
	data, err := json.Marshal(location)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err = json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	value["cloudflare"] = json.RawMessage(block)
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(service.Dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func cloudflareObjectPath(asset domain.Asset) string {
	return repository.ObjectPath(asset)
}

func cloudflareExpectedURL(base, objectPath string) string {
	parts := strings.Split(objectPath, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.Join(parts, "/")
}

type cloudflareStatusJSON struct {
	Enabled        bool   `json:"enabled"`
	Configured     bool   `json:"configured"`
	ErrorCode      string `json:"error_code"`
	PendingDeletes int    `json:"pending_deletes"`
	Assets         map[string]struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	} `json:"assets"`
}

func cloudflareState(t *testing.T, f *fixture) (map[string]json.RawMessage, *cloudflareStatusJSON) {
	t.Helper()
	out := f.request("GET", "/api/state", nil, http.StatusOK)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	raw, ok := fields["cloudflare"]
	if !ok {
		return fields, nil
	}
	var status cloudflareStatusJSON
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	return fields, &status
}

func cloudflareRequest(t *testing.T, f *fixture, method, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, testOrigin+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", testOrigin)
		request.Header.Set("X-Touko-CSRF", f.csrf)
	}
	if mutate != nil {
		mutate(request)
	}
	out := httptest.NewRecorder()
	f.server.ServeHTTP(out, request)
	return out
}

func TestCloudflareAPIDisabledCompatibility(t *testing.T) {
	f := newFixture(t)
	f.setup()
	id, _ := f.add()
	f.request("POST", "/api/commit", map[string]string{"message": "publish"}, 200)
	fields, status := cloudflareState(t, f)
	if status != nil {
		t.Fatalf("disabled state exposed cloudflare: %s", fields["cloudflare"])
	}
	out := f.request("POST", "/api/cloudflare/sync", map[string]any{"dry_run": false}, 400)
	if !strings.Contains(out.Body.String(), "delivery_unavailable") {
		t.Fatalf("disabled sync: %s", out.Body.String())
	}
	for _, provider := range []string{"cloudflare", "github", "jsdelivr", ""} {
		path := "/api/assets/" + id + "/url"
		if provider != "" {
			path += "?provider=" + provider
		}
		f.request("GET", path, nil, 400)
	}
	f.request("GET", "/api/cloudflare/unknown", nil, 404)
}

func TestCloudflareAPISyncAndDeliveryURL(t *testing.T) {
	f := newCloudflareFixture(t, false)
	ctx := t.Context()
	state, status := cloudflareState(t, f.fixture)
	if status == nil || !status.Enabled || !status.Configured || status.ErrorCode != "" {
		t.Fatalf("initial cloudflare status: %s", state["cloudflare"])
	}
	if len(status.Assets) != len(f.assets) {
		t.Fatalf("status assets = %d, want %d", len(status.Assets), len(f.assets))
	}
	for _, view := range f.assets {
		entry := status.Assets[view.Asset.ID.String()]
		if entry.State != "pending" {
			t.Fatalf("asset %s initial state = %q, want pending", view.Asset.ID, entry.State)
		}
	}
	asset := f.assets[0].Asset
	objectPath := cloudflareObjectPath(asset)
	// A committed asset without a synced receipt must never produce a URL.
	out := f.request("GET", "/api/assets/"+asset.ID.String()+"/url?provider=cloudflare", nil, 400)
	if !strings.Contains(out.Body.String(), "delivery_unavailable") || strings.Contains(out.Body.String(), "img.example.test") {
		t.Fatalf("unsynced URL: %s", out.Body.String())
	}

	out = f.request("POST", "/api/cloudflare/sync", map[string]any{"dry_run": false}, 200)
	var result app.CloudflareResult
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "synced" || result.Uploaded != len(f.assets) || result.Failed != 0 || result.Pending != 0 {
		t.Fatalf("first sync: %+v", result)
	}
	for _, view := range f.assets {
		path := cloudflareObjectPath(view.Asset)
		f.mirror.mu.Lock()
		object, ok := f.mirror.objects[path]
		f.mirror.mu.Unlock()
		if !ok {
			t.Fatalf("missing mirrored object %s", path)
		}
		if object.sha256 != view.Asset.Object.Hash.String() || object.size != view.Asset.Object.Size || int64(len(object.data)) != view.Asset.Object.Size {
			t.Fatalf("mirrored bytes for %s: sha=%s size=%d data=%d want sha=%s size=%d", path, object.sha256, object.size, len(object.data), view.Asset.Object.Hash, view.Asset.Object.Size)
		}
	}
	state, status = cloudflareState(t, f.fixture)
	if status == nil || status.Assets[asset.ID.String()].State != "synced" {
		t.Fatalf("synced state: %s", state["cloudflare"])
	}

	out = f.request("GET", "/api/assets/"+asset.ID.String()+"/url?provider=cloudflare", nil, 200)
	var delivery struct {
		URL       string `json:"url"`
		Committed bool   `json:"committed"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &delivery); err != nil {
		t.Fatal(err)
	}
	if want := cloudflareExpectedURL("https://img.example.test", objectPath); delivery.URL != want || !delivery.Committed {
		t.Fatalf("cloudflare URL = %+v, want %q committed", delivery, want)
	}

	// A repeated sync through a member URL revalidates HEAD and must not rewrite
	// the object.
	out = f.request("POST", "/w/default/api/cloudflare/sync", map[string]any{"dry_run": false}, 200)
	result = app.CloudflareResult{}
	if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Uploaded != 0 || result.Skipped != len(f.assets) || result.Status != "synced" {
		t.Fatalf("second sync: %+v", result)
	}

	// Losing the object externally invalidates the receipt and the URL.
	f.mirror.remove(objectPath)
	f.request("GET", "/api/assets/"+asset.ID.String()+"/url?provider=cloudflare", nil, 400)
	state, status = cloudflareState(t, f.fixture)
	if status == nil || status.Assets[asset.ID.String()].State != "pending" {
		t.Fatalf("deleted object state: %s", state["cloudflare"])
	}

	// A staged-only asset is not published, even with the same configuration.
	staged := []app.Input{{Reader: bytes.NewReader(cloudflareImageBytes(t, 3)), Name: "staged.png"}}
	views, err := f.service.Add(ctx, staged, app.AddOptions{Namespace: "images"})
	if err != nil {
		t.Fatal(err)
	}
	out = f.request("GET", "/api/assets/"+views[0].Asset.ID.String()+"/url?provider=cloudflare", nil, 400)
	if !strings.Contains(out.Body.String(), "delivery_unavailable") {
		t.Fatalf("staged URL: %s", out.Body.String())
	}
}

func TestCloudflareAPIPartialFailureAndSecurity(t *testing.T) {
	f := newCloudflareFixture(t, false)
	path := cloudflareObjectPath(f.assets[0].Asset)
	f.mirror.failPut(path, &r2.Error{Op: "put", Key: path, Code: "access_denied"})
	out := f.request("POST", "/api/cloudflare/sync", map[string]any{"dry_run": false}, 502)
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Cloudflare *app.CloudflareResult `json:"cloudflare"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "cloudflare_sync_failed" || envelope.Cloudflare == nil ||
		envelope.Cloudflare.Failed+envelope.Cloudflare.Pending == 0 ||
		envelope.Cloudflare.ErrorCode != "access_denied" {
		t.Fatalf("partial failure envelope: %s", out.Body.String())
	}
	if strings.Contains(out.Body.String(), "r2-test-secret") || strings.Contains(out.Body.String(), "r2-test-access") {
		t.Fatal("R2 credentials leaked into the error response")
	}
	// An R2 access failure must not look like a GitHub authentication failure.
	if f.server.login.snapshot().Status != loginIdle {
		t.Fatalf("R2 failure started GitHub login: %q", f.server.login.snapshot().Status)
	}

	for _, test := range []struct {
		name   string
		method string
		body   string
		mutate func(*http.Request)
		status int
	}{
		{name: "missing csrf", method: http.MethodPost, body: `{"dry_run":false}`, mutate: func(r *http.Request) { r.Header.Del("X-Touko-CSRF") }, status: 403},
		{name: "wrong origin", method: http.MethodPost, body: `{"dry_run":false}`, mutate: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, status: 403},
		{name: "get", method: http.MethodGet, status: 405},
		{name: "unknown field", method: http.MethodPost, body: `{"dry_run":false,"endpoint":"https://evil.example"}`, status: 400},
		{name: "wrong type", method: http.MethodPost, body: `{"dry_run":"yes"}`, status: 400},
		{name: "null body", method: http.MethodPost, body: `null`, status: 400},
		{name: "trailing object", method: http.MethodPost, body: `{"dry_run":false}{}`, status: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := cloudflareRequest(t, f.fixture, test.method, "/api/cloudflare/sync", test.body, test.mutate)
			if out.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", out.Code, test.status, out.Body.String())
			}
		})
	}
}

func TestCloudflareAPIStateInvalidConfigurationIsReported(t *testing.T) {
	f := newCloudflareFixture(t, false)
	bad := `{"enabled":true,"endpoint":"http://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.test"}`
	writeCloudflareConfig(t, f.service, f.location, bad)
	state, status := cloudflareState(t, f.fixture)
	if status == nil || !status.Enabled || status.Configured || status.ErrorCode != "configuration_invalid" {
		t.Fatalf("invalid configuration status: %s", state["cloudflare"])
	}
	out := f.request("POST", "/api/cloudflare/sync", map[string]any{"dry_run": false}, 400)
	if !strings.Contains(out.Body.String(), "delivery_unavailable") {
		t.Fatalf("invalid configuration sync: %s", out.Body.String())
	}
	if _, err := os.Stat(filepath.Join(f.service.Dir, "cloudflare")); !os.IsNotExist(err) {
		t.Fatalf("invalid configuration touched the ledger: %v", err)
	}
}

func TestCloudflareAPIPrivateAndLocalProviders(t *testing.T) {
	t.Run("private GitHub with explicit Cloudflare", func(t *testing.T) {
		f := newCloudflareFixture(t, true)
		out := f.request("POST", "/api/cloudflare/sync", map[string]any{"dry_run": false}, 200)
		var result app.CloudflareResult
		if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "synced" {
			t.Fatalf("private sync: %+v", result)
		}
		asset := f.assets[0].Asset
		f.request("GET", "/api/assets/"+asset.ID.String()+"/url?provider=cloudflare", nil, 200)
		// The public providers keep their original private-repository refusal.
		for _, provider := range []string{"github", "jsdelivr"} {
			f.request("GET", "/api/assets/"+asset.ID.String()+"/url?provider="+provider, nil, 400)
		}
	})

	t.Run("local repository stays blocked", func(t *testing.T) {
		f := newFixture(t)
		f.setup()
		id, _ := f.add()
		f.request("POST", "/api/commit", map[string]string{"message": "publish"}, 200)
		location, err := f.service.Location(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		writeCloudflareConfig(t, f.service, location, cloudflareTestConfig)
		t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "r2-test-access")
		t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "r2-test-secret")
		out := f.request("POST", "/api/cloudflare/sync", map[string]any{"dry_run": false}, 400)
		if !strings.Contains(out.Body.String(), "delivery_unavailable") {
			t.Fatalf("local sync: %s", out.Body.String())
		}
		f.request("GET", "/api/assets/"+id+"/url?provider=cloudflare", nil, 400)
	})
}

func TestCloudflareAPIResetKeepsOptionalWarning(t *testing.T) {
	f := newCloudflareFixture(t, false)
	bad := `{"enabled":true,"endpoint":"http://acct.r2.cloudflarestorage.com","bucket":"touko-images","public_base_url":"https://img.example.test"}`
	writeCloudflareConfig(t, f.service, f.location, bad)
	out := f.request("POST", "/api/reset", map[string]bool{"confirm": true}, 200)
	var value map[string]json.RawMessage
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if string(value["ok"]) != "true" {
		t.Fatalf("reset result: %s", out.Body.String())
	}
	var status cloudflareStatusJSON
	if err := json.Unmarshal(value["cloudflare"], &status); err != nil {
		t.Fatalf("reset cloudflare warning: %s", out.Body.String())
	}
	if !status.Enabled || status.Configured || status.ErrorCode != "configuration_invalid" {
		t.Fatalf("reset cloudflare warning = %+v", status)
	}

	disabled := newFixture(t)
	disabled.setup()
	out = disabled.request("POST", "/api/reset", map[string]bool{"confirm": true}, 200)
	if strings.Contains(out.Body.String(), "cloudflare") {
		t.Fatalf("disabled reset changed its response: %s", out.Body.String())
	}
}

func TestCloudflareAPILibraryMembersStayIsolated(t *testing.T) {
	f := newFixture(t)
	f.setup()
	f.add()
	f.request("POST", "/api/commit", map[string]string{"message": "publish"}, 200)
	location, err := f.service.Location(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	writeCloudflareConfig(t, f.service, location, cloudflareTestConfig)
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "r2-test-access")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "r2-test-secret")
	ctx := t.Context()
	secondPath := filepath.Join(f.root, "second.git")
	second, err := localgit.Init(ctx, secondPath, "main")
	if err != nil {
		t.Fatal(err)
	}
	server, err := f.server.spaces.selectLocation(ctx, localGitLocation(f.root, "second.git"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.service.Initialize(ctx, second); err != nil {
		t.Fatal(err)
	}
	config, err := f.server.spaces.loadViews()
	if err != nil {
		t.Fatal(err)
	}
	library := libraryView{ID: "0123456789abcdef0123456789abcdef", Name: "镜像图库", Workspaces: []string{"default", server.workspaceID}}
	config.Libraries = append(config.Libraries, library)
	config.Active = activeView{Kind: "library", ID: library.ID}
	if err := f.server.spaces.saveViews(config); err != nil {
		t.Fatal(err)
	}
	state := libraryCall(t, f, "GET", libraryURL(library.ID)+"api/state", nil, "")
	if state.Code != http.StatusOK {
		t.Fatalf("library state: %d %s", state.Code, state.Body.String())
	}
	var value struct {
		Members []map[string]json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Members) != 2 {
		t.Fatalf("members = %d", len(value.Members))
	}
	if _, ok := value.Members[0]["cloudflare"]; !ok {
		t.Fatal("enabled member lost its cloudflare status")
	}
	if _, ok := value.Members[1]["cloudflare"]; ok {
		t.Fatal("local member exposed a cloudflare status")
	}
	var status cloudflareStatusJSON
	if err := json.Unmarshal(value.Members[0]["cloudflare"], &status); err != nil {
		t.Fatal(err)
	}
	// A local repository cannot mirror, but the enabled status is still exposed
	// for this member and never leaks into its sibling.
	if !status.Enabled {
		t.Fatalf("enabled member status: %+v", status)
	}
}

func TestCloudflareAPIFailRedactsR2Secrets(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "r2-access-sentinel")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "r2-secret-sentinel")
	out := httptest.NewRecorder()
	f.server.fail(out, http.StatusBadRequest, "usage", "access=r2-access-sentinel secret=r2-secret-sentinel")
	body := out.Body.String()
	if strings.Contains(body, "r2-access-sentinel") || strings.Contains(body, "r2-secret-sentinel") {
		t.Fatalf("R2 secret exposed: %s", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("secrets not replaced: %s", body)
	}
}
