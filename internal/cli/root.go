package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/localgit"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/spf13/cobra"
)

type runtime struct {
	stateDir string
	json     bool
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	opened   *app.Service
	started  bool
}

type usageError struct{ err error }

func (e *usageError) Error() string          { return e.err.Error() }
func (e *usageError) Unwrap() error          { return e.err }
func usage(format string, args ...any) error { return &usageError{fmt.Errorf(format, args...)} }

func arguments(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return &usageError{err}
		}
		return nil
	}
}

// Run executes one invocation without using process-global arguments or streams.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	r := &runtime{stdin: stdin, stdout: stdout, stderr: stderr}
	root := r.command()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if r.opened != nil {
		if closeErr := r.opened.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		return 0
	}
	var badUsage *usageError
	if !r.started && !errors.As(err, &badUsage) {
		err = &usageError{err}
	}
	code, exit := errorCode(err)
	if r.json || requestsJSON(args) {
		_ = json.NewEncoder(stderr).Encode(struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}{Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{code, err.Error()}})
	} else {
		_, _ = fmt.Fprintln(stderr, "Error:", err)
	}
	return exit
}

// Parsing may stop before a trailing --json on an invalid command line.
func requestsJSON(args []string) bool {
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		switch arg {
		case "--json", "--json=true":
			value = true
		case "--json=false":
			value = false
		}
	}
	return value
}

func errorCode(err error) (string, int) {
	var badUsage *usageError
	if errors.As(err, &badUsage) {
		return "usage", 2
	}
	for _, entry := range []struct {
		err  error
		code string
	}{
		{domain.ErrAssetNotFound, "asset_not_found"},
		{domain.ErrAmbiguousSelector, "ambiguous_selector"},
		{domain.ErrConflict, "conflict"},
		{domain.ErrRepositoryCorrupt, "repository_corrupt"},
		{domain.ErrHashMismatch, "hash_mismatch"},
		{domain.ErrUnauthorized, "unauthorized"},
		{domain.ErrRepositoryUninitialized, "repository_uninitialized"},
		{domain.ErrBusy, "busy"},
		{domain.ErrDeliveryUnavailable, "delivery_unavailable"},
	} {
		if errors.Is(err, entry.err) {
			if entry.err == domain.ErrConflict {
				return entry.code, 3
			}
			return entry.code, 1
		}
	}
	var api interface{ APIStatus() int }
	if errors.As(err, &api) {
		return "api_error", 1
	}
	return "io_error", 1
}

func (r *runtime) service(ctx context.Context) (*app.Service, error) {
	if r.opened != nil {
		return r.opened, nil
	}
	dir, err := app.StateDir(r.stateDir)
	if err != nil {
		return nil, err
	}
	s, err := app.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	r.opened = s
	return s, nil
}

func (r *runtime) backend(ctx context.Context, s *app.Service) (repository.Git, error) {
	location, err := s.Location(ctx)
	if err != nil {
		return nil, err
	}
	switch location.Backend {
	case "localgit":
		return localgit.Open(ctx, location.Path, location.Branch)
	case "github":
		token, _, err := credential.Resolve(ctx)
		if err != nil {
			return nil, err
		}
		return github.New(location, token)
	default:
		return nil, fmt.Errorf("unsupported backend %q", location.Backend)
	}
}

// reader is the Git port for operations that only read. A GitHub repository is
// read from its local checkout, so listing metadata, verifying bytes and finding
// relationships cost no API calls and work with no connection. The checkout is
// created on first use.
func (r *runtime) reader(ctx context.Context, s *app.Service) (repository.Git, error) {
	location, err := s.Location(ctx)
	if err != nil {
		return nil, err
	}
	if location.Backend != "github" {
		return r.backend(ctx, s)
	}
	token, _, err := credential.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	return localgit.WorkingCopy(ctx, location.RemoteURL(), repository.CloneDir(s.Dir), location.Branch, token)
}

func (r *runtime) encode(v any) error { return json.NewEncoder(r.stdout).Encode(v) }

func (r *runtime) command() *cobra.Command {
	root := &cobra.Command{
		Use: "touko", Short: "Content-addressed image repositories",
		SilenceUsage: true, SilenceErrors: true,
		Args:             arguments(cobra.NoArgs),
		PersistentPreRun: func(cmd *cobra.Command, args []string) { r.started = true },
		RunE:             func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return &usageError{err} })
	root.PersistentFlags().StringVar(&r.stateDir, "state-dir", "", "Local state directory")
	root.PersistentFlags().BoolVar(&r.json, "json", false, "Write JSON output")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(r.repoCommand(), r.syncCommand(), r.reindexCommand(), r.importCommand(), r.addCommand(), r.editCommand(), r.removeCommand(), r.statusCommand(), r.resetCommand(), r.listCommand(), r.findCommand(), r.showCommand(), r.commitCommand(), r.verifyCommand(), r.authCommand(), r.urlCommand(), r.serveCommand())
	return root
}

func (r *runtime) printAssets(assets []app.AssetView) error {
	if assets == nil {
		assets = []app.AssetView{}
	}
	if r.json {
		return r.encode(struct {
			Assets []app.AssetView `json:"assets"`
		}{assets})
	}
	for _, v := range assets {
		if _, err := fmt.Fprintf(r.stdout, "%s\t%s\t%s\t%s\t%s\n", v.Asset.ID, v.State, v.Asset.Namespace, v.Asset.Title, v.Asset.Object.Hash); err != nil {
			return err
		}
	}
	return nil
}

func (r *runtime) statusCommand() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show local staged changes", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		st, err := s.Store.Load(cmd.Context())
		if err != nil {
			return err
		}
		changes := make([]domain.Change, 0, len(st.Changes))
		for _, change := range st.Changes {
			changes = append(changes, change)
		}
		sort.Slice(changes, func(i, j int) bool { return changes[i].AssetID.String() < changes[j].AssetID.String() })
		if r.json {
			return r.encode(struct {
				Head     domain.RepositoryHead `json:"head"`
				BaseHead domain.RepositoryHead `json:"base_head"`
				Changes  []domain.Change       `json:"changes"`
			}{st.Head, st.BaseHead, changes})
		}
		if _, err := fmt.Fprintf(r.stdout, "HEAD %s\nBase %s\n", st.Head, st.BaseHead); err != nil {
			return err
		}
		for _, change := range changes {
			asset := st.Assets[change.AssetID]
			if change.Asset != nil {
				asset = *change.Asset
			}
			if err := r.printAssets([]app.AssetView{{Asset: asset, State: string(change.Kind)}}); err != nil {
				return err
			}
		}
		return nil
	}}
}

func (r *runtime) resetCommand() *cobra.Command {
	var staged bool
	cmd := &cobra.Command{Use: "reset --staged", Short: "Discard all unpublished local changes", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		if !staged {
			return usage("reset requires --staged")
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		git, err := r.backend(cmd.Context(), s)
		if err != nil {
			return err
		}
		if err := s.Reset(cmd.Context(), git); err != nil {
			return err
		}
		return r.mutationResult()
	}}
	cmd.Flags().BoolVar(&staged, "staged", false, "Discard staged changes without changing the repository")
	return cmd
}

func (r *runtime) mutationResult() error {
	if r.json {
		return r.encode(struct {
			OK bool `json:"ok"`
		}{true})
	}
	return nil
}

func matches(v app.AssetView, query string) bool {
	if strings.Contains(strings.ToLower(v.Asset.Title), query) || strings.Contains(strings.ToLower(v.Asset.OriginalName), query) {
		return true
	}
	// Hashes are lowercase hex, so an upper-case query still matches through the
	// lowered query the caller builds; the canonical path never proves identity.
	if strings.Contains(v.Asset.Object.Hash.String(), query) {
		return true
	}
	for _, values := range [][]string{v.Asset.Tags, v.Asset.Collections} {
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), query) {
				return true
			}
		}
	}
	return false
}
