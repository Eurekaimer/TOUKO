package localgit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// credentialHelper answers Git's credential protocol by reading the secret from
// the inherited descriptor 3. Git runs helpers as a shell command, so the read
// happens on a pipe the parent fills: the token never enters a URL, argv, the
// environment, or any file Git writes.
const credentialHelper = `!f() { read -r _ignored; IFS= read -r p <&3; printf 'username=x-access-token\npassword=%s\n' "$p"; }; f`

// Clone materialises a working copy of one branch at dir. A valid existing clone
// is reused; an empty directory is cloned; anything else belongs to the user and
// is refused rather than overwritten. The files on disk are the point: a preview
// reads the picture file directly, with no object database to unpack.
func Clone(ctx context.Context, remote, dir, branch, token string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := validateRemote(remote); err != nil {
		return err
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	entries, err := os.ReadDir(absolute)
	switch {
	case err == nil && len(entries) > 0:
		if _, openErr := Open(ctx, absolute, branch); openErr != nil {
			return fmt.Errorf("clone path %s holds files that are not this repository: %w", absolute, openErr)
		}
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return err
	}
	args := []string{"clone", "--branch", branch, "--single-branch"}
	if token != "" {
		args = append(args, "--config", "credential.helper="+credentialHelper)
	}
	// `--` ends option parsing so a remote is never taken for a flag.
	args = append(args, "--", remote, absolute)
	stderr, err := runCredentialed(ctx, absolute, token, args...)
	if err != nil {
		return fmt.Errorf("git clone: %w: %s", err, redact(stderr, token))
	}
	return nil
}

// Update fetches the branch and moves the working copy to exactly what the
// remote now holds, so deletions leave the directory and additions appear.
func Update(ctx context.Context, dir, branch, token string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	fetch := []string{"fetch", "--no-tags", "origin", "+refs/heads/" + branch + ":refs/remotes/origin/" + branch}
	if token != "" {
		fetch = append([]string{"-c", "credential.helper=" + credentialHelper}, fetch...)
	}
	stderr, err := runCredentialed(ctx, absolute, token, fetch...)
	if err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, redact(stderr, token))
	}
	// --hard makes the tree match the remote exactly; the later clean removes the
	// files the remote deleted. Both are confined to this TOUKO-managed clone.
	if _, err := runGit(ctx, absolute, nil, "reset", "--hard", "refs/remotes/origin/"+branch); err != nil {
		return err
	}
	if _, err := runGit(ctx, absolute, nil, "clean", "-fd"); err != nil {
		return err
	}
	return nil
}

// runCredentialed runs one Git command with the optional credential helper and
// returns the trimmed stderr for error reporting.
func runCredentialed(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := command(ctx, dir, args...)
	if token != "" {
		read, write, err := os.Pipe()
		if err != nil {
			return "", err
		}
		go func() {
			defer write.Close()
			_, _ = write.WriteString(token + "\n")
		}()
		cmd.ExtraFiles = []*os.File{read}
	}
	stderr := &errorOutput{}
	cmd.Stderr = stderr
	err := cmd.Run()
	if cmd.ExtraFiles != nil {
		_ = cmd.ExtraFiles[0].Close()
	}
	if err != nil && ctx.Err() != nil {
		return stderr.String(), ctx.Err()
	}
	return stderr.String(), err
}

// redact removes a token from text bound for the user.
func redact(text, token string) string {
	if token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	return strings.TrimSpace(text)
}

// validateRemote accepts the URL forms a TOUKO location can produce and refuses
// any remote that already embeds credentials.
func validateRemote(remote string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\x00\r\n") {
		return fmt.Errorf("invalid remote %q", remote)
	}
	if strings.HasPrefix(remote, "ssh://") || strings.HasPrefix(remote, "git@") {
		return nil
	}
	if scheme := strings.Index(remote, "://"); scheme >= 0 {
		rest := remote[scheme+3:]
		if at, slash := strings.Index(rest, "@"), strings.Index(rest, "/"); at >= 0 && (slash < 0 || at < slash) {
			return fmt.Errorf("remote must not embed credentials")
		}
	}
	return nil
}

// validateBranch keeps a branch out of option position before it reaches git.
func validateBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, "\x00\r\n ") {
		return fmt.Errorf("invalid branch %q", branch)
	}
	return nil
}

var _ = exec.ExitError{}
