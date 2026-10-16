package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"

	"github.com/Eurekaimer/TOUKO/internal/webview"
	"github.com/spf13/cobra"
)

// serveCommand keeps the process alive; CLI.Run owns and closes the Service only
// after all HTTP handlers have stopped, including an interrupted publication.
func (r *runtime) serveCommand() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{Use: "serve", Short: "Serve the local TOUKO gallery in a browser", Args: arguments(cobra.NoArgs), RunE: func(cmd *cobra.Command, args []string) (err error) {
		if r.json {
			return usage("serve is an interactive server; omit --json")
		}
		if port < 0 || port > 65535 {
			return usage("--port must be between 0 and 65535")
		}
		s, err := r.service(cmd.Context())
		if err != nil {
			return err
		}
		host, err := webview.Listen(cmd.Context(), s, os.Getenv("GITHUB_TOKEN"), port)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, host.Close()) }()
		if _, err = fmt.Fprintf(r.stdout, "TOUKO Webview: %s\nState: %s\nPress Ctrl+C to stop.\n", host.URL, s.Dir); err != nil {
			return err
		}
		if !noOpen {
			if openErr := openBrowser(cmd.Context(), host.URL); openErr != nil {
				if _, err := fmt.Fprintf(r.stderr, "Could not open browser: %v. Open %s manually; the server is still running.\n", openErr, host.URL); err != nil {
					return err
				}
			}
		}
		return host.Wait(cmd.Context())
	}}
	cmd.Flags().IntVar(&port, "port", 7788, "Loopback HTTP port (0 selects an available port)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Do not automatically open the browser")
	return cmd
}

func openBrowser(ctx context.Context, url string) error {
	var name string
	var args []string
	switch goruntime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
