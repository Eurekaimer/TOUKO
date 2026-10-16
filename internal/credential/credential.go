// Package credential resolves runtime GitHub credentials without persisting them.
package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Resolve prefers explicit environment credentials, then the GitHub CLI keyring.
// A missing gh executable or an unauthenticated gh session permits anonymous reads.
func Resolve(ctx context.Context) (token string, source string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "anonymous", err
	}
	for _, entry := range []struct{ name, source string }{{"GITHUB_TOKEN", "github_token"}, {"GH_TOKEN", "gh_token"}} {
		if value := os.Getenv(entry.name); value != "" {
			if strings.ContainsAny(value, " \t\r\n\x00") {
				return "", entry.source, fmt.Errorf("invalid %s credential", entry.name)
			}
			return value, entry.source, nil
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", "gh", ctx.Err()
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", "anonymous", nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(stderr.String()) == "no oauth token found for github.com" {
			return "", "anonymous", nil
		}
		return "", "gh", fmt.Errorf("cannot read GitHub CLI credentials; run gh auth status")
	}
	token = strings.TrimSpace(stdout.String())
	if token == "" || strings.ContainsAny(token, " \t\r\n\x00") {
		return "", "gh", fmt.Errorf("GitHub CLI returned an invalid credential")
	}
	return token, "gh", nil
}
