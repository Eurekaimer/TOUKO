// Package localgit implements the Git port with local Git plumbing commands.
package localgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const zeroID = "0000000000000000000000000000000000000000"
const maxMetadataOutput = 32 << 20

// Repository addresses one branch of an existing SHA-1 Git repository.
type Repository struct {
	path   string
	branch string
}

var _ repository.Git = (*Repository)(nil)

// Open validates the repository and branch without changing either.
func Open(ctx context.Context, path, branch string) (*Repository, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	r := &Repository{path: absolute, branch: branch}
	if err := r.validateBranch(ctx); err != nil {
		return nil, err
	}
	// One process answers both questions: object format, then absolute git dir.
	out, err := r.run(ctx, nil, "rev-parse", "--show-object-format", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	format, gitDir, _ := strings.Cut(string(out), "\n")
	if format != "sha1" {
		return nil, fmt.Errorf("local Git repository must use sha1 object format")
	}
	if strings.TrimSpace(gitDir) == "" {
		return nil, fmt.Errorf("unexpected git rev-parse output")
	}
	return r, nil
}

// Init creates a bare repository, refusing to overwrite a nonempty directory.
func Init(ctx context.Context, path, branch string) (*Repository, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	r := &Repository{path: absolute, branch: branch}
	// Validate before creating even an empty target directory.
	if err := r.validateBranch(ctx); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("repository target is not a directory: %s", absolute)
		}
		directory, err := os.Open(absolute)
		if err != nil {
			return nil, err
		}
		_, readErr := directory.Readdirnames(1)
		closeErr := directory.Close()
		if readErr == nil {
			return nil, fmt.Errorf("repository target is not empty: %s", absolute)
		}
		if !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Git performs directory creation; do not remove its output on failure.
	if _, err := runGit(ctx, "", nil, "init", "--bare", "--object-format=sha1", "--initial-branch="+branch, "--", absolute); err != nil {
		return nil, err
	}
	return Open(ctx, absolute, branch)
}

func (r *Repository) validateBranch(ctx context.Context) error {
	if r.branch == "" {
		return fmt.Errorf("Git branch must not be empty")
	}
	out, err := runGit(ctx, "", nil, "check-ref-format", "--branch", r.branch)
	if err != nil {
		return err
	}
	// Do not accept check-ref-format's previous-checkout name expansion.
	if strings.TrimSuffix(string(out), "\n") != r.branch {
		return fmt.Errorf("Git branch shorthand is not allowed")
	}
	return nil
}

// Git environment supplied by a caller must not redirect the repository,
// inject configuration, replace objects, or supply commit identities.
func gitEnvironment() []string {
	source := os.Environ()
	env := make([]string, 0, len(source)+7)
	for _, item := range source {
		if !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=TOUKO", "GIT_AUTHOR_EMAIL=touko@localhost",
		"GIT_COMMITTER_NAME=TOUKO", "GIT_COMMITTER_EMAIL=touko@localhost",
		"GIT_TERMINAL_PROMPT=0")
}

func command(ctx context.Context, path string, args ...string) *exec.Cmd {
	argv := []string{"--no-replace-objects"}
	if path != "" {
		argv = append(argv, "-C", path)
	}
	cmd := exec.CommandContext(ctx, "git", append(argv, args...)...)
	cmd.Env = gitEnvironment()
	return cmd
}

type cappedOutput struct{ bytes.Buffer }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > maxMetadataOutput-b.Len() {
		return 0, fmt.Errorf("Git metadata output exceeds %d bytes", maxMetadataOutput)
	}
	return b.Buffer.Write(p)
}

type errorOutput struct{ bytes.Buffer }

func (b *errorOutput) Write(p []byte) (int, error) {
	n := len(p)
	if available := (64 << 10) - b.Len(); available > 0 {
		if len(p) > available {
			p = p[:available]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func runGit(ctx context.Context, path string, input io.Reader, args ...string) ([]byte, error) {
	cmd := command(ctx, path, args...)
	var out cappedOutput
	var stderr errorOutput
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func (r *Repository) run(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	return runGit(ctx, r.path, input, args...)
}

func validateID(id string) error {
	if len(id) != 40 {
		return fmt.Errorf("invalid SHA-1 Git object ID %q", id)
	}
	for _, c := range []byte(id) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("invalid SHA-1 Git object ID %q", id)
		}
	}
	if id == zeroID {
		return fmt.Errorf("zero is not a Git object ID")
	}
	return nil
}

func parseID(out []byte) (string, error) {
	id := strings.TrimSpace(string(out))
	return id, validateID(id)
}

func (r *Repository) Head(ctx context.Context) (domain.RepositoryHead, error) {
	ref := "refs/heads/" + r.branch
	// --exists distinguishes missing refs (exit 2) from lookup errors (exit 1).
	_, err := r.run(ctx, nil, "show-ref", "--exists", ref)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return "", nil
		}
		return "", err
	}
	out, err := r.run(ctx, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	id, err := parseID(out)
	return domain.RepositoryHead(id), err
}

func (r *Repository) Commit(ctx context.Context, head domain.RepositoryHead) (repository.GitCommit, error) {
	var result repository.GitCommit
	if err := validateID(string(head)); err != nil {
		return result, err
	}
	out, err := r.run(ctx, nil, "cat-file", "commit", string(head))
	if err != nil {
		return result, err
	}
	header, _, _ := bytes.Cut(out, []byte("\n\n"))
	for _, line := range bytes.Split(header, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("tree ")) {
			id := string(line[5:])
			if err := validateID(id); err != nil {
				return result, err
			}
			if result.Tree != "" {
				return result, fmt.Errorf("Git commit contains multiple trees")
			}
			result.Tree = repository.TreeID(id)
		} else if bytes.HasPrefix(line, []byte("parent ")) {
			id := string(line[7:])
			if err := validateID(id); err != nil {
				return result, err
			}
			result.Parents = append(result.Parents, domain.RepositoryHead(id))
		}
	}
	if result.Tree == "" {
		return result, fmt.Errorf("Git commit has no tree")
	}
	return result, nil
}

func (r *Repository) Tree(ctx context.Context, tree repository.TreeID) ([]repository.GitEntry, error) {
	if err := validateID(string(tree)); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "ls-tree", "-r", "-t", "-z", "-l", string(tree))
	if err != nil {
		return nil, err
	}
	entries := make([]repository.GitEntry, 0)
	for len(out) > 0 {
		record, rest, found := bytes.Cut(out, []byte{0})
		if !found {
			return nil, fmt.Errorf("unterminated Git tree record")
		}
		out = rest
		header, path, found := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(header)
		if !found || len(fields) != 4 {
			return nil, fmt.Errorf("invalid Git tree record")
		}
		entry := repository.GitEntry{Mode: string(fields[0]), Type: string(fields[1]), SHA: string(fields[2]), Path: string(path), Size: -1}
		if err := validateID(entry.SHA); err != nil {
			return nil, err
		}
		if string(fields[3]) != "-" {
			entry.Size, err = strconv.ParseInt(string(fields[3]), 10, 64)
			if err != nil || entry.Size < 0 {
				return nil, fmt.Errorf("invalid Git entry size")
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// blobReader waits for Git on EOF and kills it on an early Close. Both paths
// reap the process, including command-context cancellation and pipe failures.
type blobReader struct {
	pipe      io.ReadCloser
	cmd       *exec.Cmd
	ctx       context.Context
	stderr    *errorOutput
	waitOnce  sync.Once
	closeOnce sync.Once
	waitErr   error
	closeErr  error
	eof       bool
}

func (b *blobReader) wait() error {
	b.waitOnce.Do(func() {
		b.waitErr = b.cmd.Wait()
		if b.ctx.Err() != nil {
			b.waitErr = b.ctx.Err()
		} else if b.waitErr != nil {
			b.waitErr = fmt.Errorf("git cat-file: %w: %s", b.waitErr, strings.TrimSpace(b.stderr.String()))
		}
	})
	return b.waitErr
}
func (b *blobReader) Read(p []byte) (int, error) {
	n, err := b.pipe.Read(p)
	if err == io.EOF {
		b.eof = true
		if waitErr := b.wait(); waitErr != nil {
			return n, waitErr
		}
	} else if err != nil {
		_ = b.Close()
	}
	return n, err
}
func (b *blobReader) Close() error {
	b.closeOnce.Do(func() {
		_ = b.pipe.Close()
		if !b.eof {
			_ = b.cmd.Process.Kill()
		}
		err := b.wait()
		// Killing a deliberately abandoned stream is successful cleanup.
		if b.eof || b.ctx.Err() != nil {
			b.closeErr = err
		}
	})
	return b.closeErr
}

// HasObject reports whether the object database already holds id.
func (r *Repository) HasObject(ctx context.Context, id string) bool {
	if validateID(id) != nil {
		return false
	}
	_, err := r.run(ctx, nil, "cat-file", "-e", id)
	return err == nil
}

func (r *Repository) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	if err := validateID(string(id)); err != nil {
		return nil, err
	}
	cmd := command(ctx, r.path, "cat-file", "blob", string(id))
	stderr := &errorOutput{}
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = pipe.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return &blobReader{pipe: pipe, cmd: cmd, ctx: ctx, stderr: stderr}, nil
}

// NewLocalFirst answers immutable objects from the checkout when it already
// holds them. HEAD, writes and ref updates stay with api, which is
// authoritative; an object Git does not have falls back to api.
func NewLocalFirst(api repository.Git, local *Repository) repository.Git {
	return &localFirst{Git: api, local: local}
}

type localFirst struct {
	repository.Git
	local *Repository
}

func (r *localFirst) Commit(ctx context.Context, head domain.RepositoryHead) (repository.GitCommit, error) {
	if r.local.HasObject(ctx, string(head)) {
		if commit, err := r.local.Commit(ctx, head); err == nil {
			return commit, nil
		}
	}
	return r.Git.Commit(ctx, head)
}

func (r *localFirst) Tree(ctx context.Context, tree repository.TreeID) ([]repository.GitEntry, error) {
	if r.local.HasObject(ctx, string(tree)) {
		if entries, err := r.local.Tree(ctx, tree); err == nil {
			return entries, nil
		}
	}
	return r.Git.Tree(ctx, tree)
}

func (r *localFirst) OpenBlob(ctx context.Context, id repository.BlobID) (io.ReadCloser, error) {
	if r.local.HasObject(ctx, string(id)) {
		return r.local.OpenBlob(ctx, id)
	}
	return r.Git.OpenBlob(ctx, id)
}

func (r *Repository) IsAncestor(ctx context.Context, ancestor, head domain.RepositoryHead) (bool, error) {
	if err := validateID(string(ancestor)); err != nil {
		return false, err
	}
	if err := validateID(string(head)); err != nil {
		return false, err
	}
	_, err := r.run(ctx, nil, "merge-base", "--is-ancestor", string(ancestor), string(head))
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func (r *Repository) CreateBlob(ctx context.Context, reader io.Reader, size int64) (repository.BlobID, error) {
	if size < 0 {
		return "", fmt.Errorf("negative blob size")
	}
	// Read one extra byte to reject oversized input, without buffering its bytes.
	// Avoid size+1 overflow for malformed callers.
	limit := size
	if limit < int64(^uint64(0)>>1) {
		limit++
	}
	input := &io.LimitedReader{R: reader, N: limit}
	out, err := r.run(ctx, input, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if limit-input.N != size {
		return "", fmt.Errorf("blob size does not match expected %d bytes", size)
	}
	id, err := parseID(out)
	return repository.BlobID(id), err
}

type treeNode struct {
	mode, kind, sha string
	children        map[string]*treeNode
	dirty           bool
}

func directoryNode() *treeNode {
	return &treeNode{mode: "040000", kind: "tree", children: make(map[string]*treeNode)}
}

func validatePath(path string) error {
	if strings.ContainsAny(path, "\x00\\") {
		return fmt.Errorf("unsafe Git tree path %q", path)
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("unsafe Git tree path %q", path)
		}
	}
	return nil
}

func (r *Repository) CreateTree(ctx context.Context, base repository.TreeID, changes []repository.TreeChange) (repository.TreeID, error) {
	if base != "" {
		if err := validateID(string(base)); err != nil {
			return "", err
		}
	}
	paths := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if err := validatePath(change.Path); err != nil {
			return "", err
		}
		if change.Blob != nil {
			if err := validateID(string(*change.Blob)); err != nil {
				return "", err
			}
		}
		if _, exists := paths[change.Path]; exists {
			return "", fmt.Errorf("duplicate Git tree change: %s", change.Path)
		}
		paths[change.Path] = struct{}{}
	}
	for path := range paths {
		for slash := strings.LastIndexByte(path, '/'); slash >= 0; slash = strings.LastIndexByte(path, '/') {
			path = path[:slash]
			if _, exists := paths[path]; exists {
				return "", fmt.Errorf("overlapping Git tree changes")
			}
		}
	}
	root := directoryNode()
	root.sha = string(base)
	if base != "" {
		entries, err := r.Tree(ctx, base)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			parts := strings.Split(entry.Path, "/")
			parent := root
			for _, part := range parts[:len(parts)-1] {
				child := parent.children[part]
				if child == nil || child.kind != "tree" {
					return "", fmt.Errorf("invalid base tree hierarchy")
				}
				parent = child
			}
			node := &treeNode{mode: entry.Mode, kind: entry.Type, sha: entry.SHA}
			if entry.Type == "tree" {
				node.children = make(map[string]*treeNode)
			}
			parent.children[parts[len(parts)-1]] = node
		}
	}
	for _, change := range changes {
		parts := strings.Split(change.Path, "/")
		parent := root
		ancestors := []*treeNode{root}
		missing := false
		for _, part := range parts[:len(parts)-1] {
			child := parent.children[part]
			if child == nil {
				if change.Blob == nil {
					missing = true
					break
				}
				child = directoryNode()
				parent.children[part] = child
			}
			if child.kind != "tree" {
				return "", fmt.Errorf("Git tree path traverses non-directory: %s", change.Path)
			}
			parent = child
			ancestors = append(ancestors, parent)
		}
		if missing {
			continue
		}
		name := parts[len(parts)-1]
		old := parent.children[name]
		if change.Blob == nil {
			if old == nil {
				continue
			}
			delete(parent.children, name)
		} else {
			if old != nil && old.kind == "tree" {
				return "", fmt.Errorf("Git tree path replaces directory: %s", change.Path)
			}
			if old != nil && old.mode == "100644" && old.kind == "blob" && old.sha == string(*change.Blob) {
				continue
			}
			parent.children[name] = &treeNode{mode: "100644", kind: "blob", sha: string(*change.Blob)}
		}
		for _, node := range ancestors {
			node.dirty = true
		}
		// A removed last file removes its now-empty directory, not unrelated paths.
		if change.Blob == nil {
			for i := len(ancestors) - 1; i > 0; i-- {
				if len(ancestors[i].children) != 0 {
					break
				}
				delete(ancestors[i-1].children, parts[i-1])
			}
		}
	}
	return r.writeTree(ctx, root)
}

func (r *Repository) writeTree(ctx context.Context, node *treeNode) (repository.TreeID, error) {
	if !node.dirty && node.sha != "" {
		return repository.TreeID(node.sha), nil
	}
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	var input bytes.Buffer
	for _, name := range names {
		child := node.children[name]
		sha := child.sha
		if child.kind == "tree" {
			id, err := r.writeTree(ctx, child)
			if err != nil {
				return "", err
			}
			sha = string(id)
		}
		fmt.Fprintf(&input, "%s %s %s\t%s%c", child.mode, child.kind, sha, name, 0)
	}
	out, err := r.run(ctx, &input, "mktree", "-z")
	if err != nil {
		return "", err
	}
	id, err := parseID(out)
	return repository.TreeID(id), err
}

func (r *Repository) CreateCommit(ctx context.Context, tree repository.TreeID, parent domain.RepositoryHead, message string) (domain.RepositoryHead, error) {
	if err := validateID(string(tree)); err != nil {
		return "", err
	}
	args := []string{"-c", "commit.gpgSign=false", "commit-tree", string(tree)}
	if parent != "" {
		if err := validateID(string(parent)); err != nil {
			return "", err
		}
		args = append(args, "-p", string(parent))
	}
	// The message is stdin, never an option or shell argument.
	out, err := r.run(ctx, strings.NewReader(message), args...)
	if err != nil {
		return "", err
	}
	id, err := parseID(out)
	return domain.RepositoryHead(id), err
}

func (r *Repository) UpdateRef(ctx context.Context, expected, next domain.RepositoryHead) error {
	if err := validateID(string(next)); err != nil {
		return err
	}
	old := zeroID
	if expected != "" {
		if err := validateID(string(expected)); err != nil {
			return err
		}
		old = string(expected)
	}
	_, err := r.run(ctx, nil, "update-ref", "refs/heads/"+r.branch, string(next), old)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	remote, readErr := r.Head(ctx)
	if readErr == nil && remote != expected {
		return &domain.ConflictError{Base: expected, Remote: remote, Reason: "head_changed"}
	}
	return err
}
