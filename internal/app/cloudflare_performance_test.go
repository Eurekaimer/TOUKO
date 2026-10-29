package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
}

func (g *countedBlobGit) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return io.NopCloser(bytes.NewReader(g.data)), nil
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
	ref := cloudflareRef{object: asset.Object, entry: repository.GitEntry{Type: "blob", SHA: "sha"}}
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
