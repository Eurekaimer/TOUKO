package webview

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eurekaimer/TOUKO/internal/app"
)

func configureGitHubWorkspace(t *testing.T, dir string) {
	t.Helper()
	data := []byte(`{"backend":"github","owner":"owner","name":"gallery","branch":"main","private":false}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFakeClone(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "one.png"), []byte("gallery-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeCloneInfo(t *testing.T, body []byte) cloneInfo {
	t.Helper()
	var info cloneInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestCloneAPIMovesCheckoutAndReportsLocation(t *testing.T) {
	f := newFixture(t)
	configureGitHubWorkspace(t, f.service.Dir)
	clone := filepath.Join(f.service.Dir, "clone")
	writeFakeClone(t, clone)

	info := decodeCloneInfo(t, f.request("GET", "/api/clone", nil, 200).Body.Bytes())
	if info.Backend != "github" || !info.Available || info.Custom || filepath.Clean(info.Path) != filepath.Clean(clone) {
		t.Fatalf("unexpected initial clone info: %+v", info)
	}

	f.request("POST", "/api/clone", map[string]string{"path": "relative/clone"}, 400)
	destination := filepath.Join(f.root, "elsewhere", "clone")
	out := f.request("POST", "/api/clone", map[string]string{"path": destination}, 200)
	var moved app.CloneMoveResult
	if err := json.Unmarshal(out.Body.Bytes(), &moved); err != nil {
		t.Fatal(err)
	}
	if moved.Path != destination || !moved.Custom || !moved.Moved {
		t.Fatalf("unexpected move result: %+v", moved)
	}
	if _, err := os.Stat(filepath.Join(destination, ".git", "HEAD")); err != nil {
		t.Fatalf("git metadata not moved: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "images", "one.png")); err != nil || string(data) != "gallery-bytes" {
		t.Fatalf("worktree not moved: %q, %v", data, err)
	}
	if _, err := os.Stat(clone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old checkout still present: %v", err)
	}

	state := f.request("GET", "/api/state", nil, 200)
	var stateBody struct {
		LocalRead struct {
			Available bool   `json:"available"`
			Path      string `json:"path"`
		} `json:"local_read"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &stateBody); err != nil {
		t.Fatal(err)
	}
	if !stateBody.LocalRead.Available || stateBody.LocalRead.Path != destination {
		t.Fatalf("state did not report the moved checkout: %+v", stateBody.LocalRead)
	}

	out = f.request("POST", "/api/clone", map[string]string{"path": ""}, 200)
	if err := json.Unmarshal(out.Body.Bytes(), &moved); err != nil {
		t.Fatal(err)
	}
	if moved.Path != clone || moved.Custom || !moved.Moved {
		t.Fatalf("unexpected reset result: %+v", moved)
	}
	if data, err := os.ReadFile(filepath.Join(clone, "images", "one.png")); err != nil || string(data) != "gallery-bytes" {
		t.Fatalf("reset did not restore the checkout: %q, %v", data, err)
	}
}

func TestCloneAPINeverCreatesOrFetchesACheckout(t *testing.T) {
	f := newFixture(t)
	configureGitHubWorkspace(t, f.service.Dir)
	clone := filepath.Join(f.service.Dir, "clone")

	f.request("GET", "/api/state", nil, 200)
	info := decodeCloneInfo(t, f.request("GET", "/api/clone", nil, 200).Body.Bytes())
	if info.Available || filepath.Clean(info.Path) != filepath.Clean(clone) {
		t.Fatalf("unexpected clone info: %+v", info)
	}
	if _, err := os.Stat(clone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading the location created a checkout: %v", err)
	}

	destination := filepath.Join(f.root, "future", "clone")
	out := f.request("POST", "/api/clone", map[string]string{"path": destination}, 200)
	var moved app.CloneMoveResult
	if err := json.Unmarshal(out.Body.Bytes(), &moved); err != nil {
		t.Fatal(err)
	}
	if moved.Moved || moved.Path != destination {
		t.Fatalf("unexpected configuration-only result: %+v", moved)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuring a destination cloned into it: %v", err)
	}
	info = decodeCloneInfo(t, f.request("GET", "/api/clone", nil, 200).Body.Bytes())
	if !info.Custom || info.Available || info.Path != destination {
		t.Fatalf("configured location not reported: %+v", info)
	}
}

func TestCloneAPIRejectsNonEmptyDestinationAndLocalRepository(t *testing.T) {
	f := newFixture(t)
	configureGitHubWorkspace(t, f.service.Dir)
	clone := filepath.Join(f.service.Dir, "clone")
	writeFakeClone(t, clone)
	occupied := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/api/clone", map[string]string{"path": occupied}, 400)
	if _, err := os.Stat(filepath.Join(clone, "images", "one.png")); err != nil {
		t.Fatalf("refused move damaged the checkout: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(occupied, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("refused move changed the destination: %q, %v", data, err)
	}

	local := newFixture(t)
	local.setup()
	info := decodeCloneInfo(t, local.request("GET", "/api/clone", nil, 200).Body.Bytes())
	if info.Backend != "localgit" || filepath.Clean(info.Path) != filepath.Clean(filepath.Join(local.root, "gallery.git")) {
		t.Fatalf("unexpected local clone info: %+v", info)
	}
	local.request("POST", "/api/clone", map[string]string{"path": filepath.Join(local.root, "moved")}, 400)
	if _, err := os.Stat(filepath.Join(local.root, "gallery.git")); err != nil {
		t.Fatalf("local repository was moved: %v", err)
	}
}

func TestCloneAPIIsWorkspaceScoped(t *testing.T) {
	f := newFixture(t)
	memberID := strings.Repeat("a", 64)
	otherID := strings.Repeat("b", 64)
	memberDir := filepath.Join(f.service.Dir, "workspaces", memberID)
	otherDir := filepath.Join(f.service.Dir, "workspaces", otherID)
	if err := os.MkdirAll(memberDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(otherDir, "clone"), 0o700); err != nil {
		t.Fatal(err)
	}
	configureGitHubWorkspace(t, memberDir)
	writeFakeClone(t, filepath.Join(memberDir, "clone"))

	memberState := f.request("GET", "/w/"+memberID+"/api/state", nil, 200)
	var stateBody struct {
		CSRF      string `json:"csrf"`
		LocalRead struct {
			Path string `json:"path"`
		} `json:"local_read"`
	}
	if err := json.Unmarshal(memberState.Body.Bytes(), &stateBody); err != nil {
		t.Fatal(err)
	}
	if stateBody.CSRF == "" || filepath.Clean(stateBody.LocalRead.Path) != filepath.Join(memberDir, "clone") {
		t.Fatalf("unexpected member state: %+v", stateBody)
	}
	post := func(target string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"path": target})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", testOrigin+"/w/"+memberID+"/api/clone", bytes.NewReader(body))
		request.Header.Set("Origin", testOrigin)
		request.Header.Set("X-Touko-CSRF", stateBody.CSRF)
		request.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		f.server.ServeHTTP(out, request)
		return out
	}

	// Another workspace's default checkout is not a legal destination.
	if out := post(filepath.Join(otherDir, "clone")); out.Code != 400 {
		t.Fatalf("cross-workspace destination allowed: %d %s", out.Code, out.Body.String())
	}
	destination := filepath.Join(t.TempDir(), "member-clone")
	if out := post(destination); out.Code != 200 {
		t.Fatalf("scoped move failed: %d %s", out.Code, out.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(destination, "images", "one.png")); err != nil || string(data) != "gallery-bytes" {
		t.Fatalf("member checkout not moved: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(memberDir, "clone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member old checkout still present: %v", err)
	}
	// The root workspace has no repository and must not inherit the member move.
	f.request("GET", "/api/clone", nil, 400)
	if _, err := os.Stat(filepath.Join(f.service.Dir, "clone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member move created a root checkout: %v", err)
	}
}
