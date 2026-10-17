package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
)

// The desktop clipboard bridge turns a deliberate paste into the same staged
// uploads as the file picker: one bounded, private snapshot per paste, served
// back to the app's own page through an unguessable capability URL. Pages get
// no file-system access, only opaque tokens; the bridge never reads text, HTML
// or remote URLs from the clipboard, and it never moves or deletes the files
// the clipboard references.
const (
	clipboardScheme     = "touko-clipboard"
	clipboardTokenBytes = 32
	// clipboardBatchTTL bounds how long a snapshot outlives its paste even if
	// the page never calls Release.
	clipboardBatchTTL = 2 * time.Minute
	// clipboardTempPrefix is unique to this bridge so cleanup can only ever
	// remove directories it created.
	clipboardTempPrefix = "touko-clipboard-"

	maxClipboardFiles = 100
	maxClipboardBatch = 500_000_000
)

var (
	errNoClipboardImage = errors.New("the clipboard has no image or local file")
	errBatchTooLarge    = fmt.Errorf("the clipboard images exceed the %d byte batch limit", int64(maxClipboardBatch))
)

// clipboardFile is one snapshot entry as the page sees it.
type clipboardFile struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// clipboardBatch is the result of DesktopClipboard.Read.
type clipboardBatch struct {
	Token string          `json:"token"`
	Files []clipboardFile `json:"files"`
}

// clipboardBridge is bound as DesktopClipboard. Only Read, Release and
// WriteText may be exported: MyGo binds every exported method, and handlers
// that take an http.ResponseWriter would panic at startup.
type clipboardBridge struct {
	origin string

	// mu guards the fields below. It is never held across a MyGo main-thread
	// call or while bytes stream to a page.
	mu     sync.Mutex
	window *mygo.Window
	page   *mygo.Page
	batch  *clipboardBatchState
	closed bool
}

// clipboardBatchState is one immutable snapshot. refs counts live capability
// requests; a released batch removes its directory only once the last reader
// has finished. refs, released and timer are guarded by clipboardBridge.mu.
type clipboardBatchState struct {
	token string
	dir   string
	files []clipboardFile
	paths []string

	timer    *time.Timer
	refs     int
	released bool
	once     sync.Once
}

func (s *clipboardBatchState) cleanup() {
	s.once.Do(func() { os.RemoveAll(s.dir) })
}

// newClipboardBridge registers the capability protocol. It must run before the
// window that serves the pasted bytes is created.
func newClipboardBridge(origin string) (*clipboardBridge, error) {
	b := &clipboardBridge{origin: origin}
	if err := mygo.Protocol.HandleFunc(clipboardScheme, b.serveHTTP); err != nil {
		return nil, fmt.Errorf("register clipboard protocol: %w", err)
	}
	return b, nil
}

// attach makes window the only page allowed to read and release batches.
func (b *clipboardBridge) attach(window *mygo.Window, page *mygo.Page) error {
	if window == nil || page == nil {
		return errors.New("desktop clipboard needs a window with a page")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("desktop clipboard bridge is closed")
	}
	b.window, b.page = window, page
	// A navigation ends the paste it was serving: the next document must ask
	// again instead of inheriting capability URLs it never requested.
	page.OnDidNavigate(func(string) { b.discardCurrent() })
	return nil
}

// close releases the current batch and stops serving capability URLs. Bytes
// already in flight finish before their directory is removed.
func (b *clipboardBridge) close() {
	b.mu.Lock()
	state := b.batch
	b.batch = nil
	b.closed = true
	b.window, b.page = nil, nil
	b.mu.Unlock()
	mygo.Protocol.Unhandle(clipboardScheme)
	b.discard(state)
}

// Read snapshots the clipboard's local files, or else its single PNG
// representation. Only the app's own focused page with the upload dialog open
// may call it.
func (b *clipboardBridge) Read(ctx context.Context) (clipboardBatch, error) {
	if err := b.requireCaller(ctx, true); err != nil {
		return clipboardBatch{}, err
	}
	sources, err := clipboardSources()
	if err != nil {
		return clipboardBatch{}, err
	}
	state, err := b.snapshot(ctx, sources)
	if err != nil {
		return clipboardBatch{}, err
	}
	if !b.install(state) {
		return clipboardBatch{}, errors.New("desktop clipboard is unavailable")
	}
	return clipboardBatch{Token: state.token, Files: append([]clipboardFile(nil), state.files...)}, nil
}

// Release forgets a batch. It is idempotent: unknown, expired and already
// released tokens are not errors.
func (b *clipboardBridge) Release(ctx context.Context, token string) error {
	if err := b.requireCaller(ctx, false); err != nil {
		return err
	}
	b.release(token)
	return nil
}

// WriteText replaces the system clipboard with plain text, for the delivery
// copy action. The error is the native write error.
func (b *clipboardBridge) WriteText(ctx context.Context, text string) error {
	if err := b.requireCaller(ctx, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mygo.Clipboard.Write(transfer.TextData(text))
}

// requireCaller binds every bound method to the attached page and its exact
// loopback origin. Read additionally requires the paste gesture context.
func (b *clipboardBridge) requireCaller(ctx context.Context, pasting bool) error {
	b.mu.Lock()
	window, page, closed := b.window, b.page, b.closed
	b.mu.Unlock()
	if closed || window == nil || page == nil {
		return errors.New("desktop clipboard is unavailable")
	}
	if mygo.CallerWindow(ctx) != window || mygo.CallerPage(ctx) != page {
		return errors.New("clipboard access is only available to TOUKO")
	}
	if originOnly(page.URL()) != b.origin {
		return errors.New("clipboard access is only available to TOUKO")
	}
	if !pasting {
		return nil
	}
	if !window.IsFocused() {
		return errors.New("bring TOUKO to the front before pasting from the clipboard")
	}
	evalCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	open, err := page.EvalContext(evalCtx, "document.getElementById('upload-dialog')?.open === true")
	if err != nil {
		return fmt.Errorf("check the upload dialog: %w", err)
	}
	if isOpen, _ := open.(bool); !isOpen {
		return errors.New("open the upload dialog before pasting from the clipboard")
	}
	return nil
}

func originOnly(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// clipboardSources discovers one representation of the clipboard. A local file
// list wins outright; otherwise a single PNG is read. Text, HTML and remote
// URLs are never requested.
func clipboardSources() ([]clipboardSource, error) {
	formats := mygo.Clipboard.Formats()
	offered := func(want transfer.Format) bool {
		for _, format := range formats {
			if format == want {
				return true
			}
		}
		return false
	}
	if offered(transfer.FileList) || offered(transfer.URIList) {
		paths, err := mygo.Clipboard.ReadFiles()
		switch {
		case err == nil && len(paths) > 0:
			return fileSources(paths)
		case err != nil && !errors.Is(err, transfer.ErrFormat):
			// Ownership changed mid-read or the provider failed: never mix
			// bytes from two owners by retrying against a new one.
			return nil, fmt.Errorf("read clipboard files: %w", err)
		}
		// No local files (an empty list, or only remote URLs): fall through to
		// one PNG representation of the same clipboard.
	}
	if !offered(transfer.PNG) {
		return nil, errNoClipboardImage
	}
	data, err := mygo.Clipboard.Read(transfer.PNG)
	if err != nil {
		if errors.Is(err, transfer.ErrFormat) {
			return nil, errNoClipboardImage
		}
		return nil, fmt.Errorf("read clipboard image: %w", err)
	}
	raw, err := data.Read(transfer.PNG)
	if err != nil {
		if errors.Is(err, transfer.ErrFormat) {
			return nil, errNoClipboardImage
		}
		return nil, fmt.Errorf("read clipboard image: %w", err)
	}
	if len(raw) == 0 {
		return nil, errNoClipboardImage
	}
	return []clipboardSource{bitmapSource(raw)}, nil
}

// clipboardSource is one candidate image, either a local file or clipboard
// bitmap bytes. size is -1 when the size is not known up front.
type clipboardSource struct {
	name string
	size int64
	open func() (io.ReadCloser, error)
}

// fileSources validates the clipboard's file list without following symlinks
// and without opening anything yet.
func fileSources(paths []string) ([]clipboardSource, error) {
	sources := make([]clipboardSource, 0, len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fileError(name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: not a regular file", name)
		}
		sources = append(sources, clipboardSource{
			name: name,
			size: info.Size(),
			open: func() (io.ReadCloser, error) { return os.Open(path) },
		})
	}
	return sources, nil
}

// bitmapSource offers clipboard PNG bytes without copying them.
func bitmapSource(data []byte) clipboardSource {
	return clipboardSource{
		name: "clipboard.png",
		size: int64(len(data)),
		open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

// fileError keeps only the basename and the reason: clipboard contents and
// absolute paths must not reach the page or the logs.
func fileError(name string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %w", name, pathErr.Err)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// snapshot streams every source once into a private, immutable directory. Any
// failure discards the whole batch, so the page never gets a partial token.
func (b *clipboardBridge) snapshot(ctx context.Context, sources []clipboardSource) (*clipboardBatchState, error) {
	if len(sources) == 0 {
		return nil, errNoClipboardImage
	}
	if len(sources) > maxClipboardFiles {
		return nil, fmt.Errorf("the clipboard has %d files; at most %d can be pasted at once", len(sources), maxClipboardFiles)
	}
	// Reject an oversized batch before copying anything.
	var declared int64
	for _, src := range sources {
		if src.size < 0 {
			continue
		}
		if src.size == 0 {
			return nil, fmt.Errorf("%s: image is empty", src.name)
		}
		if src.size > image.MaxBytes {
			return nil, fmt.Errorf("%s: image exceeds the %d byte limit", src.name, int64(image.MaxBytes))
		}
		if declared += src.size; declared > maxClipboardBatch {
			return nil, errBatchTooLarge
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", clipboardTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("create clipboard staging: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("secure clipboard staging: %w", err)
	}
	files := make([]clipboardFile, 0, len(sources))
	paths := make([]string, 0, len(sources))
	var copied int64
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reader, err := src.open()
		if err != nil {
			return nil, fileError(src.name, err)
		}
		result, ingestErr := image.Ingest(ctx, reader, dir)
		closeErr := reader.Close()
		if ingestErr != nil {
			return nil, fileError(src.name, ingestErr)
		}
		if closeErr != nil {
			return nil, fileError(src.name, closeErr)
		}
		copied += result.Object.Size
		if copied > maxClipboardBatch {
			return nil, errBatchTooLarge
		}
		files = append(files, clipboardFile{
			Name: src.name,
			Type: result.Object.MIME,
			Size: result.Object.Size,
		})
		paths = append(paths, result.TempPath)
	}
	token, err := newClipboardToken()
	if err != nil {
		return nil, fmt.Errorf("create clipboard token: %w", err)
	}
	for index := range files {
		files[index].URL = capabilityURL(token, index)
	}
	keep = true
	return &clipboardBatchState{token: token, dir: dir, files: files, paths: paths}, nil
}

func newClipboardToken() (string, error) {
	var raw [clipboardTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// capabilityURL is the one address a page may fetch a snapshot from. WebView2
// serves custom schemes under an http host, so Windows needs that form.
func capabilityURL(token string, index int) string {
	base := clipboardScheme + "://localhost/"
	if runtime.GOOS == "windows" {
		base = "http://" + clipboardScheme + ".localhost/"
	}
	return base + token + "/" + strconv.Itoa(index)
}

// install replaces the live batch. The previous snapshot is invalidated
// immediately; its directory waits for readers that are already streaming.
func (b *clipboardBridge) install(state *clipboardBatchState) bool {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		state.cleanup()
		return false
	}
	old := b.batch
	b.batch = state
	state.timer = time.AfterFunc(clipboardBatchTTL, func() { b.discard(state) })
	b.mu.Unlock()
	b.discard(old)
	return true
}

func (b *clipboardBridge) discardCurrent() {
	b.mu.Lock()
	state := b.batch
	b.mu.Unlock()
	b.discard(state)
}

// discard invalidates a batch. File removal happens outside the lock and only
// once no capability request still holds a lease.
func (b *clipboardBridge) discard(state *clipboardBatchState) {
	if state == nil {
		return
	}
	b.mu.Lock()
	if b.batch == state {
		b.batch = nil
	}
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	state.released = true
	cleanup := state.refs == 0
	b.mu.Unlock()
	if cleanup {
		state.cleanup()
	}
}

// release is Release's body: it drops token if it is still the live batch.
func (b *clipboardBridge) release(token string) {
	b.mu.Lock()
	state := b.batch
	stale := state == nil || state.released
	b.mu.Unlock()
	if stale {
		return
	}
	if subtle.ConstantTimeCompare([]byte(state.token), []byte(token)) != 1 {
		return
	}
	b.discard(state)
}

func (b *clipboardBridge) acquire(token string, index int) (*clipboardBatchState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.batch
	if state == nil || state.released {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(state.token), []byte(token)) != 1 {
		return nil, false
	}
	if index < 0 || index >= len(state.paths) {
		return nil, false
	}
	state.refs++
	return state, true
}

func (b *clipboardBridge) releaseLease(state *clipboardBatchState) {
	b.mu.Lock()
	state.refs--
	cleanup := state.released && state.refs == 0
	b.mu.Unlock()
	if cleanup {
		state.cleanup()
	}
}

// serveHTTP answers capability requests. Only the app's exact loopback origin
// may fetch bytes; tokens are opaque and single-batch.
func (b *clipboardBridge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != b.origin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", b.origin)
	header.Add("Vary", "Origin")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		header.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, index, ok := parseCapability(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	state, ok := b.acquire(token, index)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer b.releaseLease(state)
	file, err := os.Open(state.paths[index])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	entry := state.files[index]
	header = w.Header()
	header.Set("Content-Type", entry.Type)
	header.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	buffer := make([]byte, 64<<10)
	for {
		if err := r.Context().Err(); err != nil {
			return
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			return
		}
	}
}

func parseCapability(path string) (string, int, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 {
		return "", 0, false
	}
	token, raw := parts[0], parts[1]
	if len(token) != clipboardTokenBytes*2 {
		return "", 0, false
	}
	if _, err := hex.DecodeString(token); err != nil {
		return "", 0, false
	}
	if raw == "" || strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
		return "", 0, false
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 {
		return "", 0, false
	}
	return token, index, true
}
