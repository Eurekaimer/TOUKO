package webview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdimage "image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := "POST"
	switch path {
	case "/api/state", "/api/auth", "/api/repos", "/api/branches", "/api/workspaces":
		method = "GET"
	}
	var selector, action string
	if strings.HasPrefix(path, "/api/assets/") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/assets/"), "/")
		if len(parts) != 2 {
			s.fail(w, 404, "usage", "unknown API endpoint")
			return
		}
		selector, action = parts[0], parts[1]
		if _, err := domain.ParseAssetID(selector); err != nil {
			s.respondError(w, usage("asset selector must be a full lowercase UUID"))
			return
		}
		switch action {
		case "image", "url":
			method = "GET"
		case "edit":
		default:
			s.fail(w, 404, "usage", "unknown API endpoint")
			return
		}
	} else {
		switch path {
		case "/api/state", "/api/auth", "/api/repos", "/api/branches", "/api/workspaces", "/api/repo", "/api/init", "/api/sync", "/api/reindex", "/api/import", "/api/commit", "/api/reset", "/api/remove", "/api/upload", "/api/verify":
		default:
			s.fail(w, 404, "usage", "unknown API endpoint")
			return
		}
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		s.fail(w, 405, "usage", "method not allowed")
		return
	}
	if action == "image" {
		s.preview(w, r, selector)
		return
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	var result any
	var err error
	switch {
	case r.Context().Err() != nil:
		err = r.Context().Err()
	case action == "url":
		result, err = s.assetURL(r, selector)
	case action == "edit":
		var input struct {
			Title       *string   `json:"title"`
			Tags        *[]string `json:"tags"`
			Collections *[]string `json:"collections"`
		}
		if err = decodeJSON(w, r, &input); err == nil {
			err = s.service.Edit(r.Context(), selector, app.EditOptions{Title: input.Title, Tags: input.Tags, Collections: input.Collections})
		}
	case path == "/api/state":
		result, err = s.state(r.Context())
	case path == "/api/auth":
		result, err = s.authentication(r.Context())
	case path == "/api/repos":
		result, err = s.repositories(r.Context())
	case path == "/api/branches":
		names := r.URL.Query()["repository"]
		if len(names) != 1 {
			err = usage("exactly one repository=OWNER/REPO is required")
		} else {
			result, err = s.branches(r.Context(), names[0])
		}
	case path == "/api/workspaces":
		result, err = s.spaces.list(r.Context(), s.workspaceID)
	case path == "/api/repo":
		result, err = s.selectRepository(w, r)
	case path == "/api/upload":
		result, err = s.upload(w, r)
	case path == "/api/import":
		var input struct {
			Directory   string   `json:"directory"`
			Namespace   string   `json:"namespace"`
			Tags        []string `json:"tags"`
			Collections []string `json:"collections"`
		}
		if err = decodeJSON(w, r, &input); err == nil {
			var namespace domain.Namespace
			namespace, err = domain.ParseNamespace(input.Namespace)
			if err == nil {
				var git repository.Git
				git, err = s.reader(r.Context())
				if err == nil {
					result, err = s.service.ImportLegacy(r.Context(), git, app.ImportOptions{Directory: input.Directory, Namespace: namespace, Tags: input.Tags, Collections: input.Collections})
				}
			}
		}
	case path == "/api/sync":
		if err = decodeJSON(w, r, &struct{}{}); err == nil {
			result, err = s.syncRepository(r.Context())
		}
	case path == "/api/reindex":
		if err = decodeJSON(w, r, &struct{}{}); err == nil {
			var git repository.Git
			git, err = s.reader(r.Context())
			if err == nil {
				result, err = s.service.Reindex(r.Context(), git)
			}
		}
	case path == "/api/remove":
		var input struct {
			Selectors []string `json:"selectors"`
		}
		if err = decodeJSON(w, r, &input); err == nil {
			if len(input.Selectors) == 0 {
				err = usage("select at least one asset")
			} else {
				for _, id := range input.Selectors {
					if _, e := domain.ParseAssetID(id); e != nil {
						err = usage("asset selectors must be full lowercase UUIDs")
						break
					}
				}
				if err == nil {
					err = s.service.Remove(r.Context(), input.Selectors)
				}
			}
		}
	case path == "/api/commit":
		var input struct {
			Message string `json:"message"`
		}
		if err = decodeJSON(w, r, &input); err == nil {
			if strings.TrimSpace(input.Message) == "" {
				err = usage("commit message must not be empty")
			} else {
				var git repository.Git
				git, err = s.backend(r.Context())
				if err == nil {
					result, err = s.service.Commit(r.Context(), git, input.Message)
				}
			}
		}
	case path == "/api/reset":
		var input struct {
			Confirm bool `json:"confirm"`
		}
		if err = decodeJSON(w, r, &input); err == nil {
			if !input.Confirm {
				err = usage("reset requires confirm: true")
			} else {
				// Reset needs a backend only for publication recovery. Merely opening a
				// local repository would incorrectly make ordinary offline reset fail.
				err = s.service.Reset(r.Context(), &lazyGit{server: s})
			}
		}
	default:
		if err = decodeJSON(w, r, &struct{}{}); err == nil {
			var git repository.Git
			if path == "/api/verify" {
				git, err = s.reader(r.Context())
			} else {
				git, err = s.backend(r.Context())
			}
			if err == nil {
				switch path {
				case "/api/init":
					result, err = s.service.Initialize(r.Context(), git)
				case "/api/verify":
					result, err = s.service.Verify(r.Context(), git)
				}
			}
		}
	}
	if err != nil {
		s.respondError(w, err)
		return
	}
	if result == nil {
		result = map[string]bool{"ok": true}
	}
	s.json(w, result)
}

func (s *Server) location(ctx context.Context) (*repository.Location, error) {
	// Location's user-facing missing-selection error is intentionally not a
	// sentinel. Inspect only this known config path, not arbitrary request input.
	if _, err := os.Stat(filepath.Join(s.service.Dir, "config.json")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	location, err := s.service.Location(ctx)
	if err != nil {
		return nil, err
	}
	return &location, nil
}
func (s *Server) backend(ctx context.Context) (repository.Git, error) {
	location, err := s.location(ctx)
	if err != nil {
		return nil, err
	}
	if location == nil {
		return nil, usage("select a repository first")
	}
	switch location.Backend {
	case "localgit":
		return localgit.Open(ctx, location.Path, location.Branch)
	case "github":
		token, _, err := s.credentials(ctx, false)
		if err != nil {
			return nil, err
		}
		return github.New(*location, token)
	default:
		return nil, fmt.Errorf("unsupported configured backend")
	}
}
func (s *Server) state(ctx context.Context) (any, error) {
	unlock, err := s.service.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	location, err := s.location(ctx)
	if err != nil {
		return nil, err
	}
	st, assets, err := s.service.ReadView(ctx)
	if err != nil {
		return nil, err
	}
	changes := make([]domain.Change, 0, len(st.Changes))
	for _, change := range st.Changes {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].AssetID.String() < changes[j].AssetID.String() })
	var publication any
	if st.Publication != nil {
		publication = map[string]any{"base_head": st.Publication.BaseHead, "candidate_head": st.Publication.CandidateHead, "candidate_tree": st.Publication.CandidateTree}
	}
	available, clonePath := s.CloneStatus(ctx)
	return map[string]any{"csrf": s.csrf, "location": location, "initialized": st.Manifest != nil, "head": st.Head, "base_head": st.BaseHead, "assets": assets, "changes": changes, "publication": publication, "auth": s.authenticationState(), "local_read": map[string]any{"available": available, "path": clonePath}, "workspace": s.workspaceID}, nil
}
func (s *Server) selectRepository(w http.ResponseWriter, r *http.Request) (any, error) {
	var input struct {
		Backend    string `json:"backend"`
		Path       string `json:"path"`
		Branch     string `json:"branch"`
		Create     bool   `json:"create"`
		Repository string `json:"repository"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var location repository.Location
	var git repository.Git
	var err error
	switch input.Backend {
	case "localgit":
		if input.Repository != "" || !filepath.IsAbs(input.Path) {
			return nil, usage("local repository requires an absolute path and no GitHub repository")
		}
		if input.Branch == "" {
			input.Branch = "main"
		}
		location = repository.Location{Backend: "localgit", Path: filepath.Clean(input.Path), Branch: input.Branch}
		if err = location.Validate(); err != nil {
			return nil, usage("%v", err)
		}
		if input.Create {
			git, err = localgit.Init(ctx, location.Path, location.Branch)
		} else {
			git, err = localgit.Open(ctx, location.Path, location.Branch)
		}
	case "github":
		if input.Path != "" || input.Create {
			return nil, usage("GitHub selection does not create a repository or accept a local path")
		}
		parts := strings.Split(input.Repository, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, usage("repository must be OWNER/REPO")
		}
		token, _, credentialErr := s.credentials(ctx, false)
		if credentialErr != nil {
			return nil, credentialErr
		}
		client, clientErr := github.New(repository.Location{Backend: "github", Owner: parts[0], Name: parts[1], Branch: "main"}, token)
		if clientErr != nil {
			return nil, usage("%v", clientErr)
		}
		location, err = client.Discover(ctx, parts[0], parts[1], input.Branch)
	default:
		return nil, usage("backend must be localgit or github")
	}
	if err != nil {
		return nil, err
	}
	target, err := s.spaces.selectLocation(ctx, location)
	if err != nil {
		return nil, err
	}
	if input.Create {
		if _, err = target.service.Initialize(ctx, git); err != nil {
			return nil, err
		}
	}
	if err := s.spaces.remember(target.workspaceID); err != nil {
		return nil, err
	}
	return map[string]any{"url": workspaceURL(target.workspaceID), "location": location}, nil
}
func (s *Server) assetURL(r *http.Request, selector string) (any, error) {
	provider := r.URL.Query().Get("provider")
	if provider != "github" && provider != "jsdelivr" {
		return nil, usage("provider must be github or jsdelivr")
	}
	unlock, err := s.service.Lock(r.Context())
	if err != nil {
		return nil, err
	}
	defer unlock()
	location, err := s.location(r.Context())
	if err != nil {
		return nil, err
	}
	if location == nil {
		return nil, usage("select a repository first")
	}
	view, err := s.service.Show(r.Context(), selector)
	if err != nil {
		return nil, err
	}
	url, err := delivery.BuildURL(*location, repository.ObjectPath(view.Asset), delivery.Provider(provider))
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": url, "committed": view.State != "added"}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) (any, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if r.ContentLength > maxUploadBytes {
		return nil, tooLarge("upload request exceeds 500 MB")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, usage("multipart/form-data is required: %v", err)
	}
	dir, err := os.MkdirTemp(s.service.Dir, "upload-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var files []*os.File
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	inputs := make([]app.Input, 0)
	fields := make(map[string]string)
	for {
		if err = r.Context().Err(); err != nil {
			return nil, err
		}
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, bodyError(e)
		}
		name := part.FormName()
		if part.FileName() != "" {
			if name != "files" {
				return nil, usage("file field must be named files")
			}
			if len(inputs) >= maxUploadFiles {
				return nil, tooLarge("at most 100 files may be uploaded together")
			}
			file, e := os.CreateTemp(dir, "image-")
			if e != nil {
				return nil, e
			}
			files = append(files, file)
			n, e := io.Copy(file, io.LimitReader(contextReader{r.Context(), part}, maxImageBytes+1))
			if e != nil {
				return nil, bodyError(e)
			}
			if n > maxImageBytes {
				return nil, tooLarge("each image must be at most 100 MB")
			}
			if _, e = file.Seek(0, io.SeekStart); e != nil {
				return nil, e
			}
			if n == 0 {
				return nil, usage("image is empty")
			}
			// Classify malformed image input before entering the application, whose
			// ingestion errors also include filesystem failures. Only headers are read.
			_, format, decodeErr := stdimage.DecodeConfig(contextReader{r.Context(), file})
			if err = r.Context().Err(); err != nil {
				return nil, err
			}
			if decodeErr != nil {
				return nil, usage("invalid image header: %v", decodeErr)
			}
			if format != "png" && format != "jpeg" && format != "gif" {
				return nil, usage("only PNG, JPEG and GIF images are supported")
			}
			if _, e = file.Seek(0, io.SeekStart); e != nil {
				return nil, e
			}
			inputs = append(inputs, app.Input{Reader: contextReader{r.Context(), file}, Name: part.FileName()})
		} else {
			switch name {
			case "namespace", "directory", "title", "tags", "collections":
			default:
				return nil, usage("unknown multipart field %q", name)
			}
			if _, exists := fields[name]; exists {
				return nil, usage("duplicate multipart field %q", name)
			}
			value, e := io.ReadAll(io.LimitReader(contextReader{r.Context(), part}, maxJSONBytes+1))
			if e != nil {
				return nil, bodyError(e)
			}
			if len(value) > maxJSONBytes {
				return nil, tooLarge("multipart metadata exceeds 1 MiB")
			}
			fields[name] = string(value)
		}
		if e = part.Close(); e != nil {
			return nil, bodyError(e)
		}
	}
	if len(inputs) == 0 {
		return nil, usage("select at least one image")
	}
	namespace, err := domain.ParseNamespace(fields["namespace"])
	if err != nil {
		return nil, usage("%v", err)
	}
	options := app.AddOptions{Namespace: namespace, Directory: fields["directory"]}
	if title, ok := fields["title"]; ok {
		options.Title = &title
	}
	for _, field := range []struct {
		name   string
		target *[]string
	}{{"tags", &options.Tags}, {"collections", &options.Collections}} {
		if value, ok := fields[field.name]; ok {
			if e := json.Unmarshal([]byte(value), field.target); e != nil || *field.target == nil {
				return nil, usage("%s must be a JSON array of strings", field.name)
			}
		}
	}
	assets, err := s.service.Add(r.Context(), inputs, options)
	if err != nil {
		return nil, err
	}
	return map[string]any{"assets": assets}, nil
}

// lazyGit opens the backend only when recovery actually needs it. Every method
// resolves the backend, so ordinary reset remains offline without relying on a
// particular call order in the publication coordinator.
type lazyGit struct {
	server *Server
	git    repository.Git
}

func (g *lazyGit) resolve(ctx context.Context) (repository.Git, error) {
	if g.git != nil {
		return g.git, nil
	}
	git, err := g.server.backend(ctx)
	if err == nil {
		g.git = git
	}
	return git, err
}
func (g *lazyGit) Head(ctx context.Context) (domain.RepositoryHead, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return "", e
	}
	return r.Head(ctx)
}
func (g *lazyGit) Commit(ctx context.Context, h domain.RepositoryHead) (repository.GitCommit, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return repository.GitCommit{}, e
	}
	return r.Commit(ctx, h)
}
func (g *lazyGit) Tree(ctx context.Context, id repository.TreeID) ([]repository.GitEntry, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return nil, e
	}
	return r.Tree(ctx, id)
}
func (g *lazyGit) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return nil, e
	}
	return r.OpenBlob(ctx, id)
}
func (g *lazyGit) IsAncestor(ctx context.Context, a, b domain.RepositoryHead) (bool, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return false, e
	}
	return r.IsAncestor(ctx, a, b)
}
func (g *lazyGit) CreateBlob(ctx context.Context, source io.Reader, size int64) (repository.BlobID, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return "", e
	}
	return r.CreateBlob(ctx, source, size)
}
func (g *lazyGit) CreateTree(ctx context.Context, base repository.TreeID, changes []repository.TreeChange) (repository.TreeID, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return "", e
	}
	return r.CreateTree(ctx, base, changes)
}
func (g *lazyGit) CreateCommit(ctx context.Context, tree repository.TreeID, parent domain.RepositoryHead, message string) (domain.RepositoryHead, error) {
	r, e := g.resolve(ctx)
	if e != nil {
		return "", e
	}
	return r.CreateCommit(ctx, tree, parent, message)
}
func (g *lazyGit) UpdateRef(ctx context.Context, a, b domain.RepositoryHead) error {
	r, e := g.resolve(ctx)
	if e != nil {
		return e
	}
	return r.UpdateRef(ctx, a, b)
}
