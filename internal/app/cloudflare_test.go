package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/index"
	"github.com/Eurekaimer/TOUKO/internal/r2"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/Eurekaimer/TOUKO/internal/staging"
)

// fakeStoredObject is one key in the fake bucket. Metadata is owned by the
// uploader and is what the reconciliation code must verify, not trust.
type fakeStoredObject struct {
	data         []byte
	etag         string
	repositoryID string
	sourceID     string
	sha256       string
	size         int64
}

type fakePut struct {
	key  string
	data []byte
	opts r2.PutOptions
}

// fakeMirror implements r2.Mirror with real bytes, conditional create/update
// and controllable failures. It deliberately performs no verification of its
// own: the application must HEAD the result before issuing a receipt.
type fakeMirror struct {
	mu       sync.Mutex
	objects  map[string]fakeStoredObject
	puts     []fakePut
	deletes  []string
	heads    []string
	ops      []string
	nextETag int

	headErr   error
	putErr    map[string]error
	deleteErr map[string]error

	headHook func(key string) (r2.ObjectInfo, bool, error)
	afterPut func(key string)

	putRaceAdopt     bool
	putRaceForeign   bool
	putStoreMismatch bool
	blockPut         bool
	putStarted       chan struct{}
	startedOnce      sync.Once
}

func newFakeMirror() *fakeMirror {
	return &fakeMirror{
		objects:    make(map[string]fakeStoredObject),
		putErr:     make(map[string]error),
		deleteErr:  make(map[string]error),
		putStarted: make(chan struct{}),
	}
}

func (f *fakeMirror) HeadObject(ctx context.Context, key string) (r2.ObjectInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return r2.ObjectInfo{}, false, &r2.Error{Op: "head", Key: key, Code: "canceled", Cause: err}
	}
	f.mu.Lock()
	f.heads = append(f.heads, key)
	hook := f.headHook
	headErr := f.headErr
	stored, exists := f.objects[key]
	f.mu.Unlock()
	if hook != nil {
		return hook(key)
	}
	if headErr != nil {
		return r2.ObjectInfo{}, false, headErr
	}
	if !exists {
		return r2.ObjectInfo{}, false, nil
	}
	return r2.ObjectInfo{
		ETag:         stored.etag,
		RepositoryID: stored.repositoryID,
		SourceID:     stored.sourceID,
		SHA256:       stored.sha256,
		Size:         stored.size,
	}, true, nil
}

func (f *fakeMirror) PutObject(ctx context.Context, key string, body io.ReadSeeker, opts r2.PutOptions) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", &r2.Error{Op: "put", Key: key, Code: "network", Cause: err}
	}
	f.mu.Lock()
	f.puts = append(f.puts, fakePut{key: key, data: append([]byte(nil), data...), opts: opts})
	f.ops = append(f.ops, "put:"+key)
	block := f.blockPut
	started := f.putStarted
	putErr := f.putErr[key]
	raceAdopt := f.putRaceAdopt
	raceForeign := f.putRaceForeign
	mismatch := f.putStoreMismatch
	current, exists := f.objects[key]
	f.mu.Unlock()
	if block {
		f.startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		return "", &r2.Error{Op: "put", Key: key, Code: "canceled", Cause: ctx.Err()}
	}
	if putErr != nil {
		return "", putErr
	}
	if opts.IfMatch == "" {
		if exists {
			return "", &r2.Error{Op: "put", Key: key, Code: "precondition_failed"}
		}
	} else if !exists || current.etag != opts.IfMatch {
		return "", &r2.Error{Op: "put", Key: key, Code: "precondition_failed"}
	}
	repositoryID, sourceID := opts.RepositoryID.String(), opts.SourceID
	if raceAdopt || raceForeign {
		if raceForeign {
			repositoryID = "00000000-0000-0000-0000-000000000001"
			sourceID = strings.Repeat("f", 64)
		}
		f.mu.Lock()
		f.nextETag++
		f.objects[key] = fakeStoredObject{
			data: append([]byte(nil), data...), etag: fmt.Sprintf("etag-%d", f.nextETag),
			repositoryID: repositoryID, sourceID: sourceID,
			sha256: opts.Object.Hash.String(), size: opts.Object.Size,
		}
		f.putRaceAdopt, f.putRaceForeign = false, false
		f.mu.Unlock()
		return "", &r2.Error{Op: "put", Key: key, Code: "precondition_failed"}
	}
	sha := opts.Object.Hash.String()
	if mismatch {
		sha = strings.Repeat("0", 64)
	}
	f.mu.Lock()
	f.nextETag++
	etag := fmt.Sprintf("etag-%d", f.nextETag)
	f.objects[key] = fakeStoredObject{
		data: append([]byte(nil), data...), etag: etag,
		repositoryID: repositoryID, sourceID: sourceID,
		sha256: sha, size: opts.Object.Size,
	}
	f.mu.Unlock()
	if f.afterPut != nil {
		f.afterPut(key)
	}
	return etag, nil
}

func (f *fakeMirror) DeleteObject(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return &r2.Error{Op: "delete", Key: key, Code: "canceled", Cause: err}
	}
	f.mu.Lock()
	f.deletes = append(f.deletes, key)
	f.ops = append(f.ops, "delete:"+key)
	err := f.deleteErr[key]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	return nil
}

func (f *fakeMirror) putKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.puts))
	for _, p := range f.puts {
		out = append(out, p.key)
	}
	return out
}

func (f *fakeMirror) lastPut() (fakePut, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) == 0 {
		return fakePut{}, false
	}
	return f.puts[len(f.puts)-1], true
}

func (f *fakeMirror) deleteKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeMirror) headCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.heads)
}

func (f *fakeMirror) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func (f *fakeMirror) operationIndex(want string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, op := range f.ops {
		if op == want {
			return i
		}
	}
	return -1
}

func (f *fakeMirror) object(key string) (fakeStoredObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	return o, ok
}

func (f *fakeMirror) removeObject(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

func (f *fakeMirror) clearBucket() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects = make(map[string]fakeStoredObject)
}

func (f *fakeMirror) setOwned(key string, data []byte, repositoryID, sourceID, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256(data)
	f.objects[key] = fakeStoredObject{
		data: append([]byte(nil), data...), etag: etag,
		repositoryID: repositoryID, sourceID: sourceID,
		sha256: hex.EncodeToString(sum[:]), size: int64(len(data)),
	}
}

const (
	fakeCFEndpoint  = "https://acct.r2.cloudflarestorage.com"
	fakeCFBucket    = "touko-images"
	fakeCFPublicURL = "https://img.eurekaimer.test"
)

func testCloudflareConfig() r2.Config {
	return r2.Config{Enabled: true, Endpoint: fakeCFEndpoint, Bucket: fakeCFBucket, PublicBaseURL: fakeCFPublicURL}
}

func writeTestCloudflareConfig(t *testing.T, dir string, cfg r2.Config, private bool) {
	t.Helper()
	content := fmt.Sprintf(`{"backend":"github","branch":"main","owner":"Owner","name":"Gallery","private":%t,"cloudflare":{"enabled":%t,"endpoint":%q,"bucket":%q,"public_base_url":%q}}`+"\n",
		private, cfg.Enabled, cfg.Endpoint, cfg.Bucket, cfg.PublicBaseURL)
	writeCloudflareConfigTestFile(t, dir, content)
}

func enableTestCloudflare(t *testing.T, s *Service, fake *fakeMirror, cfg r2.Config) {
	t.Helper()
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "fixture-access-key")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "fixture-secret-key")
	writeTestCloudflareConfig(t, s.Dir, cfg, false)
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) { return fake, nil }
}

func testSourceID() string {
	raw, err := json.Marshal([]string{"owner", "gallery", "main"})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func testLedger(t *testing.T, ctx context.Context, s *Service, cfg r2.Config) index.MirrorLedger {
	t.Helper()
	ledger, err := index.LoadMirror(ctx, s.Dir, cfg.TargetID())
	if err != nil {
		t.Fatalf("load mirror ledger: %v", err)
	}
	return ledger
}

func ledgerEntry(t *testing.T, ledger index.MirrorLedger, key string) index.MirrorEntry {
	t.Helper()
	for _, entry := range ledger.Entries {
		if entry.Key == key {
			return entry
		}
	}
	t.Fatalf("mirror ledger has no entry for %s: %+v", key, ledger)
	return index.MirrorEntry{}
}

func testR2Failure(op, code string) error {
	return &r2.Error{Op: op, Code: code, Cause: errors.New("synthetic transport failure")}
}

func testRepositoryID(t *testing.T, ctx context.Context, s *Service) domain.RepositoryID {
	t.Helper()
	st, err := s.Store.Load(ctx)
	if err != nil || st.Manifest == nil {
		t.Fatalf("load repository identity: %v", err)
	}
	return st.Manifest.ID
}

func assertOwnedObject(t *testing.T, fake *fakeMirror, key string, data []byte, object domain.ContentObject, repositoryID domain.RepositoryID) {
	t.Helper()
	stored, ok := fake.object(key)
	if !ok {
		t.Fatalf("fake bucket has no object %s", key)
	}
	want := sha256.Sum256(data)
	if !bytes.Equal(stored.data, data) {
		t.Fatalf("object bytes differ for %s", key)
	}
	if stored.size != object.Size || stored.sha256 != hex.EncodeToString(want[:]) ||
		stored.repositoryID != repositoryID.String() || stored.sourceID != testSourceID() || stored.etag == "" {
		t.Fatalf("object metadata differs for %s: %+v", key, stored)
	}
}

func TestCloudflareSyncUploadsSkipsAndBuildsURL(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	payload, err := os.ReadFile(staging.PayloadPath(s.Dir, asset))
	if err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	fake := newFakeMirror()
	cfg := testCloudflareConfig()
	enableTestCloudflare(t, s, fake, cfg)

	before, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head(ctx)
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil {
		t.Fatalf("CloudflareSync failed: %v", err)
	}
	key := repository.ObjectPath(asset)
	if result.Status != "synced" || result.DryRun || result.Uploaded != 1 || result.Failed != 0 || result.Pending != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Items) != 1 || result.Items[0].ObjectPath != key || result.Items[0].Action != "put" || result.Items[0].Status != "synced" {
		t.Fatalf("unexpected items: %+v", result.Items)
	}
	assertOwnedObject(t, fake, key, payload, asset.Object, testRepositoryID(t, ctx, s))
	ledger := testLedger(t, ctx, s, cfg)
	if ledger.TargetID != cfg.TargetID() || ledger.Endpoint != cfg.Endpoint || ledger.Bucket != cfg.Bucket || ledger.SourceID != testSourceID() {
		t.Fatalf("unexpected ledger identity: %+v", ledger)
	}
	entry := ledgerEntry(t, ledger, key)
	if entry.State != "synced" || entry.ETag == "" || entry.Object != asset.Object || entry.ErrorCode != "" {
		t.Fatalf("unexpected receipt: %+v", entry)
	}
	gotURL, err := s.CloudflareURL(ctx, asset.ID.String())
	if err != nil || gotURL != fakeCFPublicURL+"/"+key {
		t.Fatalf("CloudflareURL = %q, %v; want %q", gotURL, err, fakeCFPublicURL+"/"+key)
	}

	after, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("explicit Cloudflare sync changed the committed cache or staged work")
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("explicit Cloudflare sync changed the Git ref")
	}

	result, err = s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil {
		t.Fatalf("second CloudflareSync failed: %v", err)
	}
	if result.Uploaded != 0 || result.Skipped != 1 || fake.putCount() != 1 {
		t.Fatalf("repeat sync rewrote objects: %+v puts=%d", result, fake.putCount())
	}
	stored, _ := fake.object(key)
	if stored.etag != entry.ETag {
		t.Fatal("repeat sync changed the object ETag")
	}
}

func TestCloudflareAutoPublishesAddsOnly(t *testing.T) {
	ctx, r, s := realRepository(t)
	oldAsset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())

	newBytes := batchPNG(t, 7)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(newBytes), Name: "new.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	newAsset := views[0].Asset
	result := mustCommit(t, ctx, s, r)
	if result.Cloudflare == nil || result.Cloudflare.Status != "synced" || result.Cloudflare.Uploaded != 1 {
		t.Fatalf("automatic mirror result: %+v", result.Cloudflare)
	}
	newKey := repository.ObjectPath(newAsset)
	if keys := fake.putKeys(); len(keys) != 1 || keys[0] != newKey {
		t.Fatalf("automatic mirror uploaded %v; want only %s", keys, newKey)
	}
	assertOwnedObject(t, fake, newKey, newBytes, newAsset.Object, testRepositoryID(t, ctx, s))
	if _, ok := fake.object(repository.ObjectPath(oldAsset)); ok {
		t.Fatal("automatic mirror backfilled an old unrelated asset")
	}
	ledger := testLedger(t, ctx, s, testCloudflareConfig())
	if len(ledger.Entries) != 1 || ledgerEntry(t, ledger, newKey).State != "synced" {
		t.Fatalf("unexpected ledger: %+v", ledger)
	}

	title := "metadata only"
	if err := s.Edit(ctx, newAsset.ID.String(), EditOptions{Title: &title}); err != nil {
		t.Fatal(err)
	}
	before := fake.putCount()
	edited := mustCommit(t, ctx, s, r)
	if fake.putCount() != before {
		t.Fatal("metadata-only edit retriggered an upload")
	}
	if edited.Cloudflare != nil && edited.Cloudflare.Status != "synced" {
		t.Fatalf("metadata edit reported a mirror failure: %+v", edited.Cloudflare)
	}
}

func TestCloudflareAutoFailureKeepsPublicationAndRecovers(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	fake.headErr = testR2Failure("head", "network")
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	asset := stageImage(t, ctx, s)
	result := mustCommit(t, ctx, s, r)
	if result.Cloudflare == nil || result.Cloudflare.Status != "failed" || result.Cloudflare.ErrorCode != "network" {
		t.Fatalf("unexpected mirror result: %+v", result.Cloudflare)
	}
	head, err := r.Head(ctx)
	if err != nil || head != result.Head {
		t.Fatal(head, result.Head, err)
	}
	snap, err := repository.ReadSnapshot(ctx, r, head)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Assets[asset.ID].Object != asset.Object {
		t.Fatal("GitHub publication differs after mirror failure")
	}
	verified := false
	for _, entry := range snap.Entries {
		if entry.Path == repository.ObjectPath(asset) {
			verified = true
			if err := repository.VerifyObject(ctx, r, repository.BlobID(entry.SHA), asset.Object); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !verified {
		t.Fatal("published object entry missing")
	}
	st, err := s.Store.Load(ctx)
	if err != nil || len(st.Changes) != 0 || st.Publication != nil || st.Head != result.Head {
		t.Fatalf("publication was rolled back or staging retained: %+v %v", st, err)
	}
	key := repository.ObjectPath(asset)
	ledger := testLedger(t, ctx, s, testCloudflareConfig())
	entry := ledgerEntry(t, ledger, key)
	if entry.State != "failed" || entry.ErrorCode != "network" {
		t.Fatalf("target-wide failure did not leave a replayable receipt: %+v", entry)
	}

	fake.mu.Lock()
	fake.headErr = nil
	fake.mu.Unlock()
	recovered, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil || recovered.Status != "synced" || recovered.Uploaded != 1 {
		t.Fatalf("recovery sync = %+v, %v", recovered, err)
	}
	if entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), key); entry.State != "synced" {
		t.Fatalf("receipt not recovered: %+v", entry)
	}
}

func TestCloudflareAutoDeletesOnlyUnreferencedKeys(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	first := stageImage(t, ctx, s)
	result := mustCommit(t, ctx, s, r)
	if result.Cloudflare == nil || result.Cloudflare.Uploaded != 1 {
		t.Fatalf("first automatic upload: %+v", result.Cloudflare)
	}
	secondViews, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 8)), Name: "second.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	second := secondViews[0].Asset
	mustCommit(t, ctx, s, r)
	if err := s.Remove(ctx, []string{first.ID.String()}); err != nil {
		t.Fatal(err)
	}
	result = mustCommit(t, ctx, s, r)
	if result.Cloudflare == nil || result.Cloudflare.Deleted != 1 {
		t.Fatalf("delete was not mirrored: %+v", result.Cloudflare)
	}
	firstKey, secondKey := repository.ObjectPath(first), repository.ObjectPath(second)
	if deletes := fake.deleteKeys(); len(deletes) != 1 || deletes[0] != firstKey {
		t.Fatalf("deleted keys = %v; want [%s]", deletes, firstKey)
	}
	if _, ok := fake.object(secondKey); !ok {
		t.Fatal("still-referenced object was deleted")
	}
	ledger := testLedger(t, ctx, s, testCloudflareConfig())
	if ledgerEntry(t, ledger, firstKey).State != "deleted" {
		t.Fatal("delete tombstone missing")
	}
	if ledgerEntry(t, ledger, secondKey).State != "synced" {
		t.Fatal("remaining receipt changed")
	}
}

func testPNGAsset(t *testing.T, data []byte, assetPath string) domain.Asset {
	t.Helper()
	id, err := domain.NewAssetID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	return domain.Asset{
		ID: id, Namespace: "blog",
		Object:       domain.ContentObject{Hash: sha256.Sum256(data), MIME: "image/png", Ext: "png", Size: int64(len(data)), Width: 64, Height: 48},
		Path:         assetPath,
		OriginalName: "shared.png", Title: "shared", Tags: []string{}, Collections: []string{},
		CreatedAt: now, UpdatedAt: now,
	}
}

func publishV2Assets(t *testing.T, ctx context.Context, r repository.Git, s *Service, assets []domain.Asset, objectData map[string][]byte) domain.RepositoryHead {
	t.Helper()
	st, err := s.Store.Load(ctx)
	if err != nil || st.Manifest == nil {
		t.Fatal(st, err)
	}
	manifest := *st.Manifest
	manifest.FormatVersion = 2
	manifestData, err := repository.EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	changes := []repository.TreeChange{{Path: repository.ManifestPath, Blob: blob(t, ctx, r, manifestData)}}
	seen := make(map[string]bool)
	for _, asset := range assets {
		metadata, err := repository.EncodeAsset(asset)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, repository.TreeChange{Path: repository.AssetPath(asset.ID), Blob: blob(t, ctx, r, metadata)})
		objectPath := repository.ObjectPath(asset)
		if seen[objectPath] {
			continue
		}
		seen[objectPath] = true
		data, ok := objectData[objectPath]
		if !ok {
			t.Fatalf("missing object data for %s", objectPath)
		}
		changes = append(changes, repository.TreeChange{Path: objectPath, Blob: blob(t, ctx, r, data)})
	}
	head := publishRaw(t, ctx, r, changes)
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	return head
}

func TestCloudflareSharedPlacementSurvivesPartialDelete(t *testing.T) {
	ctx, r, s := realRepository(t)
	data := batchPNG(t, 3)
	first := testPNGAsset(t, data, "images/shared.png")
	second := testPNGAsset(t, data, "images/shared.png")
	publishV2Assets(t, ctx, r, s, []domain.Asset{first, second}, map[string][]byte{"images/shared.png": data})
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil || result.Uploaded != 1 {
		t.Fatalf("shared placement upload = %+v, %v", result, err)
	}
	if err := s.Remove(ctx, []string{first.ID.String()}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	if len(fake.deleteKeys()) != 0 {
		t.Fatal("deleting one of two assets sharing a path deleted the object")
	}
	if _, ok := fake.object("images/shared.png"); !ok {
		t.Fatal("shared object disappeared")
	}
	if err := s.Remove(ctx, []string{second.ID.String()}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	if deletes := fake.deleteKeys(); len(deletes) != 1 || deletes[0] != "images/shared.png" {
		t.Fatalf("last reference did not delete the object: %v", deletes)
	}
}

func TestCloudflareSyncDryRunWritesNothing(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	cfg := testCloudflareConfig()
	fm := newFakeMirror()
	enableTestCloudflare(t, s, fm, cfg)

	before, err := s.Store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head(ctx)
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if result.Status != "dry_run" || !result.DryRun || result.Uploaded != 0 || len(result.Items) != 1 ||
		result.Items[0].Action != "put" || result.Items[0].Status != "would_upload" {
		t.Fatalf("unexpected dry-run result: %+v", result)
	}
	if fm.putCount() != 0 || len(fm.deleteKeys()) != 0 || fm.headCount() == 0 {
		t.Fatalf("dry run mutated the bucket: puts=%d deletes=%v heads=%d", fm.putCount(), fm.deleteKeys(), fm.headCount())
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "cloudflare")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created a mirror ledger: %v", err)
	}
	after, _ := s.Store.Load(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("dry run changed committed state")
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("dry run changed the Git ref")
	}

	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(s.Dir, "cloudflare", cfg.TargetID()+".json")
	ledgerBefore, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	putsBefore := fm.putCount()
	result, err = s.CloudflareSync(ctx, r, CloudflareOptions{DryRun: true})
	if err != nil {
		t.Fatalf("second dry run failed: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != "exists" || result.Items[0].Action != "skip" {
		t.Fatalf("existing object not reported: %+v", result.Items)
	}
	ledgerAfter, err := os.ReadFile(ledgerPath)
	if err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
		t.Fatalf("dry run rewrote the ledger: %v", err)
	}
	if fm.putCount() != putsBefore {
		t.Fatal("dry run uploaded an existing object")
	}
}

func TestCloudflareSyncDefersDeleteUntilPutsFinish(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	old := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	if err := s.Remove(ctx, []string{old.ID.String()}); err != nil {
		t.Fatal(err)
	}
	newBytes := batchPNG(t, 11)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(newBytes), Name: "replacement.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	newAsset := views[0].Asset
	newKey := repository.ObjectPath(newAsset)
	fake.mu.Lock()
	fake.putErr[newKey] = testR2Failure("put", "network")
	fake.mu.Unlock()
	result := mustCommit(t, ctx, s, r)
	if result.Cloudflare == nil || result.Cloudflare.Status == "synced" {
		t.Fatalf("failed upload reported success: %+v", result.Cloudflare)
	}
	if len(fake.deleteKeys()) != 0 {
		t.Fatal("delete ran before all uploads completed")
	}
	ledger := testLedger(t, ctx, s, testCloudflareConfig())
	oldKey := repository.ObjectPath(old)
	if entry := ledgerEntry(t, ledger, oldKey); entry.State != "delete_pending" || entry.ErrorCode != "deferred_put_failure" {
		t.Fatalf("delete intent not deferred: %+v", entry)
	}
	if entry := ledgerEntry(t, ledger, newKey); entry.State != "failed" || entry.ErrorCode != "network" {
		t.Fatalf("upload attempt not recorded as failed: %+v", entry)
	}

	fake.mu.Lock()
	delete(fake.putErr, newKey)
	fake.mu.Unlock()
	recovered, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil || recovered.Status != "synced" {
		t.Fatalf("recovery = %+v, %v", recovered, err)
	}
	if fmPut := fake.operationIndex("put:" + newKey); fmPut < 0 {
		t.Fatal("replacement was not uploaded")
	}
	if fmDelete := fake.operationIndex("delete:" + oldKey); fmDelete < 0 {
		t.Fatal("old placement was not deleted")
	}
	if fake.operationIndex("put:"+newKey) > fake.operationIndex("delete:"+oldKey) {
		t.Fatal("delete ran before upload in the same reconciliation")
	}
	if _, ok := fake.object(oldKey); ok {
		t.Fatal("unreferenced object still exists after recovery")
	}
}

type headAdvanceGit struct {
	repository.Git
	mu      sync.Mutex
	calls   int
	at      int
	once    sync.Once
	advance func()
}

func (g *headAdvanceGit) Head(ctx context.Context) (domain.RepositoryHead, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	if call >= g.at && g.advance != nil {
		g.once.Do(g.advance)
	}
	return g.Git.Head(ctx)
}

func TestCloudflareSyncRechecksAuthoritativeHeadBeforeDelete(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	s.NewCloudflare = nil // publish the deletion without letting the automatic hook delete.
	if err := s.Remove(ctx, []string{asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) { return fake, nil }
	wrapped := &headAdvanceGit{Git: r, at: 2}
	wrapped.advance = func() {
		publishRaw(t, ctx, r, []repository.TreeChange{{Path: "README.md", Blob: blob(t, ctx, r, []byte("remote moved"))}})
	}
	_, err := s.CloudflareSync(ctx, wrapped, CloudflareOptions{})
	if err == nil {
		t.Fatal("expected the deferred delete to be reported")
	}
	var syncErr *CloudflareSyncError
	if !errors.As(err, &syncErr) || syncErr.Result.Status != "pending" {
		t.Fatalf("unexpected error: %v (%+v)", err, syncErr)
	}
	if len(fake.deleteKeys()) != 0 {
		t.Fatal("object deleted although the authoritative head moved")
	}
	ledger := testLedger(t, ctx, s, testCloudflareConfig())
	if entry := ledgerEntry(t, ledger, repository.ObjectPath(asset)); entry.State != "delete_pending" || entry.ErrorCode != "source_changed" {
		t.Fatalf("unexpected delete intent: %+v", entry)
	}
}

func TestCloudflarePreconditionAdoptAndForeignConflict(t *testing.T) {
	t.Run("adopt concurrent matching object", func(t *testing.T) {
		ctx, r, s := realRepository(t)
		asset := stageImage(t, ctx, s)
		mustCommit(t, ctx, s, r)
		fake := newFakeMirror()
		fake.putRaceAdopt = true
		enableTestCloudflare(t, s, fake, testCloudflareConfig())
		result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
		if err != nil || result.Status != "synced" || result.Failed != 0 {
			t.Fatalf("adopt = %+v, %v", result, err)
		}
		entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), repository.ObjectPath(asset))
		if entry.State != "synced" || entry.ErrorCode != "" {
			t.Fatalf("adopted object not recorded: %+v", entry)
		}
	})

	t.Run("refuse foreign object", func(t *testing.T) {
		ctx, r, s := realRepository(t)
		asset := stageImage(t, ctx, s)
		mustCommit(t, ctx, s, r)
		fake := newFakeMirror()
		fake.putRaceForeign = true
		enableTestCloudflare(t, s, fake, testCloudflareConfig())
		result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
		var syncErr *CloudflareSyncError
		if !errors.As(err, &syncErr) || result.Status != "failed" || result.Failed != 1 {
			t.Fatalf("foreign race = %+v, %v", result, err)
		}
		entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), repository.ObjectPath(asset))
		if entry.State != "failed" || entry.ErrorCode != "object_conflict" {
			t.Fatalf("foreign object not reported: %+v", entry)
		}
		stored, _ := fake.object(repository.ObjectPath(asset))
		if stored.repositoryID == testRepositoryID(t, ctx, s).String() {
			t.Fatal("foreign object was overwritten")
		}
	})
}

func TestCloudflareOwnedUpdateUsesIfMatch(t *testing.T) {
	ctx, r, s := realRepository(t)
	firstData := batchPNG(t, 21)
	asset := testPNGAsset(t, firstData, "images/owned.png")
	publishV2Assets(t, ctx, r, s, []domain.Asset{asset}, map[string][]byte{"images/owned.png": firstData})
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	firstStored, ok := fake.object("images/owned.png")
	if !ok {
		t.Fatal("initial object missing")
	}

	secondData := batchPNG(t, 22)
	updated := asset
	updated.Object = domain.ContentObject{Hash: sha256.Sum256(secondData), MIME: "image/png", Ext: "png", Size: int64(len(secondData)), Width: 64, Height: 48}
	updated.UpdatedAt = updated.UpdatedAt.Add(time.Minute)
	metadata, err := repository.EncodeAsset(updated)
	if err != nil {
		t.Fatal(err)
	}
	publishRaw(t, ctx, r, []repository.TreeChange{
		{Path: repository.AssetPath(asset.ID), Blob: blob(t, ctx, r, metadata)},
		{Path: "images/owned.png", Blob: blob(t, ctx, r, secondData)},
	})
	if _, err := s.Sync(ctx, r); err != nil {
		t.Fatal(err)
	}
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil || result.Status != "synced" || result.Uploaded != 1 {
		t.Fatalf("owned update = %+v, %v", result, err)
	}
	last, _ := fake.lastPut()
	if last.opts.IfMatch != firstStored.etag {
		t.Fatalf("conditional update If-Match = %q; want %q", last.opts.IfMatch, firstStored.etag)
	}
	stored, _ := fake.object("images/owned.png")
	if !bytes.Equal(stored.data, secondData) {
		t.Fatal("owned object was not updated")
	}
	if entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), "images/owned.png"); entry.State != "synced" || entry.Object != updated.Object {
		t.Fatalf("updated receipt: %+v", entry)
	}
}

func TestCloudflareTombstoneDeletesLateObject(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	key := repository.ObjectPath(asset)
	original, ok := fake.object(key)
	if !ok {
		t.Fatal("uploaded object missing")
	}
	if err := s.Remove(ctx, []string{asset.ID.String()}); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, ctx, s, r)
	if deletes := fake.deleteKeys(); len(deletes) != 1 {
		t.Fatalf("first delete = %v", deletes)
	}
	if entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), key); entry.State != "deleted" {
		t.Fatalf("tombstone missing: %+v", entry)
	}
	// A timed-out PUT may land after the delete. The tombstone must check it.
	fake.setOwned(key, original.data, original.repositoryID, original.sourceID, "etag-late")
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil {
		t.Fatalf("tombstone retry failed: %v", err)
	}
	if result.Status != "synced" || len(fake.deleteKeys()) != 2 {
		t.Fatalf("late object was not removed: %+v deletes=%v", result, fake.deleteKeys())
	}
	if _, ok := fake.object(key); ok {
		t.Fatal("late object still exists")
	}
}

func TestCloudflareHashMismatchFailsWithoutPut(t *testing.T) {
	ctx, r, s := realRepository(t)
	data := batchPNG(t, 31)
	asset := testPNGAsset(t, data, "images/corrupt.png")
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-8] ^= 0xff
	publishV2Assets(t, ctx, r, s, []domain.Asset{asset}, map[string][]byte{"images/corrupt.png": corrupt})
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	var syncErr *CloudflareSyncError
	if !errors.As(err, &syncErr) || result.Status != "failed" || result.Failed != 1 {
		t.Fatalf("hash mismatch = %+v, %v", result, err)
	}
	if len(result.Items) != 1 || result.Items[0].ErrorCode != "hash_mismatch" {
		t.Fatalf("unexpected items: %+v", result.Items)
	}
	if fake.putCount() != 0 {
		t.Fatal("object was uploaded despite a hash mismatch")
	}
	entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), "images/corrupt.png")
	if entry.State != "failed" || entry.ErrorCode != "hash_mismatch" {
		t.Fatalf("unexpected failure receipt: %+v", entry)
	}
}

func TestCloudflareCorruptLedgerFailsClosed(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	cfg := testCloudflareConfig()
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, cfg)
	ledgerPath := filepath.Join(s.Dir, "cloudflare", cfg.TargetID()+".json")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"version":1,"entries":"broken"}`)
	if err := os.WriteFile(ledgerPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head(ctx)
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	var syncErr *CloudflareSyncError
	if !errors.As(err, &syncErr) || result.Status != "failed" || result.ErrorCode != "mirror_state_invalid" {
		t.Fatalf("corrupt ledger = %+v, %v", result, err)
	}
	if fake.putCount() != 0 || len(fake.deleteKeys()) != 0 || fake.headCount() != 0 {
		t.Fatal("corrupt ledger still triggered network operations")
	}
	after, _ := os.ReadFile(ledgerPath)
	if !bytes.Equal(after, corrupt) {
		t.Fatal("corrupt ledger was overwritten")
	}
	if current, _ := r.Head(ctx); current != head {
		t.Fatal("corrupt ledger affected Git")
	}

	title := "still publishable"
	if err := s.Edit(ctx, asset.ID.String(), EditOptions{Title: &title}); err != nil {
		t.Fatal(err)
	}
	commit := mustCommit(t, ctx, s, r)
	if commit.Cloudflare == nil || commit.Cloudflare.Status != "failed" || commit.Cloudflare.ErrorCode != "mirror_state_invalid" {
		t.Fatalf("automatic mirror did not report corrupt state: %+v", commit.Cloudflare)
	}
	after, _ = os.ReadFile(ledgerPath)
	if !bytes.Equal(after, corrupt) {
		t.Fatal("automatic mirror removed the corrupt ledger")
	}
}

func TestCloudflareW1FailureDoesNotTouchBucket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based W1 failure cannot be simulated as root")
	}
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	dir := filepath.Join(s.Dir, "cloudflare")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	var syncErr *CloudflareSyncError
	if !errors.As(err, &syncErr) || result.Status != "failed" || result.ErrorCode != "mirror_state_write_failed" {
		t.Fatalf("W1 failure = %+v, %v", result, err)
	}
	if fake.putCount() != 0 || len(fake.deleteKeys()) != 0 {
		t.Fatal("bucket mutated after the intent write failed")
	}
	if _, statErr := os.Stat(filepath.Join(dir, testCloudflareConfig().TargetID()+".json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("W1 failure still produced a receipt")
	}
	_ = asset
}

func TestCloudflareW2FailureRecoversFromIntent(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	cfg := testCloudflareConfig()
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, cfg)
	ledgerPath := filepath.Join(s.Dir, "cloudflare", cfg.TargetID()+".json")
	backup := ledgerPath + ".w1"
	moved := false
	fake.afterPut = func(string) {
		if err := os.Rename(ledgerPath, backup); err != nil {
			t.Errorf("move W1 ledger: %v", err)
			return
		}
		if err := os.Mkdir(ledgerPath, 0700); err != nil {
			t.Errorf("block W2 ledger: %v", err)
			return
		}
		moved = true
	}
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	var syncErr *CloudflareSyncError
	if !errors.As(err, &syncErr) || result.Status != "failed" || result.ErrorCode != "mirror_state_write_failed" {
		t.Fatalf("W2 failure = %+v, %v", result, err)
	}
	if !moved {
		t.Fatal("test did not block the receipt write")
	}
	fake.afterPut = nil
	if err := os.RemoveAll(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, ledgerPath); err != nil {
		t.Fatal(err)
	}
	key := repository.ObjectPath(asset)
	entry := ledgerEntry(t, testLedger(t, ctx, s, cfg), key)
	if entry.State != "pending" {
		t.Fatalf("W1 intent not durable: %+v", entry)
	}
	result, err = s.CloudflareSync(ctx, r, CloudflareOptions{})
	if err != nil || result.Status != "synced" || result.Uploaded != 0 || result.Skipped != 1 {
		t.Fatalf("HEAD replay = %+v, %v", result, err)
	}
	if fake.putCount() != 1 {
		t.Fatalf("object was uploaded again after W2 failure: %d puts", fake.putCount())
	}
	if entry := ledgerEntry(t, testLedger(t, ctx, s, cfg), key); entry.State != "synced" {
		t.Fatalf("receipt not adopted: %+v", entry)
	}
}

func TestCloudflareStatusUsesOnlyReadViewAndLedger(t *testing.T) {
	ctx, r, s := realRepository(t)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "access-SENTINEL")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "secret-SENTINEL")
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) {
		t.Error("CloudflareStatus must not construct an R2 client")
		return nil, errors.New("must not be called")
	}
	st, views, err := s.ReadView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := s.CloudflareStatus(ctx, st, views)
	if status == nil || !status.Enabled || !status.Configured || status.ErrorCode != "" || status.PendingDeletes != 0 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if got := status.Assets[asset.ID.String()]; got.State != "synced" || got.Reason != "" {
		t.Fatalf("unexpected asset status: %+v", got)
	}

	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	status = s.CloudflareStatus(ctx, st, views)
	if status == nil || status.Configured || status.ErrorCode != "credentials_missing" {
		t.Fatalf("missing credentials status: %+v", status)
	}

	writeCloudflareConfigTestFile(t, s.Dir, cloudflareConfigJSON(`"cloudflare":{"enabled":false}`))
	if status = s.CloudflareStatus(ctx, st, views); status != nil {
		t.Fatalf("disabled Cloudflare returned status: %+v", status)
	}
	writeCloudflareConfigTestFile(t, s.Dir, cloudflareConfigJSON(`"cloudflare":{"enabled":true,"endpoint":"http://bad","bucket":"images","public_base_url":"https://img.example.test"}`))
	status = s.CloudflareStatus(ctx, st, views)
	if status == nil || !status.Enabled || status.Configured || status.ErrorCode != "configuration_invalid" {
		t.Fatalf("invalid configuration status: %+v", status)
	}
	if len(status.Assets) != 0 {
		t.Fatal("invalid configuration exposed asset receipts")
	}
}

func TestCloudflareURLReceiptRules(t *testing.T) {
	ctx, r, s := realRepository(t)
	oldAsset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	fake := newFakeMirror()
	cfg := testCloudflareConfig()
	enableTestCloudflare(t, s, fake, cfg)
	assetViews, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 41)), Name: "synced.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	asset := assetViews[0].Asset
	if result := mustCommit(t, ctx, s, r); result.Cloudflare == nil || result.Cloudflare.Status != "synced" {
		t.Fatalf("automatic upload for URL test: %+v", result.Cloudflare)
	}
	key := repository.ObjectPath(asset)
	url, err := s.CloudflareURL(ctx, asset.ID.String())
	if err != nil || url != fakeCFPublicURL+"/"+key {
		t.Fatalf("committed URL = %q, %v", url, err)
	}
	if _, err := s.CloudflareURL(ctx, oldAsset.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("asset without a receipt produced a URL: %v", err)
	}

	// A synced private GitHub repository may expose only the explicitly enabled mirror.
	writeTestCloudflareConfig(t, s.Dir, cfg, true)
	if url, err := s.CloudflareURL(ctx, asset.ID.String()); err != nil || url != fakeCFPublicURL+"/"+key {
		t.Fatalf("private enabled URL = %q, %v", url, err)
	}
	writeTestCloudflareConfig(t, s.Dir, cfg, false)

	stagedBytes := batchPNG(t, 42)
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(stagedBytes), Name: "staged.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloudflareURL(ctx, views[0].Asset.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("staged asset produced a Cloudflare URL: %v", err)
	}

	fake.removeObject(key)
	if _, err := s.CloudflareURL(ctx, asset.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("missing object produced a URL: %v", err)
	}
	if entry := ledgerEntry(t, testLedger(t, ctx, s, cfg), key); entry.State != "pending" || entry.ErrorCode != "not_found" {
		t.Fatalf("missing object did not invalidate the receipt: %+v", entry)
	}
}

func TestCloudflareDisabledAndLocalDoNothing(t *testing.T) {
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "invalid value")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "\tinvalid")
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) {
		t.Error("disabled Cloudflare constructed an R2 client")
		return nil, errors.New("must not be called")
	}
	result := mustCommit(t, ctx, s, r)
	if result.Cloudflare != nil {
		t.Fatalf("disabled Cloudflare produced a result: %+v", result.Cloudflare)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "cloudflare")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled Cloudflare created a ledger")
	}
	syncResult, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if !errors.Is(err, domain.ErrDeliveryUnavailable) || syncResult.Status != "unavailable" {
		t.Fatalf("disabled explicit sync = %+v, %v", syncResult, err)
	}
	if _, err := s.CloudflareURL(ctx, asset.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("disabled URL = %v", err)
	}

	localConfig := fmt.Sprintf(`{"backend":"localgit","path":%q,"branch":"main","cloudflare":{"enabled":true,"endpoint":%q,"bucket":%q,"public_base_url":%q}}`+"\n",
		filepath.Join(t.TempDir(), "local.git"), fakeCFEndpoint, fakeCFBucket, fakeCFPublicURL)
	writeCloudflareConfigTestFile(t, s.Dir, localConfig)
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) {
		t.Error("localgit Cloudflare constructed an R2 client")
		return nil, errors.New("must not be called")
	}
	views, err := s.Add(ctx, []Input{{Reader: bytes.NewReader(batchPNG(t, 51)), Name: "local.png"}}, AddOptions{Namespace: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	localResult := mustCommit(t, ctx, s, r)
	if localResult.Cloudflare != nil {
		t.Fatalf("localgit automatic mirror ran: %+v", localResult.Cloudflare)
	}
	if _, err := s.CloudflareURL(ctx, views[0].Asset.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("localgit URL = %v", err)
	}
	localSync, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if !errors.Is(err, domain.ErrDeliveryUnavailable) || localSync.Status != "unavailable" {
		t.Fatalf("localgit explicit sync = %+v, %v", localSync, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "cloudflare")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("localgit created a mirror ledger")
	}
}

func TestCloudflareTargetSwitchAndPublicBaseChange(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	fake := newFakeMirror()
	first := testCloudflareConfig()
	enableTestCloudflare(t, s, fake, first)
	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(s.Dir, "cloudflare", first.TargetID()+".json")
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}

	second := first
	second.Bucket = "touko-images-2"
	enableTestCloudflare(t, s, fake, second)
	fake.clearBucket()
	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(s.Dir, "cloudflare", second.TargetID()+".json")
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(firstPath)
	if err != nil || !bytes.Equal(unchanged, firstBytes) {
		t.Fatal("target switch rewrote the previous ledger")
	}
	if fake.putCount() != 2 {
		t.Fatalf("target switch reused a receipt across buckets: %d puts", fake.putCount())
	}

	putsBefore := fake.putCount()
	third := second
	third.PublicBaseURL = "https://cdn.example.test/archive/"
	enableTestCloudflare(t, s, fake, third)
	if _, err := s.CloudflareSync(ctx, r, CloudflareOptions{}); err != nil {
		t.Fatal(err)
	}
	if fake.putCount() != putsBefore {
		t.Fatal("changing only public_base_url retransferred objects")
	}
	url, err := s.CloudflareURL(ctx, asset.ID.String())
	if err != nil || url != "https://cdn.example.test/archive/"+repository.ObjectPath(asset) {
		t.Fatalf("new base URL = %q, %v", url, err)
	}
}

func TestCloudflareSyncRejectsPublicationJournal(t *testing.T) {
	ctx, r, s := realRepository(t)
	stageImage(t, ctx, s)
	fake := newFakeMirror()
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	if err := s.Store.SavePublication(ctx, &index.Publication{BaseHead: "", CandidateHead: "candidate", CandidateTree: "candidate-tree"}); err != nil {
		t.Fatal(err)
	}
	result, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if !errors.Is(err, domain.ErrConflict) || result.Status != "unavailable" || result.ErrorCode != "not_published" {
		t.Fatalf("journal conflict = %+v, %v", result, err)
	}
	if fake.putCount() != 0 || len(fake.deleteKeys()) != 0 || fake.headCount() != 0 {
		t.Fatal("journal conflict touched R2")
	}
	st, _ := s.Store.Load(ctx)
	if st.Publication == nil || len(st.Changes) != 1 {
		t.Fatal("Cloudflare sync cleared the publication journal or stage")
	}
	if err := s.Store.SavePublication(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareSyncErrorSummaryAndCancellation(t *testing.T) {
	cause := errors.New("SECRET-CAUSE-TEXT")
	summarized := &CloudflareSyncError{
		Result: CloudflareResult{Status: "pending", ErrorCode: "network", Items: []CloudflareItem{{ObjectPath: "x", Action: "put", Status: "pending", ErrorCode: "network"}}},
		cause:  cause,
	}
	if strings.Contains(summarized.Error(), "SECRET-CAUSE-TEXT") {
		t.Fatalf("summary leaked the cause: %s", summarized.Error())
	}
	if !errors.Is(summarized, cause) {
		t.Fatal("cause unwrapping lost")
	}

	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	mustCommit(t, ctx, s, r)
	fake := newFakeMirror()
	fake.blockPut = true
	enableTestCloudflare(t, s, fake, testCloudflareConfig())
	request, cancel := context.WithCancel(ctx)
	go func() {
		<-fake.putStarted
		cancel()
	}()
	result, err := s.CloudflareSync(request, r, CloudflareOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	var syncErr *CloudflareSyncError
	if !errors.As(err, &syncErr) || result.Status == "synced" || result.ErrorCode != "canceled" {
		t.Fatalf("canceled sync = %+v, %v", result, err)
	}
	entry := ledgerEntry(t, testLedger(t, ctx, s, testCloudflareConfig()), repository.ObjectPath(asset))
	if entry.State == "synced" || entry.State == "deleted" || entry.ErrorCode != "canceled" {
		t.Fatalf("cancellation lost the durable unsynced receipt: %+v", entry)
	}
}

func TestCloudflareNonPublicAndFactoryBoundaries(t *testing.T) {
	ctx, r, s := realRepository(t)
	asset := stageImage(t, ctx, s)
	// A local repository never gets an automatic mirror even with enabled config.
	localConfig := fmt.Sprintf(`{"backend":"localgit","path":%q,"branch":"main","cloudflare":{"enabled":true,"endpoint":%q,"bucket":%q,"public_base_url":%q}}`+"\n",
		filepath.Join(t.TempDir(), "repo.git"), fakeCFEndpoint, fakeCFBucket, fakeCFPublicURL)
	writeCloudflareConfigTestFile(t, s.Dir, localConfig)
	called := false
	s.NewCloudflare = func(context.Context, r2.Config, credential.R2Pair) (r2.Mirror, error) {
		called = true
		return newFakeMirror(), nil
	}
	if result := mustCommit(t, ctx, s, r); result.Cloudflare != nil || called {
		t.Fatalf("local auto mirror ran: %+v called=%v", result.Cloudflare, called)
	}
	if _, err := s.CloudflareURL(ctx, asset.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("local URL = %v", err)
	}

	// Enabled but no factory is an explicit unavailable result, never a fake success.
	writeTestCloudflareConfig(t, s.Dir, testCloudflareConfig(), false)
	s.NewCloudflare = nil
	staged := stageImage(t, ctx, s)
	commit := mustCommit(t, ctx, s, r)
	if commit.Cloudflare == nil || commit.Cloudflare.Status != "unavailable" {
		t.Fatalf("missing factory = %+v", commit.Cloudflare)
	}
	syncResult, err := s.CloudflareSync(ctx, r, CloudflareOptions{})
	if !errors.Is(err, domain.ErrDeliveryUnavailable) || syncResult.Status != "unavailable" {
		t.Fatalf("missing factory explicit sync = %+v, %v", syncResult, err)
	}
	if _, err := s.CloudflareURL(ctx, staged.ID.String()); !errors.Is(err, domain.ErrDeliveryUnavailable) {
		t.Fatalf("missing factory URL = %v", err)
	}
}

func TestCloudflareEmptySyncRequiresCredentials(t *testing.T) {
	ctx, remote, service := realRepository(t)
	writeTestCloudflareConfig(t, service.Dir, testCloudflareConfig(), false)
	service.NewCloudflare = NewCloudflareMirror
	t.Setenv("TOUKO_R2_ACCESS_KEY_ID", "")
	t.Setenv("TOUKO_R2_SECRET_ACCESS_KEY", "")
	for _, dryRun := range []bool{false, true} {
		result, err := service.CloudflareSync(ctx, remote, CloudflareOptions{DryRun: dryRun})
		if !errors.Is(err, domain.ErrDeliveryUnavailable) || result.Status != "unavailable" || result.ErrorCode != "credentials_missing" {
			t.Fatalf("empty mirror claimed readiness without credentials: %+v, %v", result, err)
		}
	}
	if _, err := os.Stat(filepath.Join(service.Dir, "cloudflare")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unavailable empty sync wrote a ledger", err)
	}
}
