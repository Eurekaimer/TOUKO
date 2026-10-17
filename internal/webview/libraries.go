package webview

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/image"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// A library is a named, ordered list of workspaces presented as one gallery.
// It stores no pictures: every asset still lives in, and is published from, the
// repository workspace that owns it. The configuration therefore holds only
// workspace IDs, which keeps a library reconstructible from the per-repository
// metadata rather than from this file.
const viewsFile = "views.json"
const viewsVersion = 1

// libraryRequest marks a request that a library handler already authorized.
type libraryRequest struct{ library string }

type libraryView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Workspaces []string `json:"workspaces"`
}

type activeView struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type viewConfig struct {
	Version   int           `json:"version"`
	Active    activeView    `json:"active"`
	Libraries []libraryView `json:"libraries"`
}

func libraryURL(id string) string { return "/l/" + id + "/" }

func validLibraryID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range id {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return false
		}
	}
	return true
}

func (m *workspaceSet) viewsPath() string { return filepath.Join(m.root.service.Dir, viewsFile) }

// loadViews reads the library configuration, migrating the single
// active-workspace marker on first use. A corrupt file is reported, never
// silently replaced: a user's gallery list is not reconstructible from anywhere
// else on this device.
func (m *workspaceSet) loadViews() (viewConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadViewsLocked()
}

func (m *workspaceSet) loadViewsLocked() (viewConfig, error) {
	data, err := os.ReadFile(m.viewsPath())
	if errors.Is(err, os.ErrNotExist) {
		return m.migrateActiveLocked()
	}
	if err != nil {
		return viewConfig{}, err
	}
	var config viewConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return viewConfig{}, fmt.Errorf("views.json: %w", err)
	}
	if config.Version != viewsVersion {
		return viewConfig{}, fmt.Errorf("views.json: unsupported version %d", config.Version)
	}
	seen := make(map[string]bool, len(config.Libraries))
	for _, library := range config.Libraries {
		if !validLibraryID(library.ID) || strings.TrimSpace(library.Name) == "" {
			return viewConfig{}, fmt.Errorf("views.json: invalid library entry")
		}
		if seen[library.ID] {
			return viewConfig{}, fmt.Errorf("views.json: duplicate library %s", library.ID)
		}
		seen[library.ID] = true
		members := make(map[string]bool, len(library.Workspaces))
		for _, id := range library.Workspaces {
			if !validWorkspaceID(id) || members[id] {
				return viewConfig{}, fmt.Errorf("views.json: invalid member in library %s", library.ID)
			}
			members[id] = true
		}
	}
	switch config.Active.Kind {
	case "", "workspace":
		if config.Active.ID != "" && !validWorkspaceID(config.Active.ID) {
			return viewConfig{}, fmt.Errorf("views.json: invalid active workspace")
		}
	case "library":
		if !seen[config.Active.ID] {
			return viewConfig{}, fmt.Errorf("views.json: active library %s is missing", config.Active.ID)
		}
	default:
		return viewConfig{}, fmt.Errorf("views.json: unknown active view %q", config.Active.Kind)
	}
	if config.Libraries == nil {
		config.Libraries = []libraryView{}
	}
	return config, nil
}

// migrateActiveLocked converts the pre-library marker once. The old file is
// removed only after the new configuration is durable.
func (m *workspaceSet) migrateActiveLocked() (viewConfig, error) {
	config := viewConfig{Version: viewsVersion, Libraries: []libraryView{}}
	data, err := os.ReadFile(filepath.Join(m.root.service.Dir, "active-workspace"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return viewConfig{}, err
	default:
		id := strings.TrimSpace(string(data))
		if !validWorkspaceID(id) {
			return viewConfig{}, fmt.Errorf("invalid active workspace")
		}
		if _, err := os.Stat(filepath.Join(m.directory(id), "config.json")); err != nil {
			return viewConfig{}, fmt.Errorf("active workspace configuration: %w", err)
		}
		config.Active = activeView{Kind: "workspace", ID: id}
		if err := writeViews(m.viewsPath(), config); err != nil {
			return viewConfig{}, err
		}
		if err := os.Remove(filepath.Join(m.root.service.Dir, "active-workspace")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return viewConfig{}, err
		}
		return config, nil
	}
	return config, nil
}

// saveViews writes the configuration atomically: a temporary file in the same
// directory is synced before it replaces the old configuration.
func (m *workspaceSet) saveViews(config viewConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeViews(m.viewsPath(), config)
}

func writeViews(path string, config viewConfig) error {
	if config.Libraries == nil {
		config.Libraries = []libraryView{}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "views-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// libraryByID returns one configured library. The caller supplies the already
// validated ID so unknown names never reach the filesystem.
func (m *workspaceSet) libraryByID(id string) (libraryView, error) {
	config, err := m.loadViews()
	if err != nil {
		return libraryView{}, err
	}
	for _, library := range config.Libraries {
		if library.ID == id {
			return library, nil
		}
	}
	return libraryView{}, os.ErrNotExist
}

// libraryCSRF returns this process's token for one library. It is generated on
// first use and never persisted, so a restart invalidates stale pages exactly
// like the per-workspace roots.
func (m *workspaceSet) libraryCSRF(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.libraryTokens == nil {
		m.libraryTokens = make(map[string]string)
	}
	if token, ok := m.libraryTokens[id]; ok {
		return token, nil
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	m.libraryTokens[id] = token
	return token, nil
}

func (m *workspaceSet) activeURL(ctx context.Context) (string, error) {
	config, err := m.loadViews()
	if err != nil {
		return "", err
	}
	switch config.Active.Kind {
	case "library":
		if config.Active.ID == "" {
			return "", nil
		}
		return libraryURL(config.Active.ID), nil
	case "workspace":
		id := config.Active.ID
		if id == "" {
			return "", nil
		}
		if !validWorkspaceID(id) {
			return "", fmt.Errorf("invalid active workspace")
		}
		if _, err := os.Stat(filepath.Join(m.directory(id), "config.json")); err != nil {
			return "", fmt.Errorf("active workspace configuration: %w", err)
		}
		return workspaceURL(id), nil
	default:
		return "", nil
	}
}

func (m *workspaceSet) remember(id string) error {
	config, err := m.loadViews()
	if err != nil {
		return err
	}
	config.Active = activeView{Kind: "workspace", ID: id}
	return m.saveViews(config)
}

func (m *workspaceSet) rememberLibrary(id string) error {
	config, err := m.loadViews()
	if err != nil {
		return err
	}
	config.Active = activeView{Kind: "library", ID: id}
	return m.saveViews(config)
}

// serveLibrary dispatches /l/<id>/... The library performs its own exact Origin
// and library-CSRF check before any write: a valid member or root token must
// never authorize a library mutation, and vice versa.
func (m *workspaceSet) serveLibrary(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/l/"), "/", 2)
	id := parts[0]
	if !validLibraryID(id) {
		m.root.fail(w, 404, "usage", "unknown library")
		return
	}
	if _, err := m.libraryByID(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.root.fail(w, 404, "usage", "unknown library")
		} else {
			m.root.respondError(w, err)
		}
		return
	}
	if len(parts) != 2 {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			m.root.fail(w, 404, "usage", "library URL requires a trailing slash")
			return
		}
		http.Redirect(w, r, libraryURL(id), http.StatusPermanentRedirect)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		csrf, err := m.libraryCSRF(id)
		if err != nil {
			m.root.respondError(w, err)
			return
		}
		if r.Header.Get("Origin") != m.root.origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Touko-CSRF")), []byte(csrf)) != 1 {
			m.root.fail(w, http.StatusForbidden, "unauthorized", "exact Origin and this library's X-Touko-CSRF are required")
			return
		}
	}
	rest := parts[1]
	if strings.HasPrefix(rest, "api/") || rest == "api" {
		m.libraryAPI(w, r, id, rest)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		m.root.fail(w, 405, "usage", "method not allowed")
		return
	}
	rewritten := r.Clone(context.WithValue(r.Context(), libraryRequest{}, id))
	rewritten.URL.Path, rewritten.URL.RawPath = "/"+rest, ""
	serveStatic(w, rewritten)
}

func (m *workspaceSet) libraryAPI(w http.ResponseWriter, r *http.Request, id, rest string) {
	switch rest {
	case "api/state":
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET")
			m.root.fail(w, 405, "usage", "method not allowed")
			return
		}
		result, err := m.libraryState(r.Context(), id)
		if err != nil {
			m.root.respondError(w, err)
			return
		}
		m.root.json(w, result)
	case "api/members":
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			m.root.fail(w, 405, "usage", "method not allowed")
			return
		}
		result, err := m.addMembers(w, r, id)
		if err != nil {
			m.root.respondError(w, err)
			return
		}
		m.root.json(w, result)
	case "api/upload":
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			m.root.fail(w, 405, "usage", "method not allowed")
			return
		}
		m.libraryUpload(w, r, id)
	default:
		m.root.fail(w, 404, "usage", "unknown library endpoint")
	}
}

// libraryAsset tags an asset with the workspace that owns it. Asset IDs are only
// unique inside one repository, so the owner is part of every identity here.
type libraryAsset struct {
	app.AssetView
	Workspace string `json:"workspace"`
}

type libraryMemberState struct {
	ID          string               `json:"id"`
	URL         string               `json:"url"`
	CSRF        string               `json:"csrf"`
	Location    *repository.Location `json:"location"`
	Initialized bool                 `json:"initialized"`
	Head        string               `json:"head"`
	BaseHead    string               `json:"base_head"`
	Assets      []app.AssetView      `json:"assets"`
	Changes     []domain.Change      `json:"changes"`
	Publication any                  `json:"publication"`
	LocalRead   any                  `json:"local_read"`
	Capacity    *app.Capacity        `json:"capacity"`
	Error       string               `json:"error,omitempty"`
}

// memberState reads one member without inventing a shared head or location. An
// unreadable member keeps its real error next to the others instead of looking
// like an empty repository.
func (m *workspaceSet) memberState(ctx context.Context, id string) (libraryMemberState, error) {
	member := libraryMemberState{ID: id, URL: workspaceURL(id)}
	server, err := m.open(ctx, id, false)
	if err != nil {
		member.Error = err.Error()
		return member, nil
	}
	member.CSRF = server.csrf
	raw, err := server.state(ctx)
	if err != nil {
		member.Error = err.Error()
		return member, nil
	}
	state, ok := raw.(map[string]any)
	if !ok {
		member.Error = "member state has an unexpected shape"
		return member, nil
	}
	if location, ok := state["location"].(*repository.Location); ok {
		member.Location = location
	}
	if initialized, ok := state["initialized"].(bool); ok {
		member.Initialized = initialized
	}
	// Heads are domain.RepositoryHead values, a named string type, so a plain
	// string assertion would silently report an empty HEAD.
	member.Head = headString(state["head"])
	member.BaseHead = headString(state["base_head"])
	member.Assets, _ = state["assets"].([]app.AssetView)
	member.Changes, _ = state["changes"].([]domain.Change)
	member.Publication = state["publication"]
	member.LocalRead = state["local_read"]
	if member.Initialized && member.Publication == nil {
		if reader, readErr := server.reader(ctx); readErr == nil {
			if capacity, capacityErr := server.service.PreviewPrepared(ctx, reader, nil, app.DefaultCapacityBytes); capacityErr == nil {
				member.Capacity = &capacity
			} else {
				member.Error = capacityErr.Error()
			}
		} else {
			member.Error = readErr.Error()
		}
	}
	return member, nil
}

func (m *workspaceSet) libraryState(ctx context.Context, id string) (any, error) {
	library, err := m.libraryByID(id)
	if err != nil {
		return nil, err
	}
	csrf, err := m.libraryCSRF(id)
	if err != nil {
		return nil, err
	}
	members := make([]libraryMemberState, 0, len(library.Workspaces))
	assets := make([]libraryAsset, 0)
	changes := make([]map[string]any, 0)
	for _, memberID := range library.Workspaces {
		member, err := m.memberState(ctx, memberID)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
		for _, view := range member.Assets {
			assets = append(assets, libraryAsset{AssetView: view, Workspace: memberID})
		}
		for _, change := range member.Changes {
			changes = append(changes, map[string]any{"workspace": memberID, "change": change})
		}
	}
	sort.SliceStable(assets, func(i, j int) bool {
		a, b := assets[i].Asset.CreatedAt, assets[j].Asset.CreatedAt
		if a.Equal(b) {
			if assets[i].Workspace == assets[j].Workspace {
				return assets[i].Asset.ID.String() < assets[j].Asset.ID.String()
			}
			return assets[i].Workspace < assets[j].Workspace
		}
		return a.Before(b)
	})
	return map[string]any{
		"kind": "library", "library": library, "csrf": csrf,
		"members": members, "assets": assets, "changes": changes,
		"url": libraryURL(id),
	}, nil
}

// handleLibraries serves the list and creation endpoints under the current view
// prefix, so a connection dialog works from both /w/ and /l/.
func (m *workspaceSet) handleLibraries(w http.ResponseWriter, r *http.Request, workspaceID string) (any, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		config, err := m.loadViews()
		if err != nil {
			return nil, err
		}
		active := map[string]any{"kind": config.Active.Kind, "id": config.Active.ID}
		return map[string]any{"current": workspaceID, "active": active, "libraries": config.Libraries}, nil
	case http.MethodPost:
		return m.createLibrary(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		return nil, usage("method not allowed")
	}
}

type repositoryChoice struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
}

type libraryRequestInput struct {
	Name         string             `json:"name"`
	Repositories []repositoryChoice `json:"repositories"`
}

// resolveRepositories validates every requested repository and branch before
// any configuration is written. The default branch is taken from GitHub, not
// guessed from the repository name.
func (m *workspaceSet) resolveRepositories(ctx context.Context, choices []repositoryChoice) ([]*Server, error) {
	if len(choices) == 0 {
		return nil, usage("select at least one repository")
	}
	resolved := make([]*Server, 0, len(choices))
	for _, choice := range choices {
		parts := strings.Split(choice.Repository, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, usage("repository must be OWNER/REPO")
		}
		token, _, err := m.root.credentials(ctx, false)
		if err != nil {
			return nil, err
		}
		client, err := githubClient(parts[0], parts[1], token)
		if err != nil {
			return nil, err
		}
		location, err := client.Discover(ctx, parts[0], parts[1], choice.Branch)
		if err != nil {
			return nil, err
		}
		server, err := m.selectLocation(ctx, location)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, server)
	}
	return resolved, nil
}

func (m *workspaceSet) createLibrary(w http.ResponseWriter, r *http.Request) (any, error) {
	var input libraryRequestInput
	if err := decodeJSON(w, r, &input); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, usage("library name must not be empty")
	}
	if len([]rune(name)) > 80 {
		return nil, usage("library name is too long")
	}
	ctx := r.Context()
	servers, err := m.resolveRepositories(ctx, input.Repositories)
	if err != nil {
		return nil, err
	}
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	library := libraryView{ID: id, Name: name}
	seen := make(map[string]bool, len(servers))
	for _, server := range servers {
		if seen[server.workspaceID] {
			continue
		}
		seen[server.workspaceID] = true
		library.Workspaces = append(library.Workspaces, server.workspaceID)
	}
	config, err := m.loadViews()
	if err != nil {
		return nil, err
	}
	config.Libraries = append(config.Libraries, library)
	config.Active = activeView{Kind: "library", ID: id}
	if err := m.saveViews(config); err != nil {
		return nil, err
	}
	members := make([]map[string]any, 0, len(library.Workspaces))
	for _, server := range servers {
		members = append(members, map[string]any{"id": server.workspaceID, "url": workspaceURL(server.workspaceID)})
	}
	return map[string]any{"library": library, "url": libraryURL(id), "members": members}, nil
}

// addMembers appends repositories to an existing library. Already-connected
// workspaces are kept in place, never duplicated, and no repository is created
// or initialized implicitly.
func (m *workspaceSet) addMembers(w http.ResponseWriter, r *http.Request, libraryID string) (any, error) {
	var input struct {
		Repositories []repositoryChoice `json:"repositories"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return nil, err
	}
	library, err := m.libraryByID(libraryID)
	if err != nil {
		return nil, err
	}
	servers, err := m.resolveRepositories(r.Context(), input.Repositories)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(library.Workspaces))
	for _, id := range library.Workspaces {
		present[id] = true
	}
	added := make([]string, 0, len(servers))
	for _, server := range servers {
		if present[server.workspaceID] {
			continue
		}
		present[server.workspaceID] = true
		library.Workspaces = append(library.Workspaces, server.workspaceID)
		added = append(added, server.workspaceID)
	}
	config, err := m.loadViews()
	if err != nil {
		return nil, err
	}
	replaced := false
	for i, existing := range config.Libraries {
		if existing.ID == libraryID {
			config.Libraries[i] = library
			replaced = true
			break
		}
	}
	if !replaced {
		return nil, os.ErrNotExist
	}
	if err := m.saveViews(config); err != nil {
		return nil, err
	}
	return map[string]any{"library": library, "added": added}, nil
}

// libraryCapacityLimit is the per-repository publication waterline used when a
// library spreads uploads across its members.
const libraryCapacityLimit = app.DefaultCapacityBytes

// libraryMaxImageBytes keeps uploads under jsDelivr's 20 MB per-file limit.
const libraryMaxImageBytes int64 = 20_000_000

// uploadTarget is the routing state of one member during a single batch.
type uploadTarget struct {
	server    *Server
	id        string
	reader    repository.Git
	projected int64
	paths     map[string]domain.ContentObject
	inputs    []app.PreparedInput
	indexes   []int
	err       error
}

func (t *uploadTarget) accepts(input app.PreparedInput) bool {
	objectPath := repository.ObjectPath(input.Asset)
	if existing, ok := t.paths[objectPath]; ok {
		return existing == input.Asset.Object
	}
	return t.projected+input.Asset.Object.Size <= libraryCapacityLimit
}

func (t *uploadTarget) assign(index int, input app.PreparedInput) {
	objectPath := repository.ObjectPath(input.Asset)
	if _, ok := t.paths[objectPath]; !ok {
		t.projected += input.Asset.Object.Size
	}
	t.paths[objectPath] = input.Asset.Object
	t.inputs = append(t.inputs, input)
	t.indexes = append(t.indexes, index)
}

// libraryUpload parses one batch, then routes each prepared image to the first
// member that fits it. Every file is ingested exactly once; nothing is staged
// until the whole batch has been validated.
func (m *workspaceSet) libraryUpload(w http.ResponseWriter, r *http.Request, libraryID string) {
	library, err := m.libraryByID(libraryID)
	if err != nil {
		m.root.respondError(w, err)
		return
	}
	ctx := r.Context()
	parsed, err := parseUploadBatch(w, r, m.root.service.Dir, libraryMaxImageBytes)
	if err != nil {
		m.root.respondError(w, err)
		return
	}
	defer parsed.discard()
	options, err := parsed.options()
	if err != nil {
		m.root.respondError(w, err)
		return
	}
	prepared := make([]app.PreparedInput, 0, len(parsed.files))
	for _, raw := range parsed.files {
		item, err := app.PrepareInput(raw.result, raw.name, options)
		if err != nil {
			for _, done := range prepared {
				os.Remove(done.Payload.TempPath)
			}
			m.root.respondError(w, err)
			return
		}
		prepared = append(prepared, item)
	}

	targets := make([]*uploadTarget, 0, len(library.Workspaces))
	for _, memberID := range library.Workspaces {
		server, err := m.open(ctx, memberID, false)
		if err != nil {
			targets = append(targets, &uploadTarget{id: memberID, err: err})
			continue
		}
		target := &uploadTarget{server: server, id: memberID, paths: map[string]domain.ContentObject{}}
		raw, err := server.state(ctx)
		if err != nil {
			target.err = err
			targets = append(targets, target)
			continue
		}
		state, _ := raw.(map[string]any)
		initialized, _ := state["initialized"].(bool)
		if !initialized {
			target.err = fmt.Errorf("%s is not initialized yet", memberID)
			targets = append(targets, target)
			continue
		}
		if state["publication"] != nil {
			target.err = fmt.Errorf("%s has an unresolved publication; sync it first", memberID)
			targets = append(targets, target)
			continue
		}
		reader, err := server.reader(ctx)
		if err != nil {
			target.err = err
			targets = append(targets, target)
			continue
		}
		target.reader = reader
		capacity, err := server.service.PreviewPrepared(ctx, reader, nil, libraryCapacityLimit)
		if err != nil {
			target.err = err
			targets = append(targets, target)
			continue
		}
		target.projected = capacity.ProjectedBytes
		for _, view := range assetViews(state) {
			target.paths[repository.ObjectPath(view.Asset)] = view.Asset.Object
		}
		targets = append(targets, target)
	}

	failed := make([]map[string]any, 0)
	for index, input := range prepared {
		var chosen *uploadTarget
		objectPath := repository.ObjectPath(input.Asset)
		// Reuse only an existing object at the same object path: identical bytes
		// under another namespace or directory is a separate asset, and moving
		// existing objects between members is out of scope.
		for _, target := range targets {
			if target.err != nil {
				continue
			}
			if existing, ok := target.paths[objectPath]; ok && existing == input.Asset.Object {
				chosen = target
				break
			}
		}
		if chosen == nil {
			for _, target := range targets {
				if target.err != nil || !target.accepts(input) {
					continue
				}
				chosen = target
				break
			}
		}
		if chosen == nil {
			failed = append(failed, map[string]any{"index": index, "code": "capacity_full", "message": capacityMessage(targets)})
			continue
		}
		chosen.assign(index, input)
	}

	results := make([]map[string]any, 0, len(prepared))
	for _, target := range targets {
		if len(target.inputs) == 0 {
			continue
		}
		if target.err != nil {
			for _, index := range target.indexes {
				_, code, message := errorDetail(target.err)
				failed = append(failed, map[string]any{"index": index, "code": code, "message": message})
			}
			continue
		}
		views, err := m.stageMember(ctx, target)
		if err != nil {
			for _, index := range target.indexes {
				_, code, message := errorDetail(err)
				failed = append(failed, map[string]any{"index": index, "workspace": target.id, "code": code, "message": message})
			}
			continue
		}
		byAsset := make(map[domain.AssetID]app.AssetView, len(views))
		for _, view := range views {
			byAsset[view.Asset.ID] = view
		}
		for i, index := range target.indexes {
			view := byAsset[target.inputs[i].Asset.ID]
			results = append(results, map[string]any{
				"index": index, "workspace": target.id,
				"asset_id": view.Asset.ID.String(), "asset": view, "state": view.State,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i]["index"].(int) < results[j]["index"].(int) })
	sort.Slice(failed, func(i, j int) bool { return failed[i]["index"].(int) < failed[j]["index"].(int) })
	status := http.StatusOK
	if len(failed) > 0 {
		status = http.StatusMultiStatus
	}
	m.root.jsonStatus(w, status, map[string]any{"results": results, "failed": failed})
}

// stageMember serializes one member's HTTP work before handing the batch to the
// service, which takes its own cross-process lock and revalidates capacity.
func (m *workspaceSet) stageMember(ctx context.Context, target *uploadTarget) ([]app.AssetView, error) {
	target.server.workMu.Lock()
	defer target.server.workMu.Unlock()
	return target.server.service.AddPrepared(ctx, target.inputs, app.PreparedOptions{Reader: target.reader, MaxBytes: libraryCapacityLimit})
}

func capacityMessage(targets []*uploadTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.err != nil {
			parts = append(parts, fmt.Sprintf("%s unavailable: %v", target.id, target.err))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s has %d bytes remaining", target.id, max64(libraryCapacityLimit-target.projected, 0)))
	}
	return "no member can hold this image (" + strings.Join(parts, "; ") + "); connect another repository"
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func headString(value any) string {
	if head, ok := value.(domain.RepositoryHead); ok {
		return string(head)
	}
	text, _ := value.(string)
	return text
}

func assetViews(state map[string]any) []app.AssetView {
	views, _ := state["assets"].([]app.AssetView)
	return views
}

// uploadFile is one accepted image part together with its ingested bytes. The
// payload temporary is owned by the caller until it is staged.
type uploadFile struct {
	name   string
	result image.Result
}

// uploadBatch is one validated multipart upload. Parsing never writes
// repository state: every part is read and ingested first, so a malformed part
// cannot leave earlier images staged.
type uploadBatch struct {
	files  []uploadFile
	fields map[string]string
	// dir owns every ingested temporary. Parsing must not delete it: the staged
	// payload is renamed out of this directory only when the batch is accepted.
	dir string
}

func (b uploadBatch) options() (app.AddOptions, error) {
	namespace, err := domain.ParseNamespace(b.fields["namespace"])
	if err != nil {
		return app.AddOptions{}, usage("%v", err)
	}
	options := app.AddOptions{Namespace: namespace, Directory: b.fields["directory"]}
	if title, ok := b.fields["title"]; ok {
		options.Title = &title
	}
	for _, field := range []struct {
		name   string
		target *[]string
	}{{"tags", &options.Tags}, {"collections", &options.Collections}} {
		if value, ok := b.fields[field.name]; ok {
			if err := json.Unmarshal([]byte(value), field.target); err != nil || *field.target == nil {
				return app.AddOptions{}, usage("%s must be a JSON array of strings", field.name)
			}
		}
	}
	return options, nil
}

// discard removes every temporary this batch created, including the directory
// that held them. It is safe after staging: staged payloads were renamed out.
func (b uploadBatch) discard() {
	if b.dir != "" {
		os.RemoveAll(b.dir)
	}
}

// parseUploadBatch reads a multipart upload into ingested temporaries. Callers
// own the batch and must either stage it or discard it. maxImage bounds one
// image; the shared 100-file and 500 MB batch limits always apply. The
// temporaries live under baseDir so staging can rename them without crossing a
// filesystem boundary.
func parseUploadBatch(w http.ResponseWriter, r *http.Request, baseDir string, maxImage int64) (uploadBatch, error) {
	batch := uploadBatch{fields: make(map[string]string)}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if r.ContentLength > maxUploadBytes {
		return batch, tooLarge("upload request exceeds 500 MB")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return batch, usage("multipart/form-data is required: %v", err)
	}
	dir, err := os.MkdirTemp(baseDir, "upload-")
	if err != nil {
		return batch, err
	}
	batch.dir = dir
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	var total int64
	for {
		if err = r.Context().Err(); err != nil {
			return batch, err
		}
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return batch, bodyError(e)
		}
		name := part.FormName()
		if part.FileName() != "" {
			if name != "files" {
				return batch, usage("file field must be named files")
			}
			if len(batch.files) >= maxUploadFiles {
				return batch, tooLarge("at most 100 files may be uploaded together")
			}
			file, e := os.CreateTemp(dir, "image-")
			if e != nil {
				return batch, e
			}
			n, e := io.Copy(file, io.LimitReader(contextReader{r.Context(), part}, maxImage+1))
			closeErr := file.Close()
			if e != nil {
				return batch, bodyError(e)
			}
			if closeErr != nil {
				return batch, closeErr
			}
			if n > maxImage {
				return batch, tooLarge(fmt.Sprintf("each image must be at most %d bytes", maxImage))
			}
			if n == 0 {
				return batch, usage("image is empty")
			}
			total += n
			if total > maxUploadBytes {
				return batch, tooLarge("upload request exceeds 500 MB")
			}
			// Ingest reads the part once from the temporary, so the same bytes are
			// never hashed twice and the batch limit above stays authoritative.
			payload, e := os.Open(file.Name())
			if e != nil {
				return batch, e
			}
			result, ingestErr := image.Ingest(r.Context(), contextReader{r.Context(), payload}, dir)
			closeErr = payload.Close()
			if ingestErr == nil {
				ingestErr = closeErr
			}
			if ingestErr != nil {
				return batch, usage("invalid image: %v", ingestErr)
			}
			batch.files = append(batch.files, uploadFile{name: part.FileName(), result: result})
			if e = part.Close(); e != nil {
				return batch, bodyError(e)
			}
			continue
		}
		switch name {
		case "namespace", "directory", "title", "tags", "collections":
		default:
			return batch, usage("unknown multipart field %q", name)
		}
		if _, exists := batch.fields[name]; exists {
			return batch, usage("duplicate multipart field %q", name)
		}
		value, e := io.ReadAll(io.LimitReader(contextReader{r.Context(), part}, maxJSONBytes+1))
		if e != nil {
			return batch, bodyError(e)
		}
		if len(value) > maxJSONBytes {
			return batch, tooLarge("multipart metadata exceeds 1 MiB")
		}
		batch.fields[name] = string(value)
		if e = part.Close(); e != nil {
			return batch, bodyError(e)
		}
	}
	if len(batch.files) == 0 {
		return batch, usage("select at least one image")
	}
	keep = true
	return batch, nil
}

func githubClient(owner, name, token string) (*github.Repository, error) {
	return github.New(repository.Location{Backend: "github", Owner: owner, Name: name, Branch: "main"}, token)
}

func randomToken() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}
