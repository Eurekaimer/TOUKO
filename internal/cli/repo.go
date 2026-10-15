package cli

import (
	"fmt"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/app"
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
	use := &cobra.Command{Use: "use --local PATH", Short: "Select an existing local Git repository without writing to it", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		if usePath == "" {
			return usage("repo use requires --local PATH")
		}
		if branch == "" {
			return usage("--branch must not be empty")
		}
		path, err := filepath.Abs(usePath)
		if err != nil {
			return err
		}
		if _, err := localgit.Open(cmd.Context(), path, branch); err != nil {
			return err
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		location := repository.Location{Backend: "localgit", Branch: branch, Path: path}
		if err := s.Select(cmd.Context(), location); err != nil {
			return err
		}
		if r.json {
			return r.encode(struct {
				Location repository.Location `json:"location"`
			}{location})
		}
		_, err = fmt.Fprintf(r.stdout, "Selected %s (branch %s)\n", path, branch)
		return err
	}}
	use.Flags().StringVar(&usePath, "local", "", "Existing local Git repository")
	use.Flags().StringVar(&branch, "branch", "main", "Git branch")
	repo.AddCommand(init, use)
	return repo
}

func (r *runtime) syncCommand() *cobra.Command {
	return &cobra.Command{Use: "sync", Short: "Rebuild the local gallery from the selected repository", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		git, err := r.backend(cmd.Context(), s)
		if err != nil {
			return err
		}
		result, err := s.Sync(cmd.Context(), git)
		if err != nil {
			return err
		}
		return r.printSync(result)
	}}
}

func (r *runtime) printSync(result app.SyncResult) error {
	if r.json {
		return r.encode(result)
	}
	_, err := fmt.Fprintf(r.stdout, "HEAD %s\nAssets %d\n", result.Head, result.AssetCount)
	return err
}
