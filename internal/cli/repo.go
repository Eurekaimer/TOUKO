package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/spf13/cobra"
)

func (r *runtime) repoCommand() *cobra.Command {
	repo := &cobra.Command{Use: "repo", Short: "Initialize or select a repository", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() }}
	var initPath string
	init := &cobra.Command{Use: "init", Short: "Initialize TOUKO metadata, optionally creating a local bare Git repository", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("local") && initPath == "" {
			return usage("--local requires a path")
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		var git repository.Git
		if initPath != "" {
			path, err := filepath.Abs(initPath)
			if err != nil {
				return err
			}
			git, err = localgit.Init(cmd.Context(), path, "main")
			if err != nil {
				return err
			}
			if err := s.Select(cmd.Context(), repository.Location{Backend: "localgit", Branch: "main", Path: path}); err != nil {
				return err
			}
		} else {
			git, err = r.backend(cmd.Context(), s)
			if err != nil {
				return err
			}
		}
		result, err := s.Initialize(cmd.Context(), git)
		if err != nil {
			return err
		}
		return r.printSync(result)
	}}
	init.Flags().StringVar(&initPath, "local", "", "Create a local bare Git repository at PATH")
	var usePath, branch string
	use := &cobra.Command{Use: "use [OWNER/REPO | --local PATH]", Short: "Select an existing repository without remote writes", Args: arguments(cobra.MaximumNArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		local := cmd.Flags().Changed("local")
		if local && (usePath == "" || len(args) != 0) || !local && len(args) != 1 {
			return usage("choose --local PATH or OWNER/REPO")
		}
		if cmd.Flags().Changed("branch") && branch == "" {
			return usage("--branch must not be empty")
		}
		var location repository.Location
		if local {
			if branch == "" {
				branch = "main"
			}
			path, err := filepath.Abs(usePath)
			if err != nil {
				return err
			}
			if _, err = localgit.Open(cmd.Context(), path, branch); err != nil {
				return err
			}
			location = repository.Location{Backend: "localgit", Branch: branch, Path: path}
		} else {
			parts := strings.Split(args[0], "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return usage("repository must be OWNER/REPO")
			}
			client, err := github.New(repository.Location{Backend: "github", Owner: parts[0], Name: parts[1], Branch: "main"}, os.Getenv("GITHUB_TOKEN"))
			if err != nil {
				return &usageError{err}
			}
			location, err = client.Discover(cmd.Context(), parts[0], parts[1], branch)
			if err != nil {
				return err
			}
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		if err = s.Select(cmd.Context(), location); err != nil {
			return err
		}
		if r.json {
			return r.encode(struct {
				Location repository.Location `json:"location"`
			}{location})
		}
		name := location.Path
		if location.Backend == "github" {
			name = location.Owner + "/" + location.Name
		}
		_, err = fmt.Fprintf(r.stdout, "Selected %s (branch %s)\n", name, location.Branch)
		return err
	}}
	use.Flags().StringVar(&usePath, "local", "", "Existing local Git repository")
	use.Flags().StringVar(&branch, "branch", "", "Git branch (local: main; GitHub: repository default)")
	repo.AddCommand(init, use)
	return repo
}

func (r *runtime) syncCommand() *cobra.Command {
	return &cobra.Command{Use: "sync", Short: "Rebuild the local gallery from the selected repository", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		git, err := r.reader(cmd.Context(), s)
		if err != nil {
			return err
		}
		result, err := s.Sync(cmd.Context(), git)
		if err != nil {
			return err
		}
		// `sync` is the explicit "talk to the remote" action, so it is where the
		// local working copy is refreshed. Previews never fetch, which is what
		// keeps them working with no connection.
		location, locationErr := s.Location(cmd.Context())
		if locationErr == nil && location.Backend == "github" {
			if err := r.syncClone(cmd.Context(), s, location); err != nil {
				return err
			}
		}
		return r.printSync(result)
	}}
}

// syncClone brings the local working copy in step with the branch that was just
// read. A repository used before the clone existed gets one created here.
func (r *runtime) syncClone(ctx context.Context, s *app.Service, location repository.Location) error {
	dir := filepath.Join(s.Dir, "clone")
	remote := "https://github.com/" + location.Owner + "/" + location.Name + ".git"
	if err := localgit.Clone(ctx, remote, dir, location.Branch, os.Getenv("GITHUB_TOKEN")); err != nil {
		return err
	}
	return localgit.Update(ctx, dir, location.Branch, os.Getenv("GITHUB_TOKEN"))
}

func (r *runtime) printSync(result app.SyncResult) error {
	if r.json {
		return r.encode(result)
	}
	_, err := fmt.Fprintf(r.stdout, "HEAD %s\nAssets %d\n", result.Head, result.AssetCount)
	return err
}
