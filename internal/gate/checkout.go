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

// Repo creates one detached worktree per run at the requested commit.
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

// AddWorktree creates a detached worktree of commit at path.
func (r Repo) AddWorktree(ctx context.Context, path, commit string) error {
	return r.git(ctx, "worktree", "add", "--detach", "--quiet", path, commit)
}

// RemoveWorktree deletes the worktree at path.
func (r Repo) RemoveWorktree(ctx context.Context, path string) error {
	return r.git(ctx, "worktree", "remove", "--force", path)
}

func (r Repo) git(ctx context.Context, args ...string) error {
	safe, err := checkedArgs(append([]string{"-C", r.Dir}, args...))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", safe...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if args[0] == "merge-base" && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return fmt.Errorf("git merge-base: %w", err)
		}
		slog.Error("gate.checkout.git_failed", "args", strings.Join(args, " "), "stderr", strings.TrimSpace(stderr.String()), "err", err)
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
