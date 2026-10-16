// Package browser hands a URL or a directory to the desktop's default handler.
package browser

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// Open starts the platform launcher for target, which may be a URL or a local
// path. It returns once the launcher has been started, not when the handler has
// finished loading the target.
func Open(ctx context.Context, target string) error {
	name, args := launcher(target)
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

// launcher names the platform's default opener. The command is passed to the
// desktop rather than resolved here, so TOUKO never guesses an application.
func launcher(target string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{target}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		return "xdg-open", []string{target}
	}
}
