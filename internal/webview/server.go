package webview

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const maxJSONBytes = 1 << 20
const maxUploadBytes int64 = 500_000_000
const maxImageBytes int64 = 100_000_000
const maxUploadFiles = 100

// Server borrows its Service; the caller owns the service and listener lifetime.
type Server struct {
	service                   *app.Service
	token, origin, host, csrf string
	// Queue this server's state operations; the filesystem lock still rejects
	// competing CLI processes. Original-image streaming runs outside this gate.
	workMu      sync.Mutex
	cacheMu     sync.Mutex
	cachedTree  repository.TreeID
	cachedBlobs map[string]repository.GitEntry
	// clone holds the working copy's one-time-per-process state; its own mutex
	// keeps concurrent previews from starting competing clones.
	clone cloneOnce
	// Each URL owns one immutable Service. Switching repositories navigates to a
	// different Server instead of changing the state under in-flight requests.
	spaces       *workspaceSet
	workspaceID  string
	authMu       sync.Mutex
	authToken    string
	authSource   string
	authLogin    string
	authResolved bool
	// authResolve is the process-wide credential lookup. The root Server owns
	// it so every workspace observes the same refreshed gh identity.
	authResolve func(context.Context) (string, string, error)
	// login, like auth, is authoritative on the root Server only.
	login *loginManager
}

func New(service *app.Service, token, origin string) (*Server, error) {
	server, err := newServer(service, token, origin)
	if err != nil {
		return nil, err
	}
	server.login = newLoginManager()
	server.workspaceID = "default"
	server.spaces = &workspaceSet{root: server, servers: map[string]*Server{"default": server}}
	return server, nil
}

func newServer(service *app.Service, token, origin string) (*Server, error) {
	if service == nil || service.Store == nil {
		return nil, fmt.Errorf("application service is required")
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("invalid loopback origin")
	}
	ip := net.ParseIP(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || origin != "http://"+u.Host {
		return nil, fmt.Errorf("origin must be an exact HTTP loopback origin with a port")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("invalid GitHub token")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	if service.NewCloudflare == nil {
		service.NewCloudflare = app.NewCloudflareMirror
	}
	return &Server{service: service, token: token, origin: origin, host: u.Host, csrf: hex.EncodeToString(nonce[:])}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' blob: data:; connect-src 'self' touko-clipboard: http://touko-clipboard.localhost; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	// State and images must never be reused; static assets override this with
	// "no-cache" plus a content tag so unchanged fonts revalidate instead of
	// being downloaded again.
	h.Set("Cache-Control", "no-store")
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	if r.Host != s.host || (origin != "" && origin != s.origin) || (site != "" && site != "same-origin" && site != "none") {
		s.fail(w, http.StatusForbidden, "unauthorized", "only same-origin loopback requests are allowed")
		return
	}
	if err := r.Context().Err(); err != nil {
		s.respondError(w, err)
		return
	}
	if s.spaces != nil && r.Context().Value(workspaceRequest{}) == nil && r.Context().Value(libraryRequest{}) == nil {
		if strings.HasPrefix(r.URL.Path, "/w/") {
			s.spaces.serve(w, r)
			return
		}
		// Library URLs are dispatched before the root CSRF check, exactly like
		// workspaces. The library handler performs its own exact Origin plus
		// that library's CSRF comparison before any write reaches business code.
		if strings.HasPrefix(r.URL.Path, "/l/") {
			s.spaces.serveLibrary(w, r)
			return
		}
		if r.URL.Path == "/" && (r.Method == "GET" || r.Method == "HEAD") {
			target, err := s.spaces.activeURL(r.Context())
			if err != nil {
				s.respondError(w, err)
				return
			}
			if target != "" {
				http.Redirect(w, r, target, http.StatusSeeOther)
				return
			}
		}
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		// Login endpoints are process-wide, so member URLs accept the root
		// token, matching the one session and one credential cache they use.
		csrf := s.csrf
		if isLoginEndpoint(r.URL.Path) {
			csrf = s.authOwner().csrf
		}
		if origin != s.origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Touko-CSRF")), []byte(csrf)) != 1 {
			s.fail(w, http.StatusForbidden, "unauthorized", "exact Origin and X-Touko-CSRF are required")
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		s.api(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		s.fail(w, 405, "usage", "method not allowed")
		return
	}
	serveStatic(w, r)
}

func (s *Server) json(w http.ResponseWriter, value any) {
	s.jsonStatus(w, http.StatusOK, value)
}
func (s *Server) jsonStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) fail(w http.ResponseWriter, status int, code, message string) {
	s.failExtra(w, status, code, message, nil)
}
func (s *Server) failExtra(w http.ResponseWriter, status int, code, message string, extra map[string]any) {
	owner := s.authOwner()
	owner.authMu.Lock()
	token, authToken := owner.token, owner.authToken
	owner.authMu.Unlock()
	secrets := append([]string{token, authToken, os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"), os.Getenv("TOUKO_R2_ACCESS_KEY_ID"), os.Getenv("TOUKO_R2_SECRET_ACCESS_KEY")}, s.r2Secrets()...)
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	body := map[string]any{"error": map[string]string{"code": code, "message": message}}
	for key, value := range extra {
		body[key] = value
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// r2Secrets returns this workspace's stored R2 pair for response scrubbing.
// It is consulted on error paths only, tolerates a missing or unreadable
// credential file, and never exposes the pair to a response.
func (s *Server) r2Secrets() []string {
	if s.service == nil {
		return nil
	}
	pair, found, err := credential.LoadR2(context.Background(), s.service.Dir)
	if err != nil || !found {
		return nil
	}
	return []string{pair.AccessKeyID, pair.SecretAccessKey}
}

type requestError struct {
	status  int
	message string
}

func (e *requestError) Error() string        { return e.message }
func usage(format string, args ...any) error { return &requestError{400, fmt.Sprintf(format, args...)} }
func tooLarge(message string) error          { return &requestError{413, message} }

// errorDetail classifies an error once, so HTTP responses and per-member
// library failures always report the same code for the same cause.
func errorDetail(err error) (int, string, string) {
	var input *requestError
	var limit *http.MaxBytesError
	var api *github.APIError
	var mirror *app.CloudflareSyncError
	var settings *app.CloudflareConfigError
	switch {
	case errors.As(err, &settings):
		return 400, settings.Code, settings.Message
	case errors.As(err, &mirror):
		return 502, "cloudflare_sync_failed", err.Error()
	case errors.As(err, &input):
		return input.status, "usage", input.message
	case errors.As(err, &limit):
		return 413, "usage", "request body exceeds the size limit"
	case errors.Is(err, domain.ErrAssetNotFound):
		return 404, "asset_not_found", err.Error()
	case errors.Is(err, domain.ErrAmbiguousSelector):
		return 400, "ambiguous_selector", err.Error()
	case errors.Is(err, domain.ErrConflict):
		return 409, "conflict", err.Error()
	case errors.Is(err, domain.ErrBusy):
		return 409, "busy", err.Error()
	case errors.Is(err, domain.ErrRepositoryCorrupt):
		return 422, "repository_corrupt", err.Error()
	case errors.Is(err, domain.ErrHashMismatch):
		return 422, "hash_mismatch", err.Error()
	case errors.Is(err, domain.ErrRepositoryUninitialized):
		return 409, "repository_uninitialized", err.Error()
	case errors.Is(err, domain.ErrDeliveryUnavailable):
		return 400, "delivery_unavailable", err.Error()
	case errors.As(err, &api):
		// A real GitHub 403 is an authenticated account without permission, not
		// a missing login; a rate-limited 403 keeps the generic API failure.
		switch {
		case api.Status == 401:
			return 401, "unauthorized", err.Error()
		case api.Status == 403 && errors.Is(err, domain.ErrUnauthorized):
			return 403, "forbidden", err.Error()
		default:
			return 502, "api_error", err.Error()
		}
	case errors.Is(err, domain.ErrUnauthorized):
		return 401, "unauthorized", err.Error()
	default:
		return 500, "io_error", err.Error()
	}
}
func (s *Server) respondError(w http.ResponseWriter, err error) {
	status, code, message := errorDetail(err)
	var mirror *app.CloudflareSyncError
	if errors.As(err, &mirror) {
		s.failExtra(w, status, code, message, map[string]any{"cloudflare": mirror.Result})
		return
	}
	s.fail(w, status, code, message)
}
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return usage("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	decoder := json.NewDecoder(r.Body)
	// All mutation inputs are objects: null must not silently act as {}.
	var raw json.RawMessage
	if err = decoder.Decode(&raw); err != nil {
		return bodyError(err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		return usage("request body must be a JSON object")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		if err != nil {
			return bodyError(err)
		}
		return usage("exactly one JSON object is required")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return usage("invalid JSON: %v", err)
	}
	return nil
}
func bodyError(err error) error {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		return err
	}
	return usage("invalid request body: %v", err)
}
