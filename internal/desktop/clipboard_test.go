package desktop

import (
	"bytes"
	"context"
	stdimage "image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testOrigin = "http://127.0.0.1:43210"

func newTestBridge(t *testing.T) *clipboardBridge {
	t.Helper()
	bridge, err := newClipboardBridge(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.close)
	return bridge
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 4, 3))
	img.Set(1, 2, color.RGBA{G: 180, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func screenshotSources(t *testing.T, paths ...string) []clipboardSource {
	t.Helper()
	sources, err := fileSources(paths)
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func get(t *testing.T, bridge *clipboardBridge, target, origin, method string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	recorder := httptest.NewRecorder()
	bridge.serveHTTP(recorder, request)
	return recorder
}

// capabilityPath is the request path the page fetches; capabilityURL is the
// full URL handed to the page, which carries the scheme the platform supports.
func capabilityPath(token string, index int) string {
	return "/" + token + "/" + strconv.Itoa(index)
}

func capabilityTarget(token string, index int) string {
	parsed, err := url.Parse(capabilityURL(token, index))
	if err != nil {
		panic(err)
	}
	return parsed.RequestURI()
}

func TestClipboardSnapshotWithoutExtensionServesOriginalBytes(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	raw := pngBytes(t)
	path := writeFile(t, dir, "截图", raw)

	batch, err := bridge.snapshot(context.Background(), screenshotSources(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.files) != 1 {
		t.Fatalf("files = %d, want 1", len(batch.files))
	}
	file := batch.files[0]
	if file.Name != "截图" {
		t.Errorf("name = %q, want the source basename", file.Name)
	}
	if file.Type != "image/png" {
		t.Errorf("type = %q, want image/png", file.Type)
	}
	if file.Size != int64(len(raw)) {
		t.Errorf("size = %d, want %d", file.Size, len(raw))
	}
	if file.URL != capabilityURL(batch.token, 0) {
		t.Errorf("url = %q, want %q", file.URL, capabilityURL(batch.token, 0))
	}
	if bridge.install(batch) == false {
		t.Fatal("install refused a live bridge")
	}

	recorder := get(t, bridge, capabilityTarget(batch.token, 0), testOrigin, http.MethodGet)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.Bytes(); !bytes.Equal(got, raw) {
		t.Errorf("body differs from the source image bytes")
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}

	head := get(t, bridge, capabilityTarget(batch.token, 0), testOrigin, http.MethodHead)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD status = %d, body = %d bytes", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(raw)) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(raw))
	}
}

func TestClipboardBitmapSourceUsesCapturedPNGBytes(t *testing.T) {
	bridge := newTestBridge(t)
	raw := pngBytes(t)
	batch, err := bridge.snapshot(context.Background(), []clipboardSource{bitmapSource(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if batch.files[0].Name != "clipboard.png" {
		t.Errorf("name = %q, want clipboard.png", batch.files[0].Name)
	}
	if !bridge.install(batch) {
		t.Fatal("install failed")
	}
	recorder := get(t, bridge, capabilityTarget(batch.token, 0), testOrigin, http.MethodGet)
	if !bytes.Equal(recorder.Body.Bytes(), raw) {
		t.Errorf("served bitmap differs from the captured PNG")
	}
}

func TestClipboardBatchRejectsNonImageAndKeepsSources(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	imagePath := writeFile(t, dir, "shot.png", pngBytes(t))
	textPath := writeFile(t, dir, "notes.txt", []byte("not an image"))
	beforeImage, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeText, err := os.ReadFile(textPath)
	if err != nil {
		t.Fatal(err)
	}
	before := clipboardTempDirs(t)

	if _, err := bridge.snapshot(context.Background(), screenshotSources(t, imagePath, textPath)); err == nil {
		t.Fatal("snapshot accepted a non-image file")
	}
	bridge.mu.Lock()
	installed := bridge.batch != nil
	bridge.mu.Unlock()
	if installed {
		t.Error("a failed batch was installed")
	}
	if after := clipboardTempDirs(t); after != before {
		t.Errorf("temporary directories = %d, want %d (whole batch must be cleaned)", after, before)
	}
	afterImage, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	afterText, err := os.ReadFile(textPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeImage, afterImage) || !bytes.Equal(beforeText, afterText) {
		t.Error("clipboard sources were modified")
	}
	if info, err := os.Stat(textPath); err != nil || !info.Mode().IsRegular() {
		t.Errorf("source file is no longer a regular file: %v", err)
	}
}

func TestClipboardBatchRejectsOversizedInput(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	sparse := filepath.Join(dir, "huge.png")
	file, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(100_000_001); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before := clipboardTempDirs(t)
	if _, err := bridge.snapshot(context.Background(), screenshotSources(t, sparse)); err == nil {
		t.Fatal("snapshot accepted a file above the per-image limit")
	}
	if after := clipboardTempDirs(t); after != before {
		t.Errorf("temporary directories = %d, want %d", after, before)
	}

	// Six sparse files stay under the per-image limit but exceed the batch one.
	var many []string
	for n := range 6 {
		path := filepath.Join(dir, "part-"+string(rune('a'+n))+".png")
		part, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := part.Truncate(90_000_000); err != nil {
			t.Fatal(err)
		}
		if err := part.Close(); err != nil {
			t.Fatal(err)
		}
		many = append(many, path)
	}
	if _, err := bridge.snapshot(context.Background(), screenshotSources(t, many...)); err == nil {
		t.Fatal("snapshot accepted a batch above the total limit")
	}
}

func TestClipboardBatchRejectsEmptyAndTooManyFiles(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	empty := writeFile(t, dir, "empty.png", nil)
	if _, err := bridge.snapshot(context.Background(), screenshotSources(t, empty)); err == nil {
		t.Fatal("snapshot accepted an empty file")
	}

	var paths []string
	raw := pngBytes(t)
	for n := range maxClipboardFiles + 1 {
		paths = append(paths, writeFile(t, dir, "f"+strconv.Itoa(n)+".png", raw))
	}
	if _, err := bridge.snapshot(context.Background(), screenshotSources(t, paths...)); err == nil {
		t.Fatal("snapshot accepted more than the file limit")
	}
}

func TestClipboardCapabilityBoundaries(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "shot.png", pngBytes(t))
	batch, err := bridge.snapshot(context.Background(), screenshotSources(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.install(batch) {
		t.Fatal("install failed")
	}

	cases := []struct {
		name   string
		target string
		origin string
		method string
		want   int
	}{
		{"wrong origin", capabilityPath(batch.token, 0), "http://127.0.0.1:1", http.MethodGet, http.StatusForbidden},
		{"missing origin", capabilityPath(batch.token, 0), "", http.MethodGet, http.StatusForbidden},
		{"unknown token", capabilityPath(strings.Repeat("0", clipboardTokenBytes*2), 0), testOrigin, http.MethodGet, http.StatusNotFound},
		{"non-hex token", capabilityPath(strings.Repeat("z", clipboardTokenBytes*2), 0), testOrigin, http.MethodGet, http.StatusNotFound},
		{"short token", capabilityPath("abcd", 0), testOrigin, http.MethodGet, http.StatusNotFound},
		{"out of range", capabilityPath(batch.token, 1), testOrigin, http.MethodGet, http.StatusNotFound},
		{"negative index", capabilityPath(batch.token, -1), testOrigin, http.MethodGet, http.StatusNotFound},
		{"plus index", "/" + batch.token + "/+0", testOrigin, http.MethodGet, http.StatusNotFound},
		{"non-decimal index", "/" + batch.token + "/0x0", testOrigin, http.MethodGet, http.StatusNotFound},
		{"path query", capabilityPath(batch.token, 0) + "?path=/etc/passwd", testOrigin, http.MethodGet, http.StatusOK},
		{"token only", "/" + batch.token, testOrigin, http.MethodGet, http.StatusNotFound},
		{"trailing segment", capabilityPath(batch.token, 0) + "/extra", testOrigin, http.MethodGet, http.StatusNotFound},
		{"post", capabilityPath(batch.token, 0), testOrigin, http.MethodPost, http.StatusMethodNotAllowed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := get(t, bridge, testCase.target, testCase.origin, testCase.method)
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d, want %d", recorder.Code, testCase.want)
			}
		})
	}

	// A query string must not select anything; only the ordinal in the path
	// identifies the snapshot entry.
	if got := get(t, bridge, capabilityTarget(batch.token, 0)+"?index=1", testOrigin, http.MethodGet); got.Code != http.StatusOK {
		t.Errorf("query string changed the result: %d", got.Code)
	}
}

func TestClipboardReleaseRemovesBatch(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "shot.png", pngBytes(t))
	batch, err := bridge.snapshot(context.Background(), screenshotSources(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.install(batch) {
		t.Fatal("install failed")
	}
	if _, err := os.Stat(batch.dir); err != nil {
		t.Fatalf("snapshot directory is missing before release: %v", err)
	}
	bridge.release(batch.token)
	if _, err := os.Stat(batch.dir); !os.IsNotExist(err) {
		t.Errorf("snapshot directory survived release: %v", err)
	}
	if recorder := get(t, bridge, capabilityTarget(batch.token, 0), testOrigin, http.MethodGet); recorder.Code != http.StatusNotFound {
		t.Errorf("released token status = %d, want 404", recorder.Code)
	}
	// Release is idempotent.
	bridge.release(batch.token)
	bridge.release("unknown")
}

func TestClipboardReleaseWaitsForActiveReaders(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "shot.png", pngBytes(t))
	batch, err := bridge.snapshot(context.Background(), screenshotSources(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.install(batch) {
		t.Fatal("install failed")
	}
	state, ok := bridge.acquire(batch.token, 0)
	if !ok {
		t.Fatal("a live token could not be acquired")
	}
	bridge.release(batch.token)
	if _, err := os.Stat(batch.dir); err != nil {
		t.Fatalf("directory removed while a reader held a lease: %v", err)
	}
	if recorder := get(t, bridge, capabilityTarget(batch.token, 0), testOrigin, http.MethodGet); recorder.Code != http.StatusNotFound {
		t.Errorf("new capability request after release = %d, want 404", recorder.Code)
	}
	bridge.releaseLease(state)
	if _, err := os.Stat(batch.dir); !os.IsNotExist(err) {
		t.Errorf("directory survived the last reader: %v", err)
	}
}

func TestClipboardExpiryDropsTokenAndDirectory(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "shot.png", pngBytes(t))
	batch, err := bridge.snapshot(context.Background(), screenshotSources(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.install(batch) {
		t.Fatal("install failed")
	}
	if batch.timer == nil {
		t.Fatal("install did not arm the expiry timer")
	}
	// The timer and Close share one code path; run it without waiting the
	// full two minutes.
	batch.timer.Stop()
	bridge.discard(batch)
	if _, err := os.Stat(batch.dir); !os.IsNotExist(err) {
		t.Errorf("expired batch kept its directory: %v", err)
	}
	if recorder := get(t, bridge, capabilityTarget(batch.token, 0), testOrigin, http.MethodGet); recorder.Code != http.StatusNotFound {
		t.Errorf("expired token status = %d, want 404", recorder.Code)
	}
}

func TestClipboardNewBatchReplacesOldOne(t *testing.T) {
	bridge := newTestBridge(t)
	dir := t.TempDir()
	raw := pngBytes(t)
	first, err := bridge.snapshot(context.Background(), screenshotSources(t, writeFile(t, dir, "a.png", raw)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := bridge.snapshot(context.Background(), screenshotSources(t, writeFile(t, dir, "b.png", append(raw, '\n'))))
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.install(first) || !bridge.install(second) {
		t.Fatal("install failed")
	}
	if _, err := os.Stat(first.dir); !os.IsNotExist(err) {
		t.Errorf("replaced batch kept its directory: %v", err)
	}
	if recorder := get(t, bridge, capabilityTarget(first.token, 0), testOrigin, http.MethodGet); recorder.Code != http.StatusNotFound {
		t.Errorf("replaced token status = %d, want 404", recorder.Code)
	}
	if recorder := get(t, bridge, capabilityTarget(second.token, 0), testOrigin, http.MethodGet); recorder.Code != http.StatusOK {
		t.Errorf("current token status = %d, want 200", recorder.Code)
	}
}

func TestClipboardCloseStopsServing(t *testing.T) {
	bridge := newTestBridge(t)
	batch, err := bridge.snapshot(context.Background(), []clipboardSource{bitmapSource(pngBytes(t))})
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.install(batch) {
		t.Fatal("install failed")
	}
	bridge.close()
	if _, err := os.Stat(batch.dir); !os.IsNotExist(err) {
		t.Errorf("closed bridge kept its directory: %v", err)
	}
	late, err := bridge.snapshot(context.Background(), []clipboardSource{bitmapSource(pngBytes(t))})
	if err != nil {
		t.Fatal(err)
	}
	if bridge.install(late) {
		t.Error("a closed bridge accepted a new batch")
	}
	if _, err := os.Stat(late.dir); !os.IsNotExist(err) {
		t.Errorf("batch installed into a closed bridge kept its directory: %v", err)
	}
}

func TestClipboardErrorsNameOnlyTheBasename(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, "inner")
	if err := os.Mkdir(hidden, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(hidden, "gone.png")
	_, err := fileSources([]string{missing})
	if err == nil {
		t.Fatal("missing file was accepted")
	}
	if !strings.Contains(err.Error(), "gone.png") {
		t.Errorf("error does not name the file: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("error leaks an absolute path: %v", err)
	}
}

func clipboardTempDirs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), clipboardTempPrefix) {
			count++
		}
	}
	return count
}

// TestClipboardBatchTimerIsBounded guards the shipped expiry window without
// waiting for it.
func TestClipboardBatchTimerIsBounded(t *testing.T) {
	if clipboardBatchTTL <= 0 || clipboardBatchTTL > 2*time.Minute {
		t.Errorf("clipboardBatchTTL = %v, want a positive window of at most two minutes", clipboardBatchTTL)
	}
}
