package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/spf13/cobra"
)

var altEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, "\n", " ", "\r", " ")

func markdownAlt(title string) string { return altEscaper.Replace(title) }
func (r *runtime) authCommand() *cobra.Command {
	auth := &cobra.Command{Use: "auth", Short: "Inspect GitHub token authentication", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() }}
	auth.AddCommand(&cobra.Command{Use: "status", Short: "Check GITHUB_TOKEN and display its GitHub login", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := github.New(repository.Location{Backend: "github", Owner: "account", Name: "status", Branch: "main"}, os.Getenv("GITHUB_TOKEN"))
		if err != nil {
			return err
		}
		login, err := client.User(cmd.Context())
		if err != nil {
			return err
		}
		if r.json {
			return r.encode(struct {
				Login         string `json:"login"`
				Authenticated bool   `json:"authenticated"`
			}{login, true})
		}
		_, err = fmt.Fprintln(r.stdout, "Authenticated as", login)
		return err
	}})
	return auth
}
func (r *runtime) urlCommand() *cobra.Command {
	var provider string
	cmd := &cobra.Command{Use: "url SELECTOR", Short: "Derive a delivery URL for a public GitHub image", Args: arguments(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		p := delivery.Provider(provider)
		if p != delivery.JSDelivr && p != delivery.GitHub {
			return usage("--provider must be jsdelivr or github")
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		location, err := s.Location(cmd.Context())
		if err != nil {
			return err
		}
		view, err := s.Show(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		link, err := delivery.BuildURL(location, repository.BuildObjectPath(view.Asset.Namespace, view.Asset.Object), p)
		if err != nil {
			return err
		}
		committed := view.State != "added"
		if r.json {
			return r.encode(struct {
				URL       string `json:"url"`
				Committed bool   `json:"committed"`
			}{link, committed})
		}
		if !committed {
			if _, err = fmt.Fprintln(r.stderr, "staged; not published until commit"); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(r.stdout, link)
		return err
	}}
	cmd.Flags().StringVar(&provider, "provider", "jsdelivr", "Delivery provider: jsdelivr or github")
	return cmd
}
