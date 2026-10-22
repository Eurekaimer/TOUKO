package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
)

const (
	testAccessKey = "AKIAEXAMPLEACCESSKEY"
	testSecretKey = "sentinel-secret-key-never-log"
	testBucket    = "touko-images"
)

func testConfig(endpoint string) Config {
	return Config{
		Enabled:       true,
		Endpoint:      endpoint,
		Bucket:        testBucket,
		PublicBaseURL: "https://img.example.test",
	}
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client, err := NewWithClient(testConfig(srv.URL), testAccessKey, testSecretKey, srv.Client())
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	return client
}

func testObject() (domain.ContentObject, domain.RepositoryID, string, []byte) {
	payload := []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x00\x01\x02\x00\xff")
	hash := sha256.Sum256(payload)
	object := domain.ContentObject{
		Hash:   domain.ObjectHash(hash),
		MIME:   "image/webp",
		Ext:    "webp",
		Size:   int64(len(payload)),
		Width:  64,
		Height: 48,
	}
	repositoryID := domain.RepositoryID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0xdc, 0xfe}
	sourceID := strings.Repeat("3f", 32)
	return object, repositoryID, sourceID, payload
}

func isBucketPath(path string) bool {
	return strings.TrimSuffix(path, "/") == "/"+testBucket
}

// TestPutObjectSendsBytesMetadataAndVerifiesHead proves the SDK adapter performs
// a path-style conditional PUT with the exact bytes and ownership metadata, then
// re-reads the object before reporting success.
func TestPutObjectSendsBytesMetadataAndVerifiesHead(t *testing.T) {
	object, repositoryID, sourceID, payload := testObject()
	const etag = "etag-one"

	var mu sync.Mutex
	var putCount, headCount int
	var putBody []byte
	var putPath string
	var putQuery string
	var putLength int64
	var putHeader http.Header

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			putCount++
			putPath = r.URL.Path
			putQuery = r.URL.RawQuery
			putLength = r.ContentLength
			putHeader = r.Header.Clone()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read put body: %v", err)
			}
			putBody = body
			w.Header().Set("ETag", strconv.Quote(etag))
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			headCount++
			w.Header().Set("ETag", strconv.Quote(etag))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Header().Set("Content-Type", object.MIME)
			w.Header().Set("X-Amz-Meta-Touko-Repository-Id", repositoryID.String())
			w.Header().Set("X-Amz-Meta-Touko-Source-Id", sourceID)
			w.Header().Set("X-Amz-Meta-Touko-Sha256", object.Hash.String())
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	key := "images/a b/图.webp"
	gotETag, err := client.PutObject(context.Background(), key, bytes.NewReader(payload), PutOptions{
		Object:       object,
		RepositoryID: repositoryID,
		SourceID:     sourceID,
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if gotETag != etag {
		t.Fatalf("PutObject etag = %q, want %q", gotETag, etag)
	}

	mu.Lock()
	defer mu.Unlock()
	if putCount != 1 {
		t.Fatalf("PUT requests = %d, want 1", putCount)
	}
	if headCount != 1 {
		t.Fatalf("verification HEAD requests = %d, want 1", headCount)
	}
	if putPath != "/"+testBucket+"/"+key {
		t.Fatalf("PUT path = %q, want path-style %q", putPath, "/"+testBucket+"/"+key)
	}
	if putQuery != "x-id=PutObject" {
		t.Fatalf("PUT query = %q, want SDK operation marker x-id=PutObject", putQuery)
	}
	if putLength != int64(len(payload)) {
		t.Fatalf("PUT content length = %d, want %d", putLength, len(payload))
	}
	if !bytes.Equal(putBody, payload) {
		t.Fatalf("PUT body mismatch: got %x want %x", putBody, payload)
	}
	if got := putHeader.Get("If-None-Match"); got != "*" {
		t.Fatalf("If-None-Match = %q, want *", got)
	}
	if got := putHeader.Get("If-Match"); got != "" {
		t.Fatalf("If-Match = %q, want empty", got)
	}
	if got := putHeader.Get("Content-Type"); got != object.MIME {
		t.Fatalf("Content-Type = %q, want %q", got, object.MIME)
	}
	metadata := map[string]string{
		"X-Amz-Meta-Touko-Repository-Id": repositoryID.String(),
		"X-Amz-Meta-Touko-Source-Id":     sourceID,
		"X-Amz-Meta-Touko-Sha256":        object.Hash.String(),
	}
	for name, want := range metadata {
		if got := putHeader.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	authorization := putHeader.Get("Authorization")
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential="+testAccessKey+"/") {
		t.Fatalf("Authorization = %q, want SigV4 with access key id", authorization)
	}
	if !strings.Contains(authorization, "/auto/s3/aws4_request") {
		t.Fatalf("Authorization = %q, want region auto scope", authorization)
	}
	if strings.Contains(authorization, testSecretKey) {
		t.Fatal("Authorization leaked the secret access key")
	}
	for name, values := range putHeader {
		for _, value := range values {
			if strings.Contains(value, testSecretKey) {
				t.Fatalf("header %s leaked the secret access key", name)
			}
		}
	}
}

func TestPutObjectConditionalUpdateUsesIfMatch(t *testing.T) {
	object, repositoryID, sourceID, payload := testObject()
	const etag = "etag-next"

	var mu sync.Mutex
	var ifMatch, ifNoneMatch string
	putCount := 0

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			putCount++
			_, _ = io.Copy(io.Discard, r.Body)
			ifMatch = r.Header.Get("If-Match")
			ifNoneMatch = r.Header.Get("If-None-Match")
			w.Header().Set("ETag", strconv.Quote(etag))
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			w.Header().Set("ETag", strconv.Quote(etag))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Header().Set("X-Amz-Meta-Touko-Repository-Id", repositoryID.String())
			w.Header().Set("X-Amz-Meta-Touko-Source-Id", sourceID)
			w.Header().Set("X-Amz-Meta-Touko-Sha256", object.Hash.String())
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	got, err := client.PutObject(context.Background(), "images/owned.webp", bytes.NewReader(payload), PutOptions{
		Object:       object,
		RepositoryID: repositoryID,
		SourceID:     sourceID,
		IfMatch:      "etag-previous",
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got != etag {
		t.Fatalf("etag = %q, want %q", got, etag)
	}
	mu.Lock()
	defer mu.Unlock()
	if putCount != 1 {
		t.Fatalf("PUT requests = %d, want 1", putCount)
	}
	if want := strconv.Quote("etag-previous"); ifMatch != want {
		t.Fatalf("If-Match = %q, want %q", ifMatch, want)
	}
	if ifNoneMatch != "" {
		t.Fatalf("If-None-Match = %q, want empty for owned update", ifNoneMatch)
	}
}

func TestPutObjectPreconditionFailureIsClassifiedWithoutRetry(t *testing.T) {
	object, repositoryID, sourceID, payload := testObject()
	var puts int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&puts, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>PreconditionFailed</Code><Message>sentinel-body-secret</Message></Error>`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.PutObject(context.Background(), "images/race.webp", bytes.NewReader(payload), PutOptions{
		Object:       object,
		RepositoryID: repositoryID,
		SourceID:     sourceID,
		IfMatch:      "etag-stale",
	})
	if err == nil {
		t.Fatal("PutObject succeeded on HTTP 412")
	}
	if code := ErrorCode(err); code != "precondition_failed" {
		t.Fatalf("ErrorCode = %q, want precondition_failed", code)
	}
	var r2err *Error
	if !errors.As(err, &r2err) {
		t.Fatalf("error %T is not *Error", err)
	}
	if r2err.HTTPStatus != http.StatusPreconditionFailed {
		t.Fatalf("HTTPStatus = %d, want 412", r2err.HTTPStatus)
	}
	if strings.Contains(err.Error(), "sentinel-body-secret") {
		t.Fatalf("error leaked response body: %v", err)
	}
	if got := atomic.LoadInt32(&puts); got != 1 {
		t.Fatalf("PUT attempts = %d, want exactly 1 (no retries)", got)
	}
}

func TestHeadObjectReturnsVerifiedMetadata(t *testing.T) {
	object, repositoryID, sourceID, payload := testObject()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !isBucketPath(r.URL.Path) {
			w.Header().Set("ETag", strconv.Quote("etag-head"))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Header().Set("Content-Type", object.MIME)
			w.Header().Set("X-Amz-Meta-Touko-Repository-Id", repositoryID.String())
			w.Header().Set("X-Amz-Meta-Touko-Source-Id", sourceID)
			w.Header().Set("X-Amz-Meta-Touko-Sha256", object.Hash.String())
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	info, exists, err := client.HeadObject(context.Background(), "images/图 one.webp")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if !exists {
		t.Fatal("HeadObject exists = false, want true")
	}
	if info.ETag != "etag-head" {
		t.Fatalf("ETag = %q, want unquoted etag-head", info.ETag)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("Size = %d, want %d", info.Size, len(payload))
	}
	if info.RepositoryID != repositoryID.String() {
		t.Fatalf("RepositoryID = %q, want %q", info.RepositoryID, repositoryID.String())
	}
	if info.SourceID != sourceID {
		t.Fatalf("SourceID = %q, want %q", info.SourceID, sourceID)
	}
	if info.SHA256 != object.Hash.String() {
		t.Fatalf("SHA256 = %q, want %q", info.SHA256, object.Hash.String())
	}
}

func TestHeadObjectMissingDisambiguation(t *testing.T) {
	tests := []struct {
		name          string
		objectStatus  int
		bucketStatus  int
		wantExists    bool
		wantCode      string
		wantHTTP      int
		wantProbes    int
		objectHasBody string
	}{
		{name: "present", objectStatus: http.StatusOK, wantExists: true},
		{name: "object missing bucket present", objectStatus: http.StatusNotFound, bucketStatus: http.StatusOK, wantProbes: 1},
		{name: "object and bucket missing", objectStatus: http.StatusNotFound, bucketStatus: http.StatusNotFound, wantCode: "bucket_unavailable", wantHTTP: http.StatusNotFound, wantProbes: 1},
		{name: "object missing bucket forbidden", objectStatus: http.StatusNotFound, bucketStatus: http.StatusForbidden, wantCode: "access_denied", wantHTTP: http.StatusForbidden, wantProbes: 1},
		{name: "object missing bucket error", objectStatus: http.StatusNotFound, bucketStatus: http.StatusInternalServerError, wantCode: "network", wantHTTP: http.StatusInternalServerError, wantProbes: 1},
		{name: "object forbidden", objectStatus: http.StatusForbidden, wantCode: "access_denied", wantHTTP: http.StatusForbidden},
		{name: "object server error leaks nothing", objectStatus: http.StatusInternalServerError, wantCode: "network", wantHTTP: http.StatusInternalServerError, objectHasBody: "sentinel-object-body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			probes := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method != http.MethodHead {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if isBucketPath(r.URL.Path) {
					probes++
					if tt.bucketStatus == 0 {
						w.WriteHeader(http.StatusOK)
					} else {
						w.WriteHeader(tt.bucketStatus)
					}
					return
				}
				if tt.objectStatus == http.StatusOK {
					w.Header().Set("ETag", strconv.Quote("etag-present"))
					w.Header().Set("Content-Length", "1")
					w.WriteHeader(http.StatusOK)
					return
				}
				if tt.objectHasBody != "" {
					w.WriteHeader(tt.objectStatus)
					_, _ = io.WriteString(w, tt.objectHasBody)
					return
				}
				w.WriteHeader(tt.objectStatus)
			}))
			defer srv.Close()

			client := newTestClient(t, srv)
			_, exists, err := client.HeadObject(context.Background(), "images/missing.webp")
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("HeadObject: %v", err)
				}
				if exists != tt.wantExists {
					t.Fatalf("exists = %v, want %v", exists, tt.wantExists)
				}
			} else {
				if err == nil {
					t.Fatal("HeadObject succeeded, want error")
				}
				if exists {
					t.Fatal("exists = true on error")
				}
				if code := ErrorCode(err); code != tt.wantCode {
					t.Fatalf("ErrorCode = %q, want %q (err=%v)", code, tt.wantCode, err)
				}
				var r2err *Error
				if !errors.As(err, &r2err) {
					t.Fatalf("error %T is not *Error", err)
				}
				if r2err.HTTPStatus != tt.wantHTTP {
					t.Fatalf("HTTPStatus = %d, want %d", r2err.HTTPStatus, tt.wantHTTP)
				}
				if tt.objectHasBody != "" && strings.Contains(err.Error(), tt.objectHasBody) {
					t.Fatalf("error leaked response body: %v", err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if probes != tt.wantProbes {
				t.Fatalf("HeadBucket probes = %d, want %d", probes, tt.wantProbes)
			}
		})
	}
}

func TestDeleteObjectIsIdempotentButReportsBucketFailures(t *testing.T) {
	tests := []struct {
		name         string
		deleteStatus int
		bucketStatus int
		wantCode     string
		wantHTTP     int
		wantProbes   int
	}{
		{name: "deleted", deleteStatus: http.StatusNoContent},
		{name: "key already missing", deleteStatus: http.StatusNotFound, bucketStatus: http.StatusOK, wantProbes: 1},
		{name: "bucket missing", deleteStatus: http.StatusNotFound, bucketStatus: http.StatusNotFound, wantCode: "bucket_unavailable", wantHTTP: http.StatusNotFound, wantProbes: 1},
		{name: "forbidden", deleteStatus: http.StatusForbidden, wantCode: "access_denied", wantHTTP: http.StatusForbidden},
		{name: "server error", deleteStatus: http.StatusInternalServerError, wantCode: "network", wantHTTP: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			probes := 0
			deletes := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case http.MethodDelete:
					deletes++
					w.WriteHeader(tt.deleteStatus)
				case http.MethodHead:
					if !isBucketPath(r.URL.Path) {
						t.Errorf("unexpected HeadObject during delete: %s", r.URL.Path)
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					probes++
					if tt.bucketStatus == 0 {
						w.WriteHeader(http.StatusOK)
					} else {
						w.WriteHeader(tt.bucketStatus)
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			client := newTestClient(t, srv)
			err := client.DeleteObject(context.Background(), "images/gone.webp")
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("DeleteObject: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("DeleteObject succeeded, want error")
				}
				if code := ErrorCode(err); code != tt.wantCode {
					t.Fatalf("ErrorCode = %q, want %q (err=%v)", code, tt.wantCode, err)
				}
				var r2err *Error
				if !errors.As(err, &r2err) {
					t.Fatalf("error %T is not *Error", err)
				}
				if r2err.HTTPStatus != tt.wantHTTP {
					t.Fatalf("HTTPStatus = %d, want %d", r2err.HTTPStatus, tt.wantHTTP)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if deletes != 1 {
				t.Fatalf("DELETE requests = %d, want 1", deletes)
			}
			if probes != tt.wantProbes {
				t.Fatalf("HeadBucket probes = %d, want %d", probes, tt.wantProbes)
			}
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	var sourceHits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sourceHits, 1)
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, _, err := client.HeadObject(context.Background(), "images/redirect.webp")
	if err == nil {
		t.Fatal("HeadObject succeeded through redirect, want error")
	}
	if got := atomic.LoadInt32(&followed); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&sourceHits); got != 1 {
		t.Fatalf("source requests = %d, want 1 (no retry)", got)
	}
	if code := ErrorCode(err); code != "network" {
		t.Fatalf("ErrorCode = %q, want network", code)
	}
	var r2err *Error
	if !errors.As(err, &r2err) {
		t.Fatalf("error %T is not *Error", err)
	}
	if r2err.HTTPStatus != http.StatusTemporaryRedirect {
		t.Fatalf("HTTPStatus = %d, want 307", r2err.HTTPStatus)
	}
	if strings.Contains(err.Error(), target.URL) {
		t.Fatalf("error leaked redirect URL: %v", err)
	}
	if strings.Contains(err.Error(), testSecretKey) || strings.Contains(err.Error(), testAccessKey) {
		t.Fatalf("error leaked credentials: %v", err)
	}
}

func TestCancellationAndDeadlinePreserveErrorsIs(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	client := newTestClient(t, srv)

	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := client.HeadObject(ctx, "images/canceled.webp")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(context.Canceled) = false: %v", err)
		}
		if code := ErrorCode(err); code != "canceled" {
			t.Fatalf("ErrorCode = %q, want canceled", code)
		}
	})

	t.Run("deadline while in flight", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, err := client.HeadObject(ctx, "images/deadline.webp")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("errors.Is(context.DeadlineExceeded) = false: %v", err)
		}
		if code := ErrorCode(err); code != "timeout" {
			t.Fatalf("ErrorCode = %q, want timeout", code)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("deadline not enforced promptly: %v", elapsed)
		}
	})

	t.Run("cancel while in flight", func(t *testing.T) {
		started := make(chan struct{})
		streaming := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-r.Context().Done()
		}))
		defer streaming.Close()
		streamClient := newTestClient(t, streaming)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, _, err := streamClient.HeadObject(ctx, "images/cancel.webp")
			done <- err
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("errors.Is(context.Canceled) = false: %v", err)
			}
			if code := ErrorCode(err); code != "canceled" {
				t.Fatalf("ErrorCode = %q, want canceled", code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("HeadObject did not return after cancellation")
		}
	})
}

type capturedCall struct {
	method    string
	path      string
	remaining time.Duration
}

type captureTransport struct {
	mu    sync.Mutex
	calls []capturedCall
	fn    func(*http.Request) (*http.Response, error)
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	remaining := time.Duration(-1)
	if deadline, ok := req.Context().Deadline(); ok {
		remaining = time.Until(deadline)
	}
	c.mu.Lock()
	c.calls = append(c.calls, capturedCall{method: req.Method, path: req.URL.Path, remaining: remaining})
	c.mu.Unlock()
	return c.fn(req)
}

func (c *captureTransport) snapshot() []capturedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedCall, len(c.calls))
	copy(out, c.calls)
	return out
}

func TestPerOperationTimeoutsAndParentDeadline(t *testing.T) {
	object, repositoryID, sourceID, payload := testObject()
	transport := &captureTransport{fn: func(req *http.Request) (*http.Response, error) {
		header := http.Header{}
		switch req.Method {
		case http.MethodPut:
			header.Set("ETag", strconv.Quote("etag-timeout"))
		case http.MethodHead:
			header.Set("ETag", strconv.Quote("etag-timeout"))
			header.Set("Content-Length", strconv.Itoa(len(payload)))
			header.Set("X-Amz-Meta-Touko-Repository-Id", repositoryID.String())
			header.Set("X-Amz-Meta-Touko-Source-Id", sourceID)
			header.Set("X-Amz-Meta-Touko-Sha256", object.Hash.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	}}
	client, err := NewWithClient(testConfig("https://r2.example.test"), testAccessKey, testSecretKey, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}

	if _, _, err := client.HeadObject(context.Background(), "images/timeouts.webp"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if _, err := client.PutObject(context.Background(), "images/timeouts.webp", bytes.NewReader(payload), PutOptions{
		Object:       object,
		RepositoryID: repositoryID,
		SourceID:     sourceID,
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	calls := transport.snapshot()
	if len(calls) != 3 {
		t.Fatalf("HTTP calls = %d, want 3 (HEAD, PUT, verify HEAD): %+v", len(calls), calls)
	}
	if calls[0].method != http.MethodHead || calls[0].remaining <= 14*time.Second || calls[0].remaining > 15*time.Second {
		t.Fatalf("HEAD deadline window = %v, want about 15s", calls[0].remaining)
	}
	if calls[1].method != http.MethodPut || calls[1].remaining <= 4*time.Minute || calls[1].remaining > 5*time.Minute {
		t.Fatalf("PUT deadline window = %v, want about 5m", calls[1].remaining)
	}
	if calls[2].method != http.MethodHead || calls[2].remaining <= 14*time.Second || calls[2].remaining > 15*time.Second {
		t.Fatalf("verify HEAD deadline window = %v, want about 15s", calls[2].remaining)
	}

	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := client.HeadObject(parent, "images/parent.webp"); err != nil {
		t.Fatalf("HeadObject with parent deadline: %v", err)
	}
	calls = transport.snapshot()
	last := calls[len(calls)-1]
	if last.remaining <= time.Second || last.remaining > 2*time.Second {
		t.Fatalf("parent deadline was not preserved: remaining %v, want at most 2s", last.remaining)
	}
}

func TestClientRejectsDisabledConfigAndBadCredentials(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request during constructor failure: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	disabled := testConfig(srv.URL)
	disabled.Enabled = false
	httpConfig := testConfig(srv.URL)
	httpConfig.Endpoint = "http://r2.example.test"

	tests := []struct {
		name   string
		cfg    Config
		client *http.Client
		key    string
		secret string
		want   string
	}{
		{name: "disabled", cfg: disabled, client: srv.Client(), key: testAccessKey, secret: testSecretKey, want: "configuration_invalid"},
		{name: "missing access key", cfg: testConfig(srv.URL), client: srv.Client(), secret: testSecretKey, want: "credentials_missing"},
		{name: "blank secret", cfg: testConfig(srv.URL), client: srv.Client(), key: testAccessKey, secret: " ", want: "credentials_missing"},
		{name: "control in access key", cfg: testConfig(srv.URL), client: srv.Client(), key: "bad\nkey", secret: testSecretKey, want: "credentials_missing"},
		{name: "control in secret", cfg: testConfig(srv.URL), client: srv.Client(), key: testAccessKey, secret: "bad\x00secret", want: "credentials_missing"},
		{name: "non-tls endpoint", cfg: httpConfig, client: srv.Client(), key: testAccessKey, secret: testSecretKey, want: "configuration_invalid"},
		{name: "nil http client", cfg: testConfig(srv.URL), key: testAccessKey, secret: testSecretKey, want: "configuration_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWithClient(tt.cfg, tt.key, tt.secret, tt.client)
			if err == nil {
				t.Fatal("NewWithClient succeeded, want error")
			}
			if code := ErrorCode(err); code != tt.want {
				t.Fatalf("ErrorCode = %q, want %q (err=%v)", code, tt.want, err)
			}
			if tt.secret == testSecretKey && strings.Contains(err.Error(), tt.secret) {
				t.Fatalf("error leaked credential: %v", err)
			}
			if tt.key == testAccessKey && tt.secret == testSecretKey && strings.Contains(err.Error(), tt.key) {
				t.Fatalf("error leaked access key: %v", err)
			}
		})
	}
}

func TestOperationsRejectUnsafeKeysBeforeNetwork(t *testing.T) {
	var requests int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := newTestClient(t, srv)
	object, repositoryID, sourceID, payload := testObject()

	keys := []string{
		"",
		".",
		"/absolute.webp",
		"../escape.webp",
		"images/../escape.webp",
		"images//double.webp",
		`images\windows.webp`,
		".git/config",
		"images/.TOUKO/metadata.json",
		"images/\x00null.webp",
		"images/\nnewline.webp",
	}
	for _, key := range keys {
		_, _, err := client.HeadObject(context.Background(), key)
		if code := ErrorCode(err); code != "configuration_invalid" {
			t.Fatalf("HeadObject(%q) ErrorCode = %q, want configuration_invalid", key, code)
		}
		_, err = client.PutObject(context.Background(), key, bytes.NewReader(payload), PutOptions{Object: object, RepositoryID: repositoryID, SourceID: sourceID})
		if code := ErrorCode(err); code != "configuration_invalid" {
			t.Fatalf("PutObject(%q) ErrorCode = %q, want configuration_invalid", key, code)
		}
		err = client.DeleteObject(context.Background(), key)
		if code := ErrorCode(err); code != "configuration_invalid" {
			t.Fatalf("DeleteObject(%q) ErrorCode = %q, want configuration_invalid", key, code)
		}
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("network requests = %d, want 0 for invalid keys", got)
	}
}

func TestPutObjectValidatesObjectAndCondition(t *testing.T) {
	var requests int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := newTestClient(t, srv)
	validObject, repositoryID, sourceID, payload := testObject()

	tests := []struct {
		name  string
		body  io.ReadSeeker
		opts  PutOptions
		check func(*testing.T, error)
	}{
		{name: "nil body", body: nil, opts: PutOptions{Object: validObject, RepositoryID: repositoryID, SourceID: sourceID}, check: wantConfigError},
		{name: "missing hash", body: bytes.NewReader(payload), opts: PutOptions{Object: domain.ContentObject{MIME: validObject.MIME, Ext: validObject.Ext, Size: validObject.Size, Width: 1, Height: 1}, RepositoryID: repositoryID, SourceID: sourceID}, check: wantConfigError},
		{name: "invalid object", body: bytes.NewReader(payload), opts: PutOptions{Object: domain.ContentObject{MIME: "text/plain", Ext: "webp", Size: 10, Width: 1, Height: 1}, RepositoryID: repositoryID, SourceID: sourceID}, check: wantConfigError},
		{name: "zero repository", body: bytes.NewReader(payload), opts: PutOptions{Object: validObject, SourceID: sourceID}, check: wantConfigError},
		{name: "short source id", body: bytes.NewReader(payload), opts: PutOptions{Object: validObject, RepositoryID: repositoryID, SourceID: "abc"}, check: wantConfigError},
		{name: "uppercase source id", body: bytes.NewReader(payload), opts: PutOptions{Object: validObject, RepositoryID: repositoryID, SourceID: strings.Repeat("AB", 32)}, check: wantConfigError},
		{name: "unsafe if match", body: bytes.NewReader(payload), opts: PutOptions{Object: validObject, RepositoryID: repositoryID, SourceID: sourceID, IfMatch: "etag\r\nX-Injected: yes"}, check: wantConfigError},
		{name: "quoted if match", body: bytes.NewReader(payload), opts: PutOptions{Object: validObject, RepositoryID: repositoryID, SourceID: sourceID, IfMatch: `"etag"`}, check: wantConfigError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.PutObject(context.Background(), "images/valid.webp", tt.body, tt.opts)
			if err == nil {
				t.Fatal("PutObject succeeded, want error")
			}
			tt.check(t, err)
		})
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("network requests = %d, want 0 for invalid input", got)
	}
}

func wantConfigError(t *testing.T, err error) {
	t.Helper()
	if code := ErrorCode(err); code != "configuration_invalid" {
		t.Fatalf("ErrorCode = %q, want configuration_invalid (err=%v)", code, err)
	}
}

func TestPutObjectRejectsReadBackMismatch(t *testing.T) {
	tests := []struct {
		name          string
		missingObject bool
		hash          string
	}{
		{name: "metadata mismatch", hash: strings.Repeat("0", 64)},
		{name: "object missing after put", missingObject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object, repositoryID, sourceID, payload := testObject()
			var puts int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPut:
					atomic.AddInt32(&puts, 1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", strconv.Quote("etag-returned"))
					w.WriteHeader(http.StatusOK)
				case http.MethodHead:
					if isBucketPath(r.URL.Path) {
						w.WriteHeader(http.StatusOK)
						return
					}
					if tt.missingObject {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("ETag", strconv.Quote("etag-returned"))
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
					w.Header().Set("X-Amz-Meta-Touko-Repository-Id", repositoryID.String())
					w.Header().Set("X-Amz-Meta-Touko-Source-Id", sourceID)
					w.Header().Set("X-Amz-Meta-Touko-Sha256", tt.hash)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			client := newTestClient(t, srv)
			etag, err := client.PutObject(context.Background(), "images/readback.webp", bytes.NewReader(payload), PutOptions{
				Object:       object,
				RepositoryID: repositoryID,
				SourceID:     sourceID,
			})
			if err == nil {
				t.Fatal("PutObject accepted an unverified read-back")
			}
			if etag != "" {
				t.Fatalf("etag = %q, want empty on failure", etag)
			}
			if code := ErrorCode(err); code != "object_conflict" {
				t.Fatalf("ErrorCode = %q, want object_conflict (err=%v)", code, err)
			}
			if got := atomic.LoadInt32(&puts); got != 1 {
				t.Fatalf("PUT attempts = %d, want 1", got)
			}
		})
	}
}

func TestPutObjectDoesNotRetryServerErrors(t *testing.T) {
	object, repositoryID, sourceID, payload := testObject()
	var puts int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&puts, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.PutObject(context.Background(), "images/retry.webp", bytes.NewReader(payload), PutOptions{
		Object:       object,
		RepositoryID: repositoryID,
		SourceID:     sourceID,
	})
	if code := ErrorCode(err); code != "network" {
		t.Fatalf("ErrorCode = %q, want network (err=%v)", code, err)
	}
	if got := atomic.LoadInt32(&puts); got != 1 {
		t.Fatalf("PUT attempts = %d, want 1 (RetryMaxAttempts=1)", got)
	}
}

func TestErrorContract(t *testing.T) {
	cause := errors.New("inner-sentinel-cause")
	err := &Error{Op: "put", Key: "images/a\nb.webp", Code: "object_conflict", HTTPStatus: http.StatusConflict, Cause: cause}
	message := err.Error()
	for _, want := range []string{"r2", "put", "object_conflict", "409"} {
		if !strings.Contains(message, want) {
			t.Fatalf("Error() = %q, want %q", message, want)
		}
	}
	if strings.Contains(message, "inner-sentinel-cause") {
		t.Fatalf("Error() leaked Cause: %q", message)
	}
	if strings.Contains(message, "\n") {
		t.Fatalf("Error() emitted a raw newline: %q", message)
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is did not unwrap Cause")
	}
	if code := ErrorCode(err); code != "object_conflict" {
		t.Fatalf("ErrorCode = %q, want object_conflict", code)
	}
	if code := ErrorCode(fmt.Errorf("wrapped: %w", err)); code != "object_conflict" {
		t.Fatalf("ErrorCode(wrapped) = %q, want object_conflict", code)
	}
	if code := ErrorCode(&Error{Code: ""}); code != "network" {
		t.Fatalf("ErrorCode(empty) = %q, want network", code)
	}
	if code := ErrorCode(errors.New("unknown")); code != "network" {
		t.Fatalf("ErrorCode(unknown) = %q, want network", code)
	}
	if code := ErrorCode(nil); code != "" {
		t.Fatalf("ErrorCode(nil) = %q, want empty", code)
	}
}

func TestR2MissingObjectProbeSharesOperationDeadline(t *testing.T) {
	var deadlines []time.Time
	transport := &captureTransport{fn: func(req *http.Request) (*http.Response, error) {
		deadline, _ := req.Context().Deadline()
		deadlines = append(deadlines, deadline)
		status := http.StatusNotFound
		if isBucketPath(req.URL.Path) {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}}
	client, err := NewWithClient(testConfig("https://r2.example.test"), testAccessKey, testSecretKey, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	_, exists, err := client.HeadObject(t.Context(), "images/missing.webp")
	if err != nil || exists {
		t.Fatal(exists, err)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("bucket probe extended HEAD deadline: %v", deadlines)
	}
}
