package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/credential"
	"github.com/Eurekaimer/TOUKO/internal/delivery"
	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/github"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	"github.com/spf13/cobra"
)

var altEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, "\n", " ", "\r", " ")

func markdownAlt(title string) string { return altEscaper.Replace(title) }
func (r *runtime) authCommand() *cobra.Command {
	auth := &cobra.Command{Use: "auth", Short: "Use GitHub CLI or environment authentication", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() }}
	auth.AddCommand(&cobra.Command{Use: "login", Short: "Sign in with GitHub CLI using the browser", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		if r.json {
			return usage("auth login is interactive; omit --json")
		}
		login := exec.CommandContext(cmd.Context(), "gh", "auth", "login", "--hostname", "github.com", "--web", "--git-protocol", "https")
		login.Stdin, login.Stdout, login.Stderr = r.stdin, r.stdout, r.stderr
		if err := login.Run(); err != nil {
			if cmd.Context().Err() != nil {
				return cmd.Context().Err()
			}
			return fmt.Errorf("GitHub login failed; install GitHub CLI and run touko auth login")
		}
		return nil
	}})
	auth.AddCommand(&cobra.Command{Use: "status", Short: "Display the GitHub login and credential source without exposing the token", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) error {
		token, source, err := credential.Resolve(cmd.Context())
		if err != nil {
			return err
		}
		if token == "" {
			return fmt.Errorf("%w: source anonymous; run touko auth login", domain.ErrUnauthorized)
		}
		client, err := github.New(repository.Location{Backend: "github", Owner: "account", Name: "status", Branch: "main"}, token)
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
				Source        string `json:"source"`
			}{login, true, source})
		}
		_, err = fmt.Fprintf(r.stdout, "Authenticated as %s (source: %s)\n", login, source)
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
		link, err := delivery.BuildURL(location, repository.ObjectPath(view.Asset), p)
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
