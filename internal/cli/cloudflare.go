package cli

import (
	"context"
	"fmt"

	"github.com/Eurekaimer/TOUKO/internal/app"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/spf13/cobra"
)

func (r *runtime) cloudflareCommand() *cobra.Command {
	var dryRun bool
	command := &cobra.Command{Use: "cloudflare", Short: "Reconcile the optional Cloudflare R2 mirror", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	sync := &cobra.Command{Use: "sync", Short: "Mirror current committed GitHub placements without publishing staging", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error {
		service, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		location, err := service.Location(cmd.Context())
		if err != nil {
			return err
		}
		var git repository.Git
		if location.Backend == "github" {
			git, err = r.backend(cmd.Context(), service)
			if err != nil {
				return err
			}
		}
		result, err := service.CloudflareSync(cmd.Context(), git, app.CloudflareOptions{DryRun: dryRun})
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(result)
		}
		_, err = fmt.Fprintf(r.stdout, "Cloudflare: %s; uploaded=%d skipped=%d deleted=%d pending=%d failed=%d\n", result.Status, result.Uploaded, result.Skipped, result.Deleted, result.Pending, result.Failed)
		if err != nil {
			return err
		}
		if dryRun {
			for _, item := range result.Items {
				if _, err = fmt.Fprintf(r.stdout, "%s\t%s\n", item.Status, item.ObjectPath); err != nil {
					return err
				}
			}
		}
		return nil
	}}
	sync.Flags().BoolVar(&dryRun, "dry-run", false, "Inspect GitHub/R2 without changing objects, receipts or staging")
	command.AddCommand(sync)
	return command
}

func (r *runtime) printCloudflare(result *app.CloudflareResult) error {
	if result == nil {
		return nil
	}
	if result.Status != "synced" {
		_, err := fmt.Fprintln(r.stderr, "GitHub: OK; Cloudflare: mirror failed; run touko cloudflare sync")
		return err
	}
	_, err := fmt.Fprintf(r.stdout, "Cloudflare: synced (uploaded=%d skipped=%d deleted=%d)\n", result.Uploaded, result.Skipped, result.Deleted)
	return err
}

func cloudflareStatusNeedsRepair(status *app.CloudflareStatus) bool {
	if status == nil {
		return false
	}
	if !status.Configured || status.PendingDeletes > 0 {
		return true
	}
	for _, asset := range status.Assets {
		if asset.State != "synced" {
			return true
		}
	}
	return false
}

func (r *runtime) resetResult(ctx context.Context, service *app.Service) error {
	state, views, err := service.ReadView(ctx)
	if err != nil {
		return r.mutationResult()
	} // Reset already succeeded; optional status cannot undo it.
	status := service.CloudflareStatus(ctx, state, views)
	if r.json {
		return r.encode(struct {
			OK         bool                  `json:"ok"`
			Cloudflare *app.CloudflareStatus `json:"cloudflare,omitempty"`
		}{true, status})
	}
	if cloudflareStatusNeedsRepair(status) {
		_, err = fmt.Fprintln(r.stderr, "GitHub: OK; Cloudflare: mirror needs repair; run touko cloudflare sync")
	}
	return err
}
