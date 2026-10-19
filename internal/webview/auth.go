package webview

import (
	"context"
	"errors"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

// authOwner returns the one Server that owns process-wide credentials and the
// login session. Workspaces never cache an independent gh identity.
func (s *Server) authOwner() *Server {
	if s.spaces != nil && s.spaces.root != nil {
		return s.spaces.root
	}
	return s
}

// Credentials remain in this server process. A deliberate identity check refreshes
// the cached gh login; gallery state and original-image hits never consult GitHub.
func (s *Server) credentials(ctx context.Context, refresh bool) (string, string, error) {
	owner := s.authOwner()
	owner.authMu.Lock()
	defer owner.authMu.Unlock()
	return owner.resolveCredentials(ctx, refresh)
}

func (s *Server) resolveCredentials(ctx context.Context, refresh bool) (string, string, error) {
	if s.authResolved && !refresh {
		return s.authToken, s.authSource, nil
	}
	token, source := s.token, "github_token"
	if token == "" {
		var err error
		if s.authResolve != nil {
			token, source, err = s.authResolve(ctx)
		} else {
			token, source, err = credential.Resolve(ctx)
		}
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

// refreshAuth is called only after gh reports a completed device flow. It
// re-reads the credential and clears the old display name so the next
// /api/auth response describes the newly signed-in account.
func (s *Server) refreshAuth(ctx context.Context) error {
	owner := s.authOwner()
	owner.authMu.Lock()
	defer owner.authMu.Unlock()
	token, _, err := owner.resolveCredentials(ctx, true)
	if err != nil {
		return err
	}
	// gh exiting zero is not enough: if no credential was stored (for example a
	// keyring failure), the session must fail instead of claiming success.
	if token == "" {
		return errors.New("GitHub CLI did not store a credential")
	}
	owner.authLogin = ""
	return nil
}

func (s *Server) authenticationState() map[string]any {
	owner := s.authOwner()
	owner.authMu.Lock()
	defer owner.authMu.Unlock()
	return map[string]any{"configured": owner.token != "" || owner.authToken != "", "source": owner.authSource, "login": owner.authLogin}
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
	owner := s.authOwner()
	owner.authMu.Lock()
	owner.authLogin = login
	owner.authMu.Unlock()
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
