package cli

import (
	"fmt"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/spf13/cobra"
)

func (r *runtime) migrateCommand() *cobra.Command {
	var dryRun, pruneOld bool
	var output string
	cmd := &cobra.Command{Use: "migrate", Short: "Rewrite stored objects to the current layout"}
	paths := &cobra.Command{
		Use:   "paths",
		Short: "Rewrite every asset to objects/<namespace>/<asset-id>.<ext> in one commit",
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
			result, err := s.MigratePaths(cmd.Context(), git, dryRun, pruneOld)
			if err != nil && result.Head == "" {
				return err
			}
			var csvPath, jsonPath string
			if !dryRun && len(result.Items) > 0 {
				dir := output
				if dir == "" {
					dir = filepath.Join(s.Dir, "migration")
				}
				csvPath, jsonPath, err = app.WriteURLMapping(dir, result.Items)
				if err != nil {
					return err
				}
			}
			if r.json {
				return r.encode(struct {
					app.MigrateResult
					MappingCSV  string `json:"mapping_csv,omitempty"`
					MappingJSON string `json:"mapping_json,omitempty"`
				}{result, csvPath, jsonPath})
			}
			if _, printErr := fmt.Fprintf(r.stdout, "head %s migrated=%d unchanged=%d failed=%d pruned=%d\n", result.Head, result.Migrated, result.Unchanged, result.Failed, result.Pruned); printErr != nil {
				return printErr
			}
			if csvPath != "" {
				if _, printErr := fmt.Fprintf(r.stdout, "mapping %s\nmapping %s\n", csvPath, jsonPath); printErr != nil {
					return printErr
				}
			}
			return err
		},
	}
	paths.Flags().BoolVar(&dryRun, "dry-run", false, "Report the planned paths without writing anything")
	paths.Flags().BoolVar(&pruneOld, "prune-old", false, "Delete legacy object paths that no asset references (run after the new URLs are verified)")
	paths.Flags().StringVar(&output, "output", "", "Directory for url-mapping.csv and url-mapping.json (default: <state>/migration)")
	cmd.AddCommand(paths)
	return cmd
}
