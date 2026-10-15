// Package github implements the GitHub Git Data API without caching credentials.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const apiVersion = "2026-03-10"
const jsonAccept = "application/vnd.github+json"
const rawAccept = "application/vnd.github.raw+json"
const requestTimeout = 45 * time.Second

// APIError preserves HTTP classification without retaining request credentials.
type APIError struct {
	Status       int
	Endpoint     string
	Message      string
	unauthorized bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s: HTTP %d: %s", e.Endpoint, e.Status, e.Message)
}
func (e *APIError) APIStatus() int { return e.Status }
func (e *APIError) Unwrap() error {
	if e.unauthorized {
		return domain.ErrUnauthorized
	}
	return nil
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// Repository addresses a single GitHub branch. GitHub's non-force ref update is
// fast-forward protection, not an expected-old-SHA CAS against external resets.
type Repository struct {
	location repository.Location
	token    string
	apiURL   string
	client   *http.Client
}

var _ repository.Git = (*Repository)(nil)

// New uses only the production GitHub API endpoint and a finite timeout.
func New(location repository.Location, token string) (*Repository, error) {
	return NewWithClient(location, token, "https://api.github.com", &http.Client{Timeout: requestTimeout})
}

// NewWithClient permits an injected API endpoint and transport for HTTP tests.
func NewWithClient(location repository.Location, token, apiURL string, client *http.Client) (*Repository, error) {
	if err := validLocation(location); err != nil {
		return nil, err
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid GitHub API endpoint")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("invalid GitHub token")
	}
	if client == nil {
		return nil, fmt.Errorf("GitHub HTTP client is required")
	}
	copyClient := *client
	if copyClient.Timeout <= 0 {
		copyClient.Timeout = requestTimeout
	}
	// Never forward a bearer token to a redirect target or replay an upload.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Repository{location: location, token: token, apiURL: strings.TrimRight(apiURL, "/"), client: &copyClient}, nil
}

func validLocation(l repository.Location) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if l.Backend != "github" {
		return fmt.Errorf("GitHub adapter requires github location")
	}
	if !validName(l.Owner) || !validName(l.Name) {
		return fmt.Errorf("invalid GitHub owner or repository name")
	}
	if !validBranch(l.Branch) {
		return fmt.Errorf("invalid GitHub branch")
	}
	return nil
}
func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func validBranch(s string) bool {
	if s == "" || s == "HEAD" || s == "@" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") || strings.Contains(s, "@{") {
		return false
	}
	for _, c := range s {
		if c <= 32 || c == 127 || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
func (r *Repository) redact(s string) string {
	if r.token != "" {
		s = strings.ReplaceAll(s, r.token, "[redacted]")
	}
	return s
}
func (r *Repository) safeError(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	return &redactedError{message: r.redact(err.Error()), cause: err}
}
func (r *Repository) path() string {
	return "/repos/" + url.PathEscape(r.location.Owner) + "/" + url.PathEscape(r.location.Name)
}
func escapeBranch(branch string) string {
	parts := strings.Split(branch, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func (r *Repository) writeAllowed() error {
	if r.token == "" {
		return fmt.Errorf("%w: set GITHUB_TOKEN for GitHub writes", domain.ErrUnauthorized)
	}
	return nil
}

func (r *Repository) request(ctx context.Context, method, endpoint, accept string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.apiURL+endpoint, body)
	if err != nil {
		return nil, r.safeError(err)
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	response, err := r.client.Do(req)
	if err != nil {
		return nil, r.safeError(err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &payload)
	message := payload.Message
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	message = r.redact(message)
	if len(message) > 4096 {
		message = message[:4096]
	}
	lower := strings.ToLower(message)
	rateLimited := response.StatusCode == 429 || response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "" || strings.Contains(lower, "rate limit") || strings.Contains(lower, "abuse detection")
	if readErr != nil {
		message += "; error response unreadable"
	}
	return nil, &APIError{Status: response.StatusCode, Endpoint: r.redact(endpoint), Message: message, unauthorized: response.StatusCode == 401 || (response.StatusCode == 403 && !rateLimited)}
}

func (r *Repository) json(ctx context.Context, method, endpoint string, input, output any) error {
	var body io.Reader
	var size int64
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return r.safeError(err)
		}
		body = bytes.NewReader(data)
		size = int64(len(data))
	}
	response, err := r.request(ctx, method, endpoint, jsonAccept, body, size)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return r.safeError(err)
	}
	// JSON API metadata is bounded; binary blobs use the separate streaming path.
	dec := json.NewDecoder(io.LimitReader(response.Body, (64<<20)+1))
	if err = dec.Decode(output); err != nil {
		return r.safeError(fmt.Errorf("GitHub %s: invalid JSON: %w", endpoint, err))
	}
	if err = dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("GitHub %s: trailing or oversized JSON", r.redact(endpoint))
	}
	return nil
}

// Discover reads repository visibility/default branch and validates the selected ref.
func (r *Repository) Discover(ctx context.Context, owner, name, branch string) (repository.Location, error) {
	if !validName(owner) || !validName(name) {
		return repository.Location{}, fmt.Errorf("invalid GitHub owner or repository name")
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if err := r.json(ctx, http.MethodGet, endpoint, nil, &info); err != nil {
		return repository.Location{}, err
	}
	if branch == "" {
		branch = info.DefaultBranch
	}
	l := repository.Location{Backend: "github", Owner: owner, Name: name, Branch: branch, Private: info.Private}
	if err := validLocation(l); err != nil {
		return repository.Location{}, err
	}
	candidate := *r
	candidate.location = l
	if _, err := candidate.Head(ctx); err != nil {
		return repository.Location{}, err
	}
	return l, nil
}

// User returns the authenticated account login without exposing the bearer token.
func (r *Repository) User(ctx context.Context) (string, error) {
	if err := r.writeAllowed(); err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := r.json(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", fmt.Errorf("GitHub user response has no login")
	}
	return user.Login, nil
}
