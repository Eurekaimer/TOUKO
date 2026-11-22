package webview

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// cloneInfo is the secret-free working-copy state for one workspace. It never
// reports credentials and never touches the network: paths are resolved and
// stat'ed only.
type cloneInfo struct {
	Backend     string `json:"backend"`
	Path        string `json:"path"`
	DefaultPath string `json:"default_path"`
	Custom      bool   `json:"custom"`
	Available   bool   `json:"available"`
}

func (s *Server) cloneInfo(ctx context.Context) (cloneInfo, error) {
	location, err := s.location(ctx)
	if err != nil {
		return cloneInfo{}, err
	}
	if location == nil {
		return cloneInfo{}, usage("select a repository first")
	}
	if location.Backend == "localgit" {
		info, statErr := os.Stat(location.Path)
		return cloneInfo{
			Backend:   "localgit",
			Path:      location.Path,
			Available: statErr == nil && info.IsDir(),
		}, nil
	}
	dir, err := s.service.CloneDir()
	if err != nil {
		return cloneInfo{}, err
	}
	defaultPath := filepath.Clean(repository.CloneDir(s.service.Dir))
	_, statErr := os.Stat(filepath.Join(dir, ".git"))
	return cloneInfo{
		Backend:     "github",
		Path:        dir,
		DefaultPath: defaultPath,
		Custom:      filepath.Clean(dir) != defaultPath,
		Available:   statErr == nil,
	}, nil
}

// cloneAPI serves the Settings section for the working copy. Moving and
// resetting are workspace-scoped: the caller's workspace URL decides whose
// configuration is read and rewritten.
func (s *Server) cloneAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info, err := s.cloneInfo(r.Context())
		if err != nil {
			s.respondError(w, err)
			return
		}
		s.json(w, info)
	case http.MethodPost:
		s.workMu.Lock()
		defer s.workMu.Unlock()
		// Hold the clone gate for the whole migration. ensureClone and updateClone
		// take it too, so no preview starts a clone into a directory this request
		// is about to rename, and the next request sees an empty one-time cache.
		s.clone.mu.Lock()
		defer s.clone.mu.Unlock()
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			s.respondError(w, err)
			return
		}
		if err := s.validateCloneDestination(r.Context(), input.Path); err != nil {
			s.respondError(w, err)
			return
		}
		result, err := s.service.MoveClone(r.Context(), input.Path)
		if err != nil {
			s.respondError(w, err)
			return
		}
		s.clone.done = nil
		s.json(w, result)
	default:
		w.Header().Set("Allow", "GET, POST")
		s.fail(w, http.StatusMethodNotAllowed, "usage", "method not allowed")
	}
}

// validateCloneDestination rejects a destination that another workspace already
// owns. Without this, two workspaces could point at one checkout and read each
// other's repository.
func (s *Server) validateCloneDestination(ctx context.Context, destination string) error {
	location, err := s.location(ctx)
	if err != nil {
		return err
	}
	if location == nil {
		return usage("select a repository first")
	}
	if location.Backend != "github" {
		return usage("本地仓库直接读取原路径，不能迁移读图副本。")
	}
	target := strings.TrimSpace(destination)
	if target == "" {
		return nil // Restores this workspace's default location.
	}
	if !filepath.IsAbs(target) {
		return usage("目标位置必须是绝对路径。")
	}
	target = filepath.Clean(target)
	stateDirs, trees := s.cloneReservations()
	for _, dir := range stateDirs {
		if filepath.Clean(target) == dir {
			return usage("不能把副本放在其他工作区的状态目录：%s", dir)
		}
	}
	for _, tree := range trees {
		if pathsOverlap(target, tree) {
			return usage("该路径与其他工作区的副本或仓库重叠：%s", tree)
		}
	}
	return nil
}

// cloneReservations lists every other workspace's state directory and every
// checkout or local repository that already belongs to one. A custom directory
// beside the state directories is allowed; sharing a checkout is not.
func (s *Server) cloneReservations() (stateDirs, trees []string) {
	if s.spaces == nil {
		return nil, nil
	}
	rootDir := s.spaces.root.service.Dir
	if filepath.Clean(rootDir) != filepath.Clean(s.service.Dir) {
		stateDirs = append(stateDirs, filepath.Clean(rootDir))
	}
	children, err := os.ReadDir(filepath.Join(rootDir, "workspaces"))
	if err == nil {
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			dir := filepath.Join(rootDir, "workspaces", child.Name())
			if filepath.Clean(dir) == filepath.Clean(s.service.Dir) {
				continue
			}
			stateDirs = append(stateDirs, filepath.Clean(dir))
		}
	}
	trees = make([]string, 0, len(stateDirs)*2)
	for _, dir := range stateDirs {
		if clone, err := app.CloneDirIn(dir); err == nil {
			trees = append(trees, filepath.Clean(clone))
		}
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			continue
		}
		var location repository.Location
		if json.Unmarshal(data, &location) == nil && location.Backend == "localgit" && location.Path != "" {
			trees = append(trees, filepath.Clean(location.Path))
		}
	}
	return stateDirs, trees
}

func pathsOverlap(a, b string) bool { return pathInside(a, b) || pathInside(b, a) }

func pathInside(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
