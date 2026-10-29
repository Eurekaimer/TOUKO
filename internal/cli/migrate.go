package cli

import (
	"fmt"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/spf13/cobra"
)

func (r *runtime) migrateCommand() *cobra.Command {
	var apply, dryRun, pruneOld bool
	var output string
	cmd := &cobra.Command{Use: "migrate", Short: "Rewrite stored objects to the current layout"}
	paths := &cobra.Command{
		Use:   "paths",
		Short: "Rewrite every asset to objects/<namespace>/<asset-id>.<ext> (preview unless --apply)",
		Args:  arguments(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := r.service(cmd.Context())
			if err != nil {
				return err
			}
			git, err := r.commitReader(cmd.Context(), s)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("dry-run") {
				if apply {
					return usage("--apply and --dry-run are mutually exclusive")
				}
				if !dryRun {
					return usage("use --apply to write migration changes")
				}
			}
			preview := !apply
			result, err := s.MigratePaths(cmd.Context(), git, preview, pruneOld)
			if err != nil && result.Head == "" {
				return err
			}
			var csvPath, jsonPath string
			if err == nil && !preview && hasPathChanges(result.Items) {
				dir := output
				if dir == "" {
					dir = filepath.Join(s.Dir, "migration")
				}
				csvPath, jsonPath, err = app.WriteURLMapping(cmd.Context(), dir, result.Items)
				if err != nil {
					return err
				}
			}
			if r.json {
				if printErr := r.encode(struct {
					app.MigrateResult
					MappingCSV  string `json:"mapping_csv,omitempty"`
					MappingJSON string `json:"mapping_json,omitempty"`
				}{result, csvPath, jsonPath}); printErr != nil {
					return printErr
				}
				return err
			}
			if _, printErr := fmt.Fprintf(r.stdout, "head %s migrated=%d unchanged=%d failed=%d pruned=%d r2_retained=%d\n", result.Head, result.Migrated, result.Unchanged, result.Failed, result.Pruned, result.R2Retained); printErr != nil {
				return printErr
			}
			if csvPath != "" {
				if _, printErr := fmt.Fprintf(r.stdout, "mapping %s\nmapping %s\n", csvPath, jsonPath); printErr != nil {
					return printErr
				}
			}
			if err == nil && preview {
				if _, printErr := fmt.Fprintln(r.stdout, "dry run: nothing was written; re-run with --apply to migrate"); printErr != nil {
					return printErr
				}
			}
			return err
		},
	}
	paths.Flags().BoolVar(&apply, "apply", false, "Write the new objects, switch metadata, and write the URL mapping")
	paths.Flags().BoolVar(&dryRun, "dry-run", true, "Preview only; mutually exclusive with --apply")
	paths.Flags().BoolVar(&pruneOld, "prune-old", false, "With --apply, delete old Git objects recorded in the durable mapping (never R2 keys)")
	paths.Flags().StringVar(&output, "output", "", "Directory for url-mapping.csv and url-mapping.json (default: <state>/migration)")
	cmd.AddCommand(paths)
	return cmd
}

// hasPathChanges keeps an all-unchanged run from writing a mapping that the
// rewrite script would reject as having no usable path changes.
func hasPathChanges(items []app.MigrateItem) bool {
	for _, item := range items {
		if item.OldPath != item.NewPath {
			return true
		}
	}
	return false
}
