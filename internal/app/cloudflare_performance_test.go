package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

type countedBlobGit struct {
	repository.Git
	mu    sync.Mutex
	calls int
	data  []byte
	ids   []repository.BlobID
	// delegate reads the wrapped repository instead of replacing every blob
	// with data; used when the commit must still read real metadata.
	delegate bool
}

func (g *countedBlobGit) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	g.mu.Lock()
	g.calls++
	g.ids = append(g.ids, id)
	delegate := g.delegate
	data := g.data
	g.mu.Unlock()
	if delegate {
		return g.Git.OpenBlob(ctx, id)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (g *countedBlobGit) callsFor(id repository.BlobID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for _, seen := range g.ids {
		if seen == id {
			count++
		}
	}
	return count
}
func TestCloudflareStagedPayloadAndFallback(t *testing.T) {
	ctx, git, s := realRepository(t)
	data := batchPNG(t, 91)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "image.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	asset := views[0].Asset
	key := repository.ObjectPath(asset)
	path := staging.PayloadPath(s.Dir, asset)
	source := &countedBlobGit{Git: git, data: data}
	mirror := newFakeMirror()
	run := &cloudflareReconcile{service: s, repo: source, mirror: mirror, payloads: map[string]string{key: path}}
	// The staged fast path exists only for hash-verified bytes; state the
	// expectation explicitly so the test is independent of the upload hash
	// default.
	object := asset.Object
	object.Hash = sha256.Sum256(data)
	ref := cloudflareRef{object: object, entry: repository.GitEntry{Type: "blob", SHA: strings.Repeat("a", 40)}}
	if _, _, err := run.putBlob(ctx, key, ref, ""); err != nil || source.calls != 0 {
		t.Fatalf("staged source: calls=%d error=%v", source.calls, err)
	}
	mirror.clearBucket()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run.putBlob(ctx, key, ref, ""); err != nil || source.calls != 1 {
		t.Fatalf("fallback source: calls=%d error=%v", source.calls, err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	before := mirror.putCount()
	if _, _, err := run.putBlob(ctx, key, ref, ""); err == nil || mirror.putCount() != before || source.calls != 1 {
		t.Fatalf("corrupt payload was uploaded or fell back: %v", err)
	}
}

type parallelMirror struct {
	*fakeMirror
	mu2     sync.Mutex
	starts  int
	release chan struct{}
	fail    bool
}

func (m *parallelMirror) HeadObject(ctx context.Context, key string) (r2.ObjectInfo, bool, error) {
	m.mu2.Lock()
	m.starts++
	n := m.starts
	if n == 4 {
		close(m.release)
	}
	m.mu2.Unlock()
	if n <= 4 {
		select {
		case <-m.release:
		case <-ctx.Done():
			return r2.ObjectInfo{}, false, ctx.Err()
		}
	}
	if m.fail {
		return r2.ObjectInfo{}, false, testR2Failure("head", "network")
	}
	return m.fakeMirror.HeadObject(ctx, key)
}
// TestCloudflareDefaultHashlessCommitUsesStagedPayload proves the default
// hashless upload path does not add a GitHub blob GET: the commit-created blob
// identity is the proof that the private staged payload is the published
// content, so the mirror streams it directly.
func TestCloudflareDefaultHashlessCommitUsesStagedPayload(t *testing.T) {
	ctx, git, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	data := batchPNG(t, 98)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(data), Name: "hashless.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	asset := views[0].Asset
	if asset.Object.HasHash() {
		t.Fatal("default upload unexpectedly carries a digest")
	}
	source := &countedBlobGit{Git: git, delegate: true}
	if result := mustCommit(t, ctx, s, source); result.Cloudflare == nil ||
		result.Cloudflare.Status != "synced" || result.Cloudflare.Uploaded != 1 {
		t.Fatalf("automatic hashless mirror: %+v", result.Cloudflare)
	}
	key := repository.ObjectPath(asset)
	head, err := git.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := repository.ReadSnapshot(ctx, git, head)
	if err != nil {
		t.Fatal(err)
	}
	var blob repository.BlobID
	for _, entry := range snapshot.Entries {
		if entry.Path == key {
			blob = repository.BlobID(entry.SHA)
		}
	}
	if blob == "" {
		t.Fatal("published hashless object has no Git blob")
	}
	if reads := source.callsFor(blob); reads != 0 {
		t.Fatalf("GitHub blob GETs for the image = %d, want 0 with a proven staged payload", reads)
	}
	stored, ok := fake.object(key)
	if !ok || !bytes.Equal(stored.data, data) || stored.sha256 != "" || stored.blob != string(blob) {
		t.Fatalf("mirrored hashless object: %+v", stored)
	}
}

func TestCloudflareParallelBatchStopsOnTargetFailure(t *testing.T) {
	ctx, git, s := realRepository(t)
	inputs := make([]Input, 5)
	for i := range inputs {
		inputs[i] = Input{Reader: bytes.NewReader(batchPNG(t, i+40)), Name: "image.png"}
	}
	if _, err := s.Add(ctx, inputs, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, git)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	m := &parallelMirror{fakeMirror: fake, release: make(chan struct{}), fail: true}
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) { return m, nil }
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.CloudflareSync(deadline, git, CloudflareOptions{})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sync error %v", err)
	}
	if m.starts != 4 || result.Failed != 4 || result.Pending != 1 || fake.putCount() != 0 {
		t.Fatalf("batch starts=%d result=%+v", m.starts, result)
	}
}

func TestCloudflareSyncParallelBatchCompletes(t *testing.T) {
	ctx, git, s := realRepository(t)
	inputs := make([]Input, 5)
	for i := range inputs {
		inputs[i] = Input{Reader: bytes.NewReader(batchPNG(t, i+60)), Name: "image.png"}
	}
	if _, err := s.Add(ctx, inputs, AddOptions{Namespace: "blog"}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, git)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	m := &parallelMirror{fakeMirror: fake, release: make(chan struct{})}
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) { return m, nil }
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.CloudflareSync(deadline, git, CloudflareOptions{})
	if err != nil || result.Status != "synced" || result.Uploaded != 5 || fake.putCount() != 5 {
		t.Fatalf("sync result = %+v, %v", result, err)
	}
}
