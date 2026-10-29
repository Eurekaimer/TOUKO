package github_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	gh "github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const sentinel = "secret-sentinel-never-print"

type gitAPI struct {
	t                    *testing.T
	git                  *localgit.Repository
	server               *httptest.Server
	mu                   sync.Mutex
	failMethod, failPath string
	status               int
	truncate, loseRef    bool
	rejectionHead        domain.RepositoryHead
}

func newAPI(t *testing.T) (*gitAPI, *gh.Repository) {
	t.Helper()
	ctx := context.Background()
	g, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "gallery.git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.CreateBlob(ctx, strings.NewReader("keep README\n"), 12)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := g.CreateTree(ctx, "", []repository.TreeChange{{Path: "README.md", Blob: &b}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := g.CreateCommit(ctx, tree, "", "README")
	if err != nil {
		t.Fatal(err)
	}
	if err = g.UpdateRef(ctx, "", h); err != nil {
		t.Fatal(err)
	}
	api := &gitAPI{t: t, git: g}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	r, err := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, api.server.URL, api.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return api, r
}
func (a *gitAPI) fault(method, path string, status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failMethod = method
	a.failPath = path
	a.status = status
}
func (a *gitAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	fail := r.Method == a.failMethod && strings.Contains(r.URL.Path, a.failPath)
	status := a.status
	truncated := a.truncate
	lost := a.loseRef
	rejectionHead := a.rejectionHead
	a.mu.Unlock()
	if r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
		a.t.Error("missing API version")
	}
	if !(r.Method == "GET" && strings.Contains(r.URL.Path, "/git/blobs/")) && r.Header.Get("Accept") != "application/vnd.github+json" {
		a.t.Error("missing JSON accept")
	}
	if auth := r.Header.Get("Authorization"); auth != "" && auth != "Bearer "+sentinel {
		a.t.Error("incorrect authorization")
	}
	if fail && status == 0 {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return
	}
	if fail {
		if rejectionHead != "" && r.Method == "PATCH" {
			old, e := a.git.Head(r.Context())
			if e != nil {
				a.t.Error(e)
			} else if e = a.git.UpdateRef(r.Context(), old, rejectionHead); e != nil {
				a.t.Error(e)
			}
		}
		if status == 403 {
			w.Header().Set("X-RateLimit-Remaining", "1")
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":%q}`, "fault "+sentinel)
		return
	}
	respond := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			return
		}
	}
	failErr := func(err error) bool {
		if err != nil {
			w.WriteHeader(500)
			respond(map[string]string{"message": err.Error()})
			return true
		}
		return false
	}
	ctx := r.Context()
	p := strings.TrimPrefix(r.URL.Path, "/repos/owner/gallery")
	if r.URL.Path == "/user" {
		respond(map[string]string{"login": "octocat"})
		return
	}
	if p == "" && r.Method == "GET" {
		respond(map[string]any{"default_branch": "main", "private": false})
		return
	}
	if p == "/git/ref/heads/main" && r.Method == "GET" {
		h, e := a.git.Head(ctx)
		if failErr(e) {
			return
		}
		respond(map[string]any{"object": map[string]any{"sha": h, "type": "commit"}})
		return
	}
	if strings.HasPrefix(p, "/compare/") {
		if r.URL.Query().Get("per_page") != "1" {
			a.t.Error("compare must use one result")
		}
		pair := strings.Split(strings.TrimPrefix(p, "/compare/"), "...")
		if len(pair) != 2 {
			w.WriteHeader(400)
			return
		}
		yes, e := a.git.IsAncestor(ctx, domain.RepositoryHead(pair[0]), domain.RepositoryHead(pair[1]))
		if failErr(e) {
			return
		}
		status := "diverged"
		if yes {
			status = "ahead"
			if pair[0] == pair[1] {
				status = "identical"
			}
		} else if reverse, e := a.git.IsAncestor(ctx, domain.RepositoryHead(pair[1]), domain.RepositoryHead(pair[0])); e == nil && reverse {
			status = "behind"
		}
		respond(map[string]any{"status": status, "commits": []any{}})
		return
	}
	if strings.HasPrefix(p, "/git/commits/") && r.Method == "GET" {
		c, e := a.git.Commit(ctx, domain.RepositoryHead(strings.TrimPrefix(p, "/git/commits/")))
		if failErr(e) {
			return
		}
		parents := []any{}
		for _, v := range c.Parents {
			parents = append(parents, map[string]any{"sha": v})
		}
		respond(map[string]any{"tree": map[string]any{"sha": c.Tree}, "parents": parents})
		return
	}
	if strings.HasPrefix(p, "/git/trees/") && r.Method == "GET" {
		if r.URL.Query().Get("recursive") != "1" {
			a.t.Error("tree must be recursive")
		}
		entries, e := a.git.Tree(ctx, repository.TreeID(strings.TrimPrefix(p, "/git/trees/")))
		if failErr(e) {
			return
		}
		tree := []any{}
		for _, v := range entries {
			tree = append(tree, map[string]any{"path": v.Path, "mode": v.Mode, "type": v.Type, "sha": v.SHA, "size": v.Size})
		}
		respond(map[string]any{"tree": tree, "truncated": truncated})
		return
	}
	if strings.HasPrefix(p, "/git/blobs/") && r.Method == "GET" {
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			a.t.Error("blob must use raw accept")
		}
		body, e := a.git.OpenBlob(ctx, repository.BlobID(strings.TrimPrefix(p, "/git/blobs/")))
		if failErr(e) {
			return
		}
		defer body.Close()
		io.Copy(w, body)
		return
	}
	if p == "/git/blobs" && r.Method == "POST" {
		var v struct{ Content, Encoding string }
		data, e := io.ReadAll(r.Body)
		if failErr(e) {
			return
		}
		if int64(len(data)) != r.ContentLength {
			a.t.Errorf("content length %d != %d", len(data), r.ContentLength)
		}
		if failErr(json.Unmarshal(data, &v)) {
			return
		}
		if v.Encoding != "base64" {
			a.t.Error("wrong blob encoding")
		}
		decoded, e := base64.StdEncoding.DecodeString(v.Content)
		if failErr(e) {
			return
		}
		id, e := a.git.CreateBlob(ctx, bytes.NewReader(decoded), int64(len(decoded)))
		if failErr(e) {
			return
		}
		respond(map[string]any{"sha": id})
		return
	}
	if p == "/git/trees" && r.Method == "POST" {
		var v struct {
			Base string `json:"base_tree"`
			Tree []struct {
				Path, Mode, Type string
				SHA              json.RawMessage `json:"sha"`
			} `json:"tree"`
		}
		if failErr(json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if v.Base == "" {
			a.t.Error("base tree omitted")
		}
		changes := []repository.TreeChange{}
		for _, x := range v.Tree {
			if len(x.SHA) == 0 {
				a.t.Error("deletion sha omitted")
				w.WriteHeader(400)
				return
			}
			var id *repository.BlobID
			if failErr(json.Unmarshal(x.SHA, &id)) {
				return
			}
			if x.Mode != "100644" || x.Type != "blob" {
				a.t.Error("invalid tree mode/type")
			}
			changes = append(changes, repository.TreeChange{Path: x.Path, Blob: id})
		}
		id, e := a.git.CreateTree(ctx, repository.TreeID(v.Base), changes)
		if failErr(e) {
			return
		}
		respond(map[string]any{"sha": id})
		return
	}
	if p == "/git/commits" && r.Method == "POST" {
		var v struct {
			Tree    repository.TreeID
			Parents []domain.RepositoryHead
			Message string
		}
		if failErr(json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if len(v.Parents) != 1 || v.Parents[0] == "" {
			a.t.Error("expected one parent")
			w.WriteHeader(400)
			return
		}
		id, e := a.git.CreateCommit(ctx, v.Tree, v.Parents[0], v.Message)
		if failErr(e) {
			return
		}
		respond(map[string]any{"sha": id})
		return
	}
	if p == "/git/refs/heads/main" && r.Method == "PATCH" {
		var v struct {
			SHA   domain.RepositoryHead
			Force *bool
		}
		if failErr(json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if v.Force == nil || *v.Force {
			a.t.Error("force must be false")
		}
		h, e := a.git.Head(ctx)
		if failErr(e) {
			return
		}
		ok, e := a.git.IsAncestor(ctx, h, v.SHA)
		if failErr(e) {
			return
		}
		if !ok {
			w.WriteHeader(422)
			respond(map[string]string{"message": "not fast forward"})
			return
		}
		if failErr(a.git.UpdateRef(ctx, h, v.SHA)) {
			return
		}
		if lost {
			hj := w.(http.Hijacker)
			conn, _, e := hj.Hijack()
			if e == nil {
				conn.Close()
			}
			return
		}
		respond(map[string]any{"object": map[string]any{"sha": v.SHA}})
		return
	}
	w.WriteHeader(404)
	respond(map[string]string{"message": "not found"})
}
func service(t *testing.T) *app.Service {
	t.Helper()
	s, e := app.Open(context.Background(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func stageImage(t *testing.T, s *app.Service) domain.Asset {
	t.Helper()
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 3, 2))
	im.Set(1, 1, color.RGBA{R: 255, A: 255})
	if e := png.Encode(&b, im); e != nil {
		t.Fatal(e)
	}
	v, e := s.Add(context.Background(), []app.Input{{Reader: bytes.NewReader(b.Bytes()), Name: "截图.png"}}, app.AddOptions{Namespace: "blog", Tags: []string{"demo"}})
	if e != nil {
		t.Fatal(e)
	}
	return v[0].Asset
}

func TestAdapterPublishesAndDeletesRealGit(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	s := service(t)
	if _, e := s.Initialize(ctx, r); e != nil {
		t.Fatal(e)
	}
	asset := stageImage(t, s)
	if _, e := s.Commit(ctx, r, "add"); e != nil {
		t.Fatal(e)
	}
	fresh := service(t)
	if _, e := fresh.Sync(ctx, r); e != nil {
		t.Fatal(e)
	}
	st, e := fresh.Store.Load(ctx)
	if e != nil || !reflect.DeepEqual(st.Assets[asset.ID], asset) {
		t.Fatal("metadata not restored", e)
	}
	reader, e := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, "", a.server.URL, a.server.Client())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = repository.ReadSnapshot(ctx, reader, st.Head); e != nil {
		t.Fatal("public read", e)
	}
	if _, e = reader.CreateBlob(ctx, strings.NewReader("x"), 1); !errors.Is(e, domain.ErrUnauthorized) {
		t.Fatal(e)
	}
	if e = s.Remove(ctx, []string{asset.ID.String()}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Commit(ctx, r, "delete"); e != nil {
		t.Fatal(e)
	}
	if _, e = fresh.Sync(ctx, r); e != nil {
		t.Fatal(e)
	}
	after, _ := fresh.Store.Load(ctx)
	if len(after.Assets) != 0 {
		t.Fatal("deleted asset resurrected")
	}
	c, e := a.git.Commit(ctx, after.Head)
	if e != nil {
		t.Fatal(e)
	}
	entries, e := a.git.Tree(ctx, c.Tree)
	if e != nil {
		t.Fatal(e)
	}
	readme := false
	for _, v := range entries {
		if v.Path == "README.md" {
			readme = true
		}
		if strings.HasPrefix(v.Path, "objects/") || strings.HasPrefix(v.Path, ".touko/assets/") {
			t.Fatalf("deleted path remains %s", v.Path)
		}
	}
	if !readme {
		t.Fatal("README removed")
	}
	loc, e := r.Discover(ctx, "owner", "gallery", "")
	if e != nil || loc.Branch != "main" || loc.Private {
		t.Fatal(loc, e)
	}
	if user, e := r.User(ctx); e != nil || user != "octocat" {
		t.Fatal(user, e)
	}
}

func TestAPIFailuresPreserveStagingAndHead(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"POST", "/git/blobs", 401}, {"POST", "/git/blobs", 403}, {"POST", "/git/blobs", 404}, {"POST", "/git/blobs", 429}, {"POST", "/git/blobs", 503}, {"POST", "/git/trees", 500}, {"POST", "/git/commits", 500}, {"PATCH", "/git/refs/", 422}} {
		t.Run(fmt.Sprintf("%s-%d", tc.path, tc.status), func(t *testing.T) {
			a, r := newAPI(t)
			ctx := context.Background()
			s := service(t)
			if _, e := s.Initialize(ctx, r); e != nil {
				t.Fatal(e)
			}
			asset := stageImage(t, s)
			before, _ := s.Store.Load(ctx)
			a.fault(tc.method, tc.path, tc.status)
			_, e := s.Commit(ctx, r, "fault")
			if e == nil {
				t.Fatal("fault succeeded")
			}
			var api *gh.APIError
			if !errors.As(e, &api) || api.Status != tc.status {
				t.Fatalf("classification %T %v", e, e)
			}
			if strings.Contains(e.Error(), sentinel) || strings.Contains(api.Message, sentinel) || strings.Contains(api.Endpoint, sentinel) {
				t.Fatal("credential leaked")
			}
			if (tc.status == 401 || tc.status == 403) != errors.Is(e, domain.ErrUnauthorized) {
				t.Fatal("authorization classification", e)
			}
			h, _ := a.git.Head(ctx)
			after, _ := s.Store.Load(ctx)
			if h != before.Head || after.Head != before.Head || after.Changes[asset.ID].Asset == nil {
				t.Fatal("fault changed published/cache/stage state")
			}
			a.fault("", "", 0)
			if _, e = s.Commit(ctx, r, "retry"); e != nil {
				t.Fatal(e)
			}
			fresh := service(t)
			if _, e = fresh.Sync(ctx, r); e != nil {
				t.Fatal(e)
			}
			st, _ := fresh.Store.Load(ctx)
			if !reflect.DeepEqual(st.Assets[asset.ID], asset) {
				t.Fatal("retry metadata differs")
			}
		})
	}
}

func TestTruncatedTreeKeepsExistingCache(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	s := service(t)
	if _, e := s.Initialize(ctx, r); e != nil {
		t.Fatal(e)
	}
	stageImage(t, s)
	before, e := s.Store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	a.mu.Lock()
	a.truncate = true
	a.mu.Unlock()
	if _, e = s.Sync(ctx, r); e == nil {
		t.Fatal("accepted truncated tree")
	}
	after, e := s.Store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("truncated response replaced cache or stage")
	}
}

func TestLostRefResponseRecoversPublishedCandidate(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	s := service(t)
	if _, e := s.Initialize(ctx, r); e != nil {
		t.Fatal(e)
	}
	asset := stageImage(t, s)
	a.mu.Lock()
	a.loseRef = true
	a.mu.Unlock()
	_, _ = s.Commit(ctx, r, "lost response")
	published, e := a.git.Head(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Sync(ctx, r); e != nil {
		t.Fatal(e)
	}
	st, e := s.Store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	h, e := a.git.Head(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if h != published || st.Head != published || st.Publication != nil || len(st.Changes) != 0 || !reflect.DeepEqual(st.Assets[asset.ID], asset) {
		t.Fatal("publication not recovered exactly once")
	}
}

func TestRateLimitAndTimeoutRemainAPIErrors(t *testing.T) {
	for _, status := range []int{403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"message":%q}`, "rate limit "+sentinel)
			}))
			defer srv.Close()
			r, e := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, srv.URL, srv.Client())
			if e != nil {
				t.Fatal(e)
			}
			_, e = r.Head(context.Background())
			var api *gh.APIError
			if !errors.As(e, &api) || api.Status != status || errors.Is(e, domain.ErrUnauthorized) || strings.Contains(e.Error(), sentinel) {
				t.Fatal(e)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	r, e := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, srv.URL, &http.Client{Timeout: 20 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.Head(context.Background())
	var timeout interface{ Timeout() bool }
	if !errors.As(e, &timeout) || !timeout.Timeout() {
		t.Fatal("timeout lost", e)
	}
}

func TestStreamingCancellationAndLengthValidation(t *testing.T) {
	a, r := newAPI(t)
	a.fault("POST", "/git/blobs", 503)
	done := make(chan error, 1)
	go func() {
		_, e := r.CreateBlob(context.Background(), bytes.NewReader(bytes.Repeat([]byte("x"), 8<<20)), 8<<20)
		done <- e
	}()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pipe writer did not terminate")
	}
	a.fault("", "", 0)
	for _, size := range []int64{2, 4} {
		if _, e := r.CreateBlob(context.Background(), strings.NewReader("abc"), size); e == nil {
			t.Fatalf("accepted wrong length %d", size)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := r.CreateBlob(ctx, strings.NewReader("abc"), 3); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestRejectsInvalidLocationsAndRootCommit(t *testing.T) {
	for _, branch := range []string{"", "../main", "HEAD", "@{-1}", "a.lock", "a//b", "bad branch", "main?token=x"} {
		if _, e := gh.New(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: branch}, ""); e == nil {
			t.Fatal("accepted branch", branch)
		}
	}
	a, r := newAPI(t)
	before, e := a.git.Head(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.CreateCommit(context.Background(), "tree", "", "root"); e == nil || !strings.Contains(e.Error(), "README") {
		t.Fatal(e)
	}
	after, e := a.git.Head(context.Background())
	if e != nil || after != before {
		t.Fatal("root rejection changed HEAD", e)
	}
}

func TestCompareAndRejectedRefDistinguishConflict(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	base, e := a.git.Head(ctx)
	if e != nil {
		t.Fatal(e)
	}
	c, e := a.git.Commit(ctx, base)
	if e != nil {
		t.Fatal(e)
	}
	left, e := a.git.CreateCommit(ctx, c.Tree, base, "left")
	if e != nil {
		t.Fatal(e)
	}
	right, e := a.git.CreateCommit(ctx, c.Tree, base, "right")
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		a, b domain.RepositoryHead
		want bool
	}{{base, left, true}, {left, left, true}, {left, base, false}, {left, right, false}} {
		got, e := r.IsAncestor(ctx, tc.a, tc.b)
		if e != nil || got != tc.want {
			t.Fatalf("compare %s...%s = %v: %v", tc.a, tc.b, got, e)
		}
	}
	a.mu.Lock()
	a.rejectionHead = left
	a.mu.Unlock()
	a.fault("PATCH", "/git/refs/", 409)
	e = r.UpdateRef(ctx, base, right)
	var conflict *domain.ConflictError
	if !errors.As(e, &conflict) || conflict.Base != base || conflict.Remote != left {
		t.Fatal("changed HEAD rejection must be conflict", e)
	}
	h, e := a.git.Head(ctx)
	if e != nil || h != left {
		t.Fatal("external publication overwritten", e)
	}
	a.fault("", "", 0)
	if e = r.UpdateRef(ctx, base, right); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("stale base accepted", e)
	}
	h, e = a.git.Head(ctx)
	if e != nil || h != left {
		t.Fatal("stale ref changed HEAD", e)
	}
}

func TestUpdateRefRejectsBaseAheadOfCurrentHead(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	current, err := a.git.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := a.git.Commit(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	base, err := a.git.CreateCommit(ctx, commit.Tree, current, "previous base")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := a.git.CreateCommit(ctx, commit.Tree, base, "new candidate")
	if err != nil {
		t.Fatal(err)
	}
	err = r.UpdateRef(ctx, base, candidate)
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Base != base || conflict.Remote != current {
		t.Fatalf("expected a conflict after the branch was reset, got %v", err)
	}
	head, err := a.git.Head(ctx)
	if err != nil || head != current {
		t.Fatalf("reset branch was changed: %s, %v", head, err)
	}
}

func TestUpdateRefStopsWhenHeadCannotBeRead(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	base, err := a.git.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := a.git.Commit(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := a.git.CreateCommit(ctx, commit.Tree, base, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	a.fault("GET", "/git/ref/heads/", 503)
	err = r.UpdateRef(ctx, base, candidate)
	var api *gh.APIError
	if !errors.As(err, &api) || api.Status != 503 {
		t.Fatalf("expected HEAD read failure, got %v", err)
	}
	head, err := a.git.Head(ctx)
	if err != nil || head != base {
		t.Fatalf("branch changed after failed HEAD read: %s, %v", head, err)
	}
}

type blockedSource struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (s *blockedSource) Read([]byte) (int, error) {
	s.startOnce.Do(func() { close(s.started) })
	<-s.closed
	return 0, io.ErrClosedPipe
}
func (s *blockedSource) Close() error { s.closeOnce.Do(func() { close(s.closed) }); return nil }

func TestUploadCancellationClosesBlockedSourceAndJoinsWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) }))
	defer srv.Close()
	r, e := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, srv.URL, srv.Client())
	if e != nil {
		t.Fatal(e)
	}
	source := &blockedSource{started: make(chan struct{}), closed: make(chan struct{})}
	defer source.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := r.CreateBlob(ctx, source, 100); done <- e }()
	select {
	case <-source.started:
	case <-time.After(3 * time.Second):
		t.Fatal("upload never read source")
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled upload left pipe writer blocked")
	}
	select {
	case <-source.closed:
	default:
		t.Fatal("blocked source was not closed")
	}
}

func TestTimedOutTreeCreationPreservesPendingChanges(t *testing.T) {
	a, r := newAPI(t)
	ctx := context.Background()
	s := service(t)
	if _, e := s.Initialize(ctx, r); e != nil {
		t.Fatal(e)
	}
	asset := stageImage(t, s)
	before, e := s.Store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	client := *a.server.Client()
	client.Timeout = 500 * time.Millisecond
	timed, e := gh.NewWithClient(repository.Location{Backend: "github", Owner: "owner", Name: "gallery", Branch: "main"}, sentinel, a.server.URL, &client)
	if e != nil {
		t.Fatal(e)
	}
	a.fault("POST", "/git/trees", 0)
	_, e = s.Commit(ctx, timed, "timeout")
	var timeout interface{ Timeout() bool }
	if !errors.As(e, &timeout) || !timeout.Timeout() {
		t.Fatal("timeout classification lost", e)
	}
	h, e := a.git.Head(ctx)
	if e != nil {
		t.Fatal(e)
	}
	after, e := s.Store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if h != before.Head || !reflect.DeepEqual(before, after) || after.Changes[asset.ID].Asset == nil {
		t.Fatal("timeout changed HEAD, cache, or stage")
	}
	a.fault("", "", 0)
	if _, e = s.Commit(ctx, r, "retry timeout"); e != nil {
		t.Fatal(e)
	}
	fresh := service(t)
	if _, e = fresh.Sync(ctx, r); e != nil {
		t.Fatal(e)
	}
	restored, e := fresh.Store.Load(ctx)
	if e != nil || !reflect.DeepEqual(restored.Assets[asset.ID], asset) {
		t.Fatal("retry did not restore asset", e)
	}
}
