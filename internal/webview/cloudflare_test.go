package webview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Eurekaimer/TOUKO/internal/credential"
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
	f.service.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) { return mirror, nil }
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
	logData, err := os.ReadFile(filepath.Join(f.service.Dir, "operation-errors.jsonl"))
	if err != nil || !bytes.Contains(logData, []byte("access_denied")) || bytes.Contains(logData, []byte("r2-test-secret")) {
		t.Fatalf("mirror failure diagnostic missing or unsafe: %q %v", logData, err)
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

func cloudflareSettingsJSON(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.NewDecoder(body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCloudflareConfigEndpointRoundTrip(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	f := newFixture(t)
	f.setup()

	out := f.request("GET", "/api/cloudflare/config", nil, 200)
	value := cloudflareSettingsJSON(t, out.Body)
	for _, field := range []string{"enabled", "endpoint", "bucket", "public_base_url", "credentials_configured"} {
		if _, ok := value[field]; !ok {
			t.Fatalf("GET response lacks %q: %v", field, value)
		}
	}
	if value["enabled"] != false || value["credentials_configured"] != false {
		t.Fatalf("initial settings = %v", value)
	}
	for _, field := range []string{"access_key_id", "secret_access_key"} {
		if _, ok := value[field]; ok {
			t.Fatalf("GET response exposes %q", field)
		}
	}

	out = f.request("POST", "/api/cloudflare/config", map[string]any{
		"enabled":           true,
		"endpoint":          "https://acct.r2.cloudflarestorage.com",
		"bucket":            "touko-images",
		"public_base_url":   "https://img.example.test",
		"access_key_id":     "access-SENTINEL",
		"secret_access_key": "secret-SENTINEL",
	}, 200)
	if strings.Contains(out.Body.String(), "SENTINEL") {
		t.Fatalf("POST response leaked credentials: %s", out.Body.String())
	}
	value = cloudflareSettingsJSON(t, out.Body)
	if value["enabled"] != true || value["credentials_configured"] != true || value["bucket"] != "touko-images" {
		t.Fatalf("saved settings = %v", value)
	}

	config, err := os.ReadFile(filepath.Join(f.service.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte("SENTINEL")) {
		t.Fatal("config.json contains credentials")
	}
	credentialPath := filepath.Join(f.service.Dir, "cloudflare", "credentials.json")
	info, err := os.Stat(credentialPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file: %v %v", info, err)
	}
	pair, found, err := credential.LoadR2(t.Context(), f.service.Dir)
	if err != nil || !found || pair.AccessKeyID != "access-SENTINEL" || pair.SecretAccessKey != "secret-SENTINEL" {
		t.Fatalf("stored credentials missing or mismatched: found=%t, err=%v", found, err)
	}
	before, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}

	// Blank credential fields keep the saved pair and a disabled toggle keeps
	// the target fields readable.
	f.request("POST", "/api/cloudflare/config", map[string]any{
		"enabled":           false,
		"endpoint":          "https://acct.r2.cloudflarestorage.com",
		"bucket":            "touko-images",
		"public_base_url":   "https://img.example.test",
		"access_key_id":     "",
		"secret_access_key": "",
	}, 200)
	after, err := os.ReadFile(credentialPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("blank credentials rewrote the stored pair: %v", err)
	}
	value = cloudflareSettingsJSON(t, f.request("GET", "/api/cloudflare/config", nil, 200).Body)
	if value["enabled"] != false || value["endpoint"] != "https://acct.r2.cloudflarestorage.com" || value["credentials_configured"] != true {
		t.Fatalf("disabled settings were not retained: %v", value)
	}

	// A partial pair and an unsafe public base are rejected without touching
	// the saved settings or credentials.
	configBefore, err := os.ReadFile(filepath.Join(f.service.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{
		{"enabled": false, "access_key_id": "only-access-SENTINEL", "secret_access_key": ""},
		{"enabled": false, "access_key_id": "", "secret_access_key": "only-secret-SENTINEL"},
		{"enabled": true, "endpoint": "https://acct.r2.cloudflarestorage.com", "bucket": "touko-images", "public_base_url": "https://img.example.test/%2e%2e/secret"},
	} {
		out := f.request("POST", "/api/cloudflare/config", body, 400)
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &failure); err != nil {
			t.Fatal(err)
		}
		if failure.Error.Code != "configuration_invalid" || failure.Error.Message == "" {
			t.Fatalf("settings rejection = %s", out.Body.String())
		}
		if strings.Contains(out.Body.String(), "SENTINEL") {
			t.Fatalf("settings rejection leaked a credential: %s", out.Body.String())
		}
	}
	configAfter, err := os.ReadFile(filepath.Join(f.service.Dir, "config.json"))
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatal("rejected settings rewrote config.json")
	}
	after, err = os.ReadFile(credentialPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected settings rewrote the stored pair")
	}

	// The endpoint requires the exact same CSRF and Origin protections as the
	// other member mutations.
	req := httptest.NewRequest("POST", testOrigin+"/api/cloudflare/config", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", recorder.Code)
	}
}

func TestCloudflareConfigEndpointIsMemberScoped(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	f := newFixture(t)
	f.setup()
	location := repository.Location{Backend: "localgit", Path: filepath.Join(f.root, "second.git"), Branch: "main"}
	target, err := f.server.spaces.selectLocation(t.Context(), location)
	if err != nil {
		t.Fatal(err)
	}
	memberPath := workspaceURL(target.workspaceID) + "api/cloudflare/config"
	post := func(body any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", testOrigin+memberPath, bytes.NewReader(data))
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("X-Touko-CSRF", target.csrf)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		f.server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("member POST status %d: %s", recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	post(map[string]any{
		"enabled":           true,
		"endpoint":          "https://member.r2.cloudflarestorage.com",
		"bucket":            "member-bucket",
		"public_base_url":   "https://member.example.test",
		"access_key_id":     "member-access-SENTINEL",
		"secret_access_key": "member-secret-SENTINEL",
	})

	req := httptest.NewRequest("GET", testOrigin+memberPath, nil)
	recorder := httptest.NewRecorder()
	f.server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("member GET status %d: %s", recorder.Code, recorder.Body.String())
	}
	var member map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &member); err != nil {
		t.Fatal(err)
	}
	if member["bucket"] != "member-bucket" || member["credentials_configured"] != true {
		t.Fatalf("member settings = %v", member)
	}

	root := cloudflareSettingsJSON(t, f.request("GET", "/api/cloudflare/config", nil, 200).Body)
	if root["enabled"] != false || root["credentials_configured"] != false || root["bucket"] != "" {
		t.Fatalf("member settings leaked into the root workspace: %v", root)
	}
	if _, err := os.Stat(filepath.Join(f.service.Dir, "cloudflare")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root workspace gained member credential state: %v", err)
	}
	memberPair, found, err := credential.LoadR2(t.Context(), target.service.Dir)
	if err != nil || !found || memberPair.AccessKeyID != "member-access-SENTINEL" {
		t.Fatalf("member credentials missing or mismatched: found=%t, err=%v", found, err)
	}
	if strings.Contains(recorder.Body.String(), "SENTINEL") {
		t.Fatal("member response leaked credentials")
	}
}

func TestCloudflareFailRedactsSavedWorkspaceCredentials(t *testing.T) {
	f := newFixture(t)
	f.setup()
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	if _, err := f.service.UpdateCloudflareSettings(t.Context(), app.CloudflareConfigInput{
		AccessKeyID: "saved-access-SENTINEL", SecretAccessKey: "saved-secret-SENTINEL",
	}); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	f.server.fail(out, http.StatusBadRequest, "usage", "access=saved-access-SENTINEL secret=saved-secret-SENTINEL")
	body := out.Body.String()
	if strings.Contains(body, "saved-access-SENTINEL") || strings.Contains(body, "saved-secret-SENTINEL") {
		t.Fatalf("saved credential exposed: %s", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("saved credentials not replaced: %s", body)
	}
}

type cloudflareCredentialsJSON struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Source          string `json:"source"`
}

func decodeCloudflareCredentials(t *testing.T, out *httptest.ResponseRecorder) cloudflareCredentialsJSON {
	t.Helper()
	if out.Code != http.StatusOK {
		t.Fatalf("credentials status %d: %s", out.Code, out.Body.String())
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("credentials response is cacheable: %q", out.Header().Get("Cache-Control"))
	}
	var value cloudflareCredentialsJSON
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("credentials response fields = %v", fields)
	}
	return value
}

func TestCloudflareCredentialsEndpointIsPostOnlyAndProtected(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	f := newFixture(t)
	f.setup()
	path := "/api/cloudflare/credentials"

	value := decodeCloudflareCredentials(t, cloudflareRequest(t, f, http.MethodPost, path, `{}`, nil))
	if value.Source != "none" || value.AccessKeyID != "" || value.SecretAccessKey != "" {
		t.Fatalf("unconfigured credentials = %+v", value)
	}

	out := cloudflareRequest(t, f, http.MethodGet, path, "", nil)
	if out.Code != http.StatusMethodNotAllowed || out.Header().Get("Allow") != "POST" {
		t.Fatalf("GET credentials: status=%d allow=%q", out.Code, out.Header().Get("Allow"))
	}

	for _, body := range []string{`{"access_key_id":"unexpected"}`, `null`, `[]`, ``, `{} {}`} {
		out := cloudflareRequest(t, f, http.MethodPost, path, body, nil)
		if out.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d: %s", body, out.Code, out.Body.String())
		}
	}

	for name, mutate := range map[string]func(*http.Request){
		"missing csrf": func(req *http.Request) { req.Header.Del("X-Touko-CSRF") },
		"wrong csrf":   func(req *http.Request) { req.Header.Set("X-Touko-CSRF", "wrong-token") },
		"missing origin": func(req *http.Request) {
			req.Header.Del("Origin")
		},
		"wrong origin": func(req *http.Request) { req.Header.Set("Origin", "https://evil.example") },
	} {
		out := cloudflareRequest(t, f, http.MethodPost, path, `{}`, mutate)
		if out.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d: %s", name, out.Code, out.Body.String())
		}
	}
}

func TestCloudflareCredentialsAreWorkspaceScopedAndNeverMixed(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	f := newFixture(t)
	f.setup()
	location := repository.Location{Backend: "localgit", Path: filepath.Join(f.root, "credentials-member.git"), Branch: "main"}
	target, err := f.server.spaces.selectLocation(t.Context(), location)
	if err != nil {
		t.Fatal(err)
	}
	memberPath := workspaceURL(target.workspaceID) + "api/cloudflare/credentials"
	memberGet := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, testOrigin+path, nil)
		recorder := httptest.NewRecorder()
		f.server.ServeHTTP(recorder, req)
		return recorder
	}
	memberCredentials := func() cloudflareCredentialsJSON {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, testOrigin+memberPath, strings.NewReader(`{}`))
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("X-Touko-CSRF", target.csrf)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		f.server.ServeHTTP(recorder, req)
		return decodeCloudflareCredentials(t, recorder)
	}

	memberPair := credential.R2Pair{AccessKeyID: "member-access-SENTINEL", SecretAccessKey: "member-secret-SENTINEL"}
	if err := credential.SaveR2(t.Context(), target.service.Dir, memberPair); err != nil {
		t.Fatal(err)
	}
	value := memberCredentials()
	if value.Source != "saved" || value.AccessKeyID != memberPair.AccessKeyID || value.SecretAccessKey != memberPair.SecretAccessKey {
		t.Fatalf("member credentials = %+v", value)
	}
	root := decodeCloudflareCredentials(t, f.request("POST", "/api/cloudflare/credentials", map[string]any{}, 200))
	if root.Source != "none" || root.AccessKeyID != "" || root.SecretAccessKey != "" {
		t.Fatalf("member credentials leaked into the root workspace: %+v", root)
	}

	rootPair := credential.R2Pair{AccessKeyID: "root-access-SENTINEL", SecretAccessKey: "root-secret-SENTINEL"}
	if err := credential.SaveR2(t.Context(), f.service.Dir, rootPair); err != nil {
		t.Fatal(err)
	}
	value = memberCredentials()
	if value.Source != "saved" || value.AccessKeyID != memberPair.AccessKeyID {
		t.Fatalf("root credentials replaced the member pair: %+v", value)
	}
	root = decodeCloudflareCredentials(t, f.request("POST", "/api/cloudflare/credentials", map[string]any{}, 200))
	if root.Source != "saved" || root.AccessKeyID != rootPair.AccessKeyID || root.SecretAccessKey != rootPair.SecretAccessKey {
		t.Fatalf("root credentials = %+v", root)
	}

	// A complete environment pair is used only when the workspace has no saved
	// file, and a saved pair always wins instead of mixing both sources.
	envPair := credential.R2Pair{AccessKeyID: "env-access-SENTINEL", SecretAccessKey: "env-secret-SENTINEL"}
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", envPair.AccessKeyID)
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", envPair.SecretAccessKey)
	value = memberCredentials()
	if value.Source != "saved" || value.AccessKeyID != memberPair.AccessKeyID {
		t.Fatalf("environment overrode a saved member pair: %+v", value)
	}
	if err := os.Remove(filepath.Join(target.service.Dir, "cloudflare", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	value = memberCredentials()
	if value.Source != "environment" || value.AccessKeyID != envPair.AccessKeyID || value.SecretAccessKey != envPair.SecretAccessKey {
		t.Fatalf("environment fallback = %+v", value)
	}
	root = decodeCloudflareCredentials(t, f.request("POST", "/api/cloudflare/credentials", map[string]any{}, 200))
	if root.Source != "saved" || root.AccessKeyID != rootPair.AccessKeyID {
		t.Fatalf("environment overrode the saved root pair: %+v", root)
	}
	if err := os.Remove(filepath.Join(f.service.Dir, "cloudflare", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	// A partial environment pair is not a usable source and is never completed
	// with a saved value.
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	root = decodeCloudflareCredentials(t, f.request("POST", "/api/cloudflare/credentials", map[string]any{}, 200))
	if root.Source != "none" || root.AccessKeyID != "" || root.SecretAccessKey != "" {
		t.Fatalf("partial environment pair was mixed: %+v", root)
	}

	for _, path := range []string{"/api/cloudflare/config", "/api/state"} {
		out := f.request("GET", path, nil, 200)
		for _, secret := range []string{memberPair.AccessKeyID, memberPair.SecretAccessKey, rootPair.AccessKeyID, rootPair.SecretAccessKey, envPair.AccessKeyID, envPair.SecretAccessKey} {
			if strings.Contains(out.Body.String(), secret) {
				t.Fatalf("GET %s exposed %q: %s", path, secret, out.Body.String())
			}
		}
	}
	for _, path := range []string{"api/cloudflare/config", "api/state"} {
		out := memberGet(workspaceURL(target.workspaceID) + path)
		if out.Code != http.StatusOK {
			t.Fatalf("member GET %s status = %d: %s", path, out.Code, out.Body.String())
		}
		for _, secret := range []string{memberPair.AccessKeyID, memberPair.SecretAccessKey, rootPair.AccessKeyID, rootPair.SecretAccessKey, envPair.AccessKeyID, envPair.SecretAccessKey} {
			if strings.Contains(out.Body.String(), secret) {
				t.Fatalf("member GET %s exposed %q: %s", path, secret, out.Body.String())
			}
		}
	}
}

func TestCloudflareCredentialsCorruptSavedFileIsControlled(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	f := newFixture(t)
	f.setup()
	dir := filepath.Join(f.service.Dir, "cloudflare")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	corrupt := `{"version":1,"access_key_id":"corrupt-access-SENTINEL","secret_access_key":""}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	out := f.request("POST", "/api/cloudflare/credentials", map[string]any{}, 500)
	if out.Code != http.StatusInternalServerError || strings.Contains(out.Body.String(), "corrupt-access-SENTINEL") {
		t.Fatalf("corrupt credentials response = %d: %s", out.Code, out.Body.String())
	}
	config := f.request("GET", "/api/cloudflare/config", nil, 200)
	if strings.Contains(config.Body.String(), "corrupt-access-SENTINEL") {
		t.Fatalf("config exposed corrupt credentials: %s", config.Body.String())
	}
	var settings map[string]any
	if err := json.Unmarshal(config.Body.Bytes(), &settings); err != nil || settings["credentials_configured"] != false {
		t.Fatalf("config readiness = %v, err=%v", settings, err)
	}
}
