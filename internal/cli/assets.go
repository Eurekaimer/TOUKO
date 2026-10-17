package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/spf13/cobra"
)

func (r *runtime) addCommand() *cobra.Command {
	var namespace, directory, title, name, output string
	var tags, collections []string
	cmd := &cobra.Command{Use: "add FILE...", Short: "Stage images, or read one image from stdin with add -", Args: arguments(cobra.MinimumNArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("output") && r.json {
			return usage("--output cannot be combined with --json")
		}
		if output != "" && output != "id" && output != "markdown" {
			return usage("--output must be id or markdown")
		}
		if cmd.Flags().Changed("output") && output == "" {
			return usage("--output must be id or markdown")
		}
		stdin := len(args) == 1 && args[0] == "-"
		for _, path := range args {
			if path == "-" && !stdin {
				return usage("stdin '-' cannot be combined with other inputs")
			}
		}
		if cmd.Flags().Changed("name") && !stdin {
			return usage("--name is only valid with stdin input")
		}
		ns, err := domain.ParseNamespace(namespace)
		if err != nil {
			return &usageError{err}
		}
		opts := app.AddOptions{Namespace: ns, Directory: directory, Tags: tags, Collections: collections}
		if cmd.Flags().Changed("title") {
			opts.Title = &title
		}
		inputs := make([]app.Input, 0, len(args))
		files := make([]*os.File, 0, len(args))
		defer func() {
			for _, file := range files {
				_ = file.Close()
			}
		}()
		if stdin {
			inputs = append(inputs, app.Input{Reader: r.stdin, Name: name})
		} else {
			for _, path := range args {
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				files = append(files, file)
				inputs = append(inputs, app.Input{Reader: file, Name: filepath.Base(path)})
			}
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		var location repository.Location
		if output == "markdown" {
			location, err = s.Location(cmd.Context())
			if err != nil {
				return err
			}
			if location.Backend != "github" || location.Private {
				return fmt.Errorf("%w: Markdown requires a public GitHub repository", domain.ErrDeliveryUnavailable)
			}
		}
		assets, err := s.Add(cmd.Context(), inputs, opts)
		if err != nil {
			return err
		}
		if output == "id" {
			for _, asset := range assets {
				if _, err := fmt.Fprintln(r.stdout, asset.Asset.ID); err != nil {
					return err
				}
			}
			return nil
		}
		if output == "markdown" {
			for _, view := range assets {
				link, err := delivery.BuildURL(location, repository.ObjectPath(view.Asset), delivery.JSDelivr)
				if err != nil {
					return err
				}
				if _, err = fmt.Fprintf(r.stdout, "![%s](%s)\n", markdownAlt(view.Asset.Title), link); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(r.stderr, "staged; not published until commit")
			return err
		}
		return r.printAssets(assets)
	}}
	cmd.Flags().StringVar(&namespace, "namespace", "default", "Object namespace")
	cmd.Flags().StringVar(&directory, "directory", "", "Repository directory for uploaded images (default: canonical objects path)")
	cmd.Flags().StringVar(&title, "title", "", "Image title (defaults to original name)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Tag; repeat for multiple tags")
	cmd.Flags().StringArrayVar(&collections, "collection", nil, "Collection; repeat for multiple collections")
	cmd.Flags().StringVar(&name, "name", "", "Original filename for stdin input")
	cmd.Flags().StringVar(&output, "output", "", "Output format: id or markdown (default: human-readable)")
	return cmd
}

func (r *runtime) editCommand() *cobra.Command {
	var title string
	var tags, collections []string
	var clearTags, clearCollections bool
	cmd := &cobra.Command{Use: "edit SELECTOR", Short: "Stage title, tag or collection changes", Args: arguments(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		if clearTags && cmd.Flags().Changed("tag") {
			return usage("--clear-tags cannot be combined with --tag")
		}
		if clearCollections && cmd.Flags().Changed("collection") {
			return usage("--clear-collections cannot be combined with --collection")
		}
		opts := app.EditOptions{}
		if cmd.Flags().Changed("title") {
			opts.Title = &title
		}
		if clearTags {
			tags = []string{}
		}
		if clearCollections {
			collections = []string{}
		}
		if clearTags || cmd.Flags().Changed("tag") {
			opts.Tags = &tags
		}
		if clearCollections || cmd.Flags().Changed("collection") {
			opts.Collections = &collections
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		if err := s.Edit(cmd.Context(), args[0], opts); err != nil {
			return err
		}
		return r.mutationResult()
	}}
	cmd.Flags().StringVar(&title, "title", "", "Replace title")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Replace tags with the complete repeated list")
	cmd.Flags().StringArrayVar(&collections, "collection", nil, "Replace collections with the complete repeated list")
	cmd.Flags().BoolVar(&clearTags, "clear-tags", false, "Remove all tags")
	cmd.Flags().BoolVar(&clearCollections, "clear-collections", false, "Remove all collections")
	return cmd
}

func (r *runtime) removeCommand() *cobra.Command {
	return &cobra.Command{Use: "rm SELECTOR...", Short: "Stage removal of images", Args: arguments(cobra.MinimumNArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		if err := s.Remove(cmd.Context(), args); err != nil {
			return err
		}
		return r.mutationResult()
	}}
}

func (r *runtime) listCommand() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{Use: "ls", Short: "List images including staged changes", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		var ns domain.Namespace
		if cmd.Flags().Changed("namespace") {
			var err error
			ns, err = domain.ParseNamespace(namespace)
			if err != nil {
				return &usageError{err}
			}
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		assets, err := s.List(cmd.Context())
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("namespace") {
			filtered := make([]app.AssetView, 0, len(assets))
			for _, asset := range assets {
				if asset.Asset.Namespace == ns {
					filtered = append(filtered, asset)
				}
			}
			assets = filtered
		}
		return r.printAssets(assets)
	}}
	cmd.Flags().StringVar(&namespace, "namespace", "", "Only show this namespace")
	return cmd
}

func (r *runtime) findCommand() *cobra.Command {
	return &cobra.Command{Use: "find QUERY", Short: "Search titles, filenames, tags, collections and content hashes", Args: arguments(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[0]) == "" {
			return usage("find requires a nonempty query")
		}
		query := strings.ToLower(args[0])
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		assets, err := s.List(cmd.Context())
		if err != nil {
			return err
		}
		found := make([]app.AssetView, 0)
		for _, asset := range assets {
			if matches(asset, query) {
				found = append(found, asset)
			}
		}
		return r.printAssets(found)
	}}
}

func (r *runtime) showCommand() *cobra.Command {
	return &cobra.Command{Use: "show SELECTOR", Short: "Show one image by UUID or unambiguous UUID prefix", Args: arguments(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		asset, err := s.Show(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(struct {
				Asset app.AssetView `json:"asset"`
			}{asset})
		}
		return r.printAssets([]app.AssetView{asset})
	}}
}
