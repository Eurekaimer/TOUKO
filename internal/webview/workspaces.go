package webview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

type workspaceRequest struct{}

type workspaceSet struct {
	mu      sync.Mutex
	root    *Server
	servers map[string]*Server
	// libraryTokens holds this process's per-library CSRF tokens. Like the root
	// token they are never persisted, so a restart invalidates stale pages.
	libraryTokens map[string]string
}

type workspaceEntry struct {
	ID       string              `json:"id"`
	URL      string              `json:"url"`
	Location repository.Location `json:"location"`
}

func workspaceKey(location repository.Location) string {
	data, _ := json.Marshal(location) // Location contains only strings and a bool.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validWorkspaceID(id string) bool {
	if id == "default" {
		return true
	}
	if len(id) != 64 {
		return false
	}
	for _, ch := range id {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return false
		}
	}
	return true
}

func workspaceURL(id string) string { return "/w/" + id + "/" }

func (m *workspaceSet) directory(id string) string {
	if id == "default" {
		return m.root.service.Dir
	}
	return filepath.Join(m.root.service.Dir, "workspaces", id)
}

// open never changes an existing Server's service. Open image handles and slow
// publication requests therefore remain bound to the repository they started in.
func (m *workspaceSet) open(ctx context.Context, id string, create bool) (*Server, error) {
	if !validWorkspaceID(id) {
		return nil, usage("invalid workspace")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if server := m.servers[id]; server != nil {
		return server, nil
	}
	dir := m.directory(id)
	info, err := os.Lstat(dir)
	if err == nil && !info.IsDir() {
		return nil, fmt.Errorf("workspace is not a directory")
	}
	if err != nil && (!create || !errors.Is(err, os.ErrNotExist)) {
		return nil, err
	}
	if !create {
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
			return nil, err
		}
	}
	service, err := app.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	server, err := newServer(service, m.root.token, m.root.origin)
	if err != nil {
		return nil, errors.Join(err, service.Close())
	}
	server.spaces, server.workspaceID = m, id
	m.servers[id] = server
	return server, nil
}

func (m *workspaceSet) selectLocation(ctx context.Context, location repository.Location) (*Server, error) {
	previous, err := m.root.location(ctx)
	if err != nil {
		return nil, err
	}
	id := workspaceKey(location)
	if previous == nil || *previous == location {
		id = "default"
	}
	server, err := m.open(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if err := server.service.Select(ctx, location); err != nil {
		return nil, err
	}
	return server, nil
}

func (m *workspaceSet) list(ctx context.Context, current string) (any, error) {
	entries := make([]workspaceEntry, 0)
	location, err := m.root.location(ctx)
	if err != nil {
		return nil, err
	}
	if location != nil {
		entries = append(entries, workspaceEntry{ID: "default", URL: workspaceURL("default"), Location: *location})
	}
	children, err := os.ReadDir(filepath.Join(m.root.service.Dir, "workspaces"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, child := range children {
		id := child.Name()
		if !child.IsDir() || id == "default" || !validWorkspaceID(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.directory(id), "config.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue // A failed selection can leave an empty, unselected workspace.
		}
		if err != nil {
			return nil, err
		}
		var location repository.Location
		if err := json.Unmarshal(data, &location); err != nil {
			return nil, fmt.Errorf("workspace %s configuration: %w", id, err)
		}
		if err := location.Validate(); err != nil {
			return nil, fmt.Errorf("workspace %s configuration: %w", id, err)
		}
		if workspaceKey(location) != id {
			return nil, fmt.Errorf("workspace %s repository identity differs from its directory", id)
		}
		entries = append(entries, workspaceEntry{ID: id, URL: workspaceURL(id), Location: location})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return map[string]any{"current": current, "workspaces": entries}, nil
}

func (m *workspaceSet) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/w/"), "/", 2)
	id := parts[0]
	if !validWorkspaceID(id) {
		m.root.fail(w, 404, "usage", "unknown workspace")
		return
	}
	if len(parts) != 2 {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			m.root.fail(w, 404, "usage", "workspace URL requires a trailing slash")
			return
		}
		http.Redirect(w, r, workspaceURL(id), http.StatusPermanentRedirect)
		return
	}
	server, err := m.open(r.Context(), id, false)
	if errors.Is(err, os.ErrNotExist) {
		m.root.fail(w, 404, "usage", "unknown workspace")
		return
	}
	if err != nil {
		m.root.respondError(w, err)
		return
	}
	request := r.Clone(context.WithValue(r.Context(), workspaceRequest{}, true))
	request.URL.Path, request.URL.RawPath = "/"+parts[1], ""
	server.ServeHTTP(w, request)
}

// close runs after the Host has joined all requests. The caller still owns the
// original Service; only the extra workspaces opened by this server are closed.
func (m *workspaceSet) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for id, server := range m.servers {
		if id != "default" {
			result = errors.Join(result, server.service.Close())
			delete(m.servers, id)
		}
	}
	return result
}
