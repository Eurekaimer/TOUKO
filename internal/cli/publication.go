package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"strings"
)

func (r *runtime) commitCommand() *cobra.Command {
	var message string
	cmd := &cobra.Command{Use: "commit -m MESSAGE", Short: "Publish all staged changes as one recoverable Git commit", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(message) == "" {
			return usage("commit requires a nonempty -m MESSAGE")
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		git, err := r.backend(cmd.Context(), s)
		if err != nil {
			return err
		}
		result, err := s.Commit(cmd.Context(), git, message)
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(result)
		}
		if result.ChangeCount == 0 {
			_, err = fmt.Fprintln(r.stdout, "nothing staged")
			if err != nil {
				return err
			}
			return r.printCloudflare(result.Cloudflare)
		}
		_, err = fmt.Fprintf(r.stdout, "Committed %s (%d changes, %d assets)\n", result.Head, result.ChangeCount, result.AssetCount)
		if err != nil {
			return err
		}
		return r.printCloudflare(result.Cloudflare)
	}}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Commit message")
	return cmd
}
func (r *runtime) verifyCommand() *cobra.Command {
	return &cobra.Command{Use: "verify", Short: "Verify current remote metadata and every referenced image byte", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		git, err := r.reader(cmd.Context(), s)
		if err != nil {
			return err
		}
		result, err := s.Verify(cmd.Context(), git)
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(result)
		}
		_, err = fmt.Fprintf(r.stdout, "Verified %s: assets=%d objects=%d\n", result.Head, result.Assets, result.Objects)
		return err
	}}
}
