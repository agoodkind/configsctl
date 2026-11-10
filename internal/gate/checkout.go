package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// mainRef is the remote branch every requested commit must be reachable from.
const mainRef = "origin/main"

// ErrDirty reports configuration changes outside the current commit.
var ErrDirty = errors.New("commit or remove configuration changes before changing production")

// Repo checks Git revisions and manages detached worktrees.
type Repo struct {
	Dir string
}

// Fetch updates the remote branches of the clone.
func (r Repo) Fetch(ctx context.Context) error {
	return r.git(ctx, "fetch", "--quiet", "origin")
}

// RequireOnMain refuses a commit that origin/main does not contain.
func (r Repo) RequireOnMain(ctx context.Context, commit string) error {
	err := r.git(ctx, "merge-base", "--is-ancestor", commit, mainRef)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return fmt.Errorf("commit %s is not on %s", commit, mainRef)
	}

	return err
}

// CleanRevision returns HEAD only when tracked and untracked files are clean.
func (r Repo) CleanRevision(ctx context.Context) (string, error) {
	checkout, err := r.Snapshot(ctx)
	if err != nil {
		return "", err
	}

	if checkout.Dirty {
		return "", ErrDirty
	}

	return checkout.Commit, nil
}

// CheckoutRevision records the current commit and uncommitted changes.
type CheckoutRevision struct {
	Commit string
	Dirty  bool
}

// Snapshot reads the current commit and working tree status.
func (r Repo) Snapshot(ctx context.Context) (CheckoutRevision, error) {
	status, err := r.output(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return CheckoutRevision{}, err
	}

	revision, err := r.output(ctx, "rev-parse", "HEAD")
	if err != nil {
		return CheckoutRevision{}, err
	}

	return CheckoutRevision{
		Commit: strings.TrimSpace(string(revision)),
		Dirty:  len(status) != 0,
	}, nil
}

// Root returns the configuration repository root.
func (r Repo) Root(ctx context.Context) (string, error) {
	root, err := r.output(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(root)), nil
}

// AddWorktree creates a detached worktree of commit at path.
func (r Repo) AddWorktree(ctx context.Context, path, commit string) error {
	return r.git(ctx, "worktree", "add", "--detach", "--quiet", path, commit)
}

// RemoveWorktree deletes the worktree at path.
func (r Repo) RemoveWorktree(ctx context.Context, path string) error {
	return r.git(ctx, "worktree", "remove", "--force", path)
}

func (r Repo) git(ctx context.Context, args ...string) error {
	_, err := r.output(ctx, args...)
	return err
}

func (r Repo) output(ctx context.Context, args ...string) ([]byte, error) {
	safe, err := checkedArgs(append([]string{"-C", r.Dir}, args...))
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "git", safe...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if args[0] == "merge-base" && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, fmt.Errorf("git merge-base: %w", err)
		}

		slog.Error(
			"gate.checkout.git_failed",
			"args",
			strings.Join(args, " "),
			"stderr",
			strings.TrimSpace(stderr.String()),
			"err",
			err,
		)

		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return output, nil
}
