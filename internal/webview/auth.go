package webview

import (
	"context"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// Credentials remain in this server process. A deliberate identity check refreshes
// the cached gh login; gallery state and original-image hits never consult GitHub.
func (s *Server) credentials(ctx context.Context, refresh bool) (string, string, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authResolved && !refresh {
		return s.authToken, s.authSource, nil
	}
	token, source := s.token, "github_token"
	var err error
	if token == "" {
		token, source, err = credential.Resolve(ctx)
		if err != nil {
			return "", "", err
		}
	}
	if token != s.authToken {
		s.authLogin = ""
	}
	s.authToken, s.authSource, s.authResolved = token, source, true
	return token, source, nil
}

func (s *Server) authenticationState() map[string]any {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return map[string]any{"configured": s.token != "" || s.authToken != "", "source": s.authSource, "login": s.authLogin}
}

func (s *Server) authentication(ctx context.Context) (any, error) {
	token, source, err := s.credentials(ctx, true)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return map[string]any{"configured": false, "source": source, "login": ""}, nil
	}
	client, err := github.New(repository.Location{Backend: "github", Owner: "touko", Name: "auth", Branch: "main"}, token)
	if err != nil {
		return nil, err
	}
	login, err := client.User(ctx)
	if err != nil {
		return nil, err
	}
	s.authMu.Lock()
	s.authLogin = login
	s.authMu.Unlock()
	return map[string]any{"configured": true, "source": source, "login": login}, nil
}

func (s *Server) repositories(ctx context.Context) (any, error) {
	token, _, err := s.credentials(ctx, false)
	if err != nil {
		return nil, err
	}
	client, err := github.New(repository.Location{Backend: "github", Owner: "touko", Name: "auth", Branch: "main"}, token)
	if err != nil {
		return nil, err
	}
	entries, err := client.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"repositories": entries}, nil
}

func (s *Server) branches(ctx context.Context, name string) (any, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, usage("repository must be OWNER/REPO")
	}
	token, _, err := s.credentials(ctx, false)
	if err != nil {
		return nil, err
	}
	client, err := github.New(repository.Location{Backend: "github", Owner: parts[0], Name: parts[1], Branch: "main"}, token)
	if err != nil {
		return nil, usage("%v", err)
	}
	entries, err := client.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"branches": entries}, nil
}
