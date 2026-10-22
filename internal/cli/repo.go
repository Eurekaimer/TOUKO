package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/domain"
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
			token, _, err := credential.Resolve(cmd.Context())
			if err != nil {
				return err
			}
			client, err := github.New(repository.Location{Backend: "github", Owner: parts[0], Name: parts[1], Branch: "main"}, token)
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
	list := &cobra.Command{Use: "list", Short: "List repositories accessible to the current GitHub login", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		token, _, err := credential.Resolve(cmd.Context())
		if err != nil {
			return err
		}
		client, err := github.New(repository.Location{Backend: "github", Owner: "account", Name: "repositories", Branch: "main"}, token)
		if err != nil {
			return err
		}
		repositories, err := client.ListRepositories(cmd.Context())
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(struct {
				Repositories []github.RepositoryInfo `json:"repositories"`
			}{repositories})
		}
		for _, info := range repositories {
			if _, err := fmt.Fprintf(r.stdout, "%s\t%s\tprivate=%t\tcan_push=%t\n", info.FullName, info.DefaultBranch, info.Private, info.CanPush); err != nil {
				return err
			}
		}
		return nil
	}}
	repo.AddCommand(init, use, list)
	return repo
}

func (r *runtime) syncCommand() *cobra.Command {
	return &cobra.Command{Use: "sync", Short: "Rebuild the local gallery from the selected repository", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		location, err := s.Location(cmd.Context())
		if err != nil {
			return err
		}
		result, err := s.SyncWith(cmd.Context(), func(ctx context.Context, recovering bool) (repository.Git, error) {
			if location.Backend == "github" {
				if recovering {
					// A shallow clone cannot establish publication or read an
					// unpublished candidate; recovery must use the API.
					return r.backend(ctx, s)
				}
				if err := r.syncClone(ctx, s, location); err != nil {
					return nil, err
				}
			}
			return r.reader(ctx, s)
		})
		if err != nil {
			return err
		}
		return r.printSync(result)
	}}
}

// syncClone explicitly refreshes the TOUKO-managed checkout before indexing.
func (r *runtime) syncClone(ctx context.Context, s *app.Service, location repository.Location) error {
	token, _, err := credential.Resolve(ctx)
	if err != nil {
		return err
	}
	dir := repository.CloneDir(s.Dir)
	if err := localgit.Clone(ctx, location.RemoteURL(), dir, location.Branch, token); err != nil {
		return err
	}
	return localgit.Update(ctx, dir, location.Branch, token)
}

func (r *runtime) reindexCommand() *cobra.Command {
	return &cobra.Command{Use: "reindex", Short: "Rebuild the committed cache without discarding unpublished changes", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := app.StateDir(r.stateDir)
		if err != nil {
			return err
		}
		// Location needs only config.json: do not open a possibly corrupt cache.
		s := &app.Service{Dir: dir}
		location, err := s.Location(cmd.Context())
		if err != nil {
			return err
		}
		if location.Backend == "github" {
			if err := r.syncClone(cmd.Context(), s, location); err != nil {
				return err
			}
		}
		git, err := r.reader(cmd.Context(), s)
		if err != nil {
			return err
		}
		result, err := app.Reindex(cmd.Context(), dir, git)
		if err != nil {
			return err
		}
		return r.printSync(result)
	}}
}

func (r *runtime) importCommand() *cobra.Command {
	var directory, namespace string
	var tags, collections []string
	cmd := &cobra.Command{Use: "import", Short: "Stage existing Git images in place, preserving their paths", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		git, err := r.reader(cmd.Context(), s)
		if err != nil {
			return err
		}
		result, err := s.ImportLegacy(cmd.Context(), git, app.ImportOptions{Directory: directory, Namespace: domain.Namespace(namespace), Tags: tags, Collections: collections})
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(result)
		}
		_, err = fmt.Fprintf(r.stdout, "Imported %d; skipped %d. Changes are staged; run commit to publish metadata.\n", result.Imported, result.Skipped)
		return err
	}}
	cmd.Flags().StringVar(&directory, "directory", "", "Only import images under this repository directory")
	cmd.Flags().StringVar(&namespace, "namespace", "default", "Namespace for imported images")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Tag for imported images (repeatable)")
	cmd.Flags().StringArrayVar(&collections, "collection", nil, "Collection for imported images (repeatable)")
	return cmd
}

func (r *runtime) printSync(result app.SyncResult) error {
	if r.json {
		return r.encode(result)
	}
	_, err := fmt.Fprintf(r.stdout, "HEAD %s\nAssets %d\n", result.Head, result.AssetCount)
	if err != nil {
		return err
	}
	return r.printCloudflare(result.Cloudflare)
}
