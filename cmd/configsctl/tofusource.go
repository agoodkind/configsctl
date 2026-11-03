package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/configsctl/internal/gate"
	"goodkind.io/configsctl/internal/tofuargs"
)

type tofuSourceStatus string

const (
	tofuSourceCommitted tofuSourceStatus = "committed"
	tofuSourcePreview   tofuSourceStatus = "preview"
)

type tofuPlanSource struct {
	Status    tofuSourceStatus `json:"status"`
	Commit    string           `json:"commit"`
	Workspace string           `json:"workspace"`
}

func requireMergedTofuSource(ctx context.Context, workspace string, args []string, changesCredential bool) error {
	if (!tofuWritesState(args) && !changesCredential) || tofuCommandHelp(args) {
		return nil
	}
	for _, argument := range args {
		if argument == "--" {
			break
		}
		option, _, _ := strings.Cut(argument, "=")
		if option == "-chdir" || option == "--chdir" {
			return errors.New("select the configuration with configsctl tofu <workspace>; -chdir cannot change production")
		}
	}
	repository := gate.Repo{Dir: "."}
	revision, err := repository.CleanRevision(ctx)
	if err != nil {
		slog.Error("tofu.source.checkout_refused", "err", err)
		return fmt.Errorf("verify merged OpenTofu source: %w", err)
	}
	if err := repository.Fetch(ctx); err != nil {
		slog.Error("tofu.source.fetch_failed", "err", err)
		return fmt.Errorf("refresh origin/main before changing production: %w", err)
	}
	if err := repository.RequireOnMain(ctx, revision); err != nil {
		slog.Error("tofu.source.unmerged", "commit", revision, "err", err)
		return fmt.Errorf("merge the configuration before changing production: %w", err)
	}
	if _, err := tofuSourceWorkspace(ctx, workspace); err != nil {
		return err
	}
	if candidate := savedPlanCandidate(args); candidate != "" {
		return requireTofuPlanSource(ctx, workspace, workspacePath(workspace, candidate))
	}
	return nil
}

func tofuCommandHelp(args []string) bool {
	return tofuargs.BooleanArgument(args, "help", false) || tofuargs.BooleanArgument(args, "h", false)
}

func tofuSourceWorkspace(ctx context.Context, workspace string) (string, error) {
	repository := gate.Repo{Dir: "."}
	root, err := repository.Root(ctx)
	if err != nil {
		slog.Error("tofu.source.workspace_failed", "operation", "repository", "err", err)
		return "", fmt.Errorf("identify the configuration repository: %w", err)
	}
	directory, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		slog.Error("tofu.source.workspace_failed", "operation", "resolve", "err", err)
		return "", fmt.Errorf("resolve the configuration workspace: %w", err)
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		slog.Error("tofu.source.workspace_failed", "operation", "absolute", "err", err)
		return "", fmt.Errorf("resolve the absolute configuration workspace: %w", err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		slog.Error("tofu.source.workspace_failed", "operation", "relative", "err", err)
		return "", fmt.Errorf("identify the configuration workspace: %w", err)
	}
	if !filepath.IsLocal(relative) {
		return "", errors.New("the OpenTofu workspace must be inside the configuration repository")
	}
	return filepath.ToSlash(relative), nil
}

func tofuPlanOutput(args []string) string {
	if tofuargs.Subcommand(args) != tofuargs.Plan || tofuCommandHelp(args) {
		return ""
	}
	var path string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		name, value, assigned := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if name == "out" && strings.HasPrefix(argument, "-") {
			if assigned {
				path = value
			} else if index+1 < len(args) {
				index++
				path = args[index]
			}
		} else if tofuargs.ConsumesValue(argument) {
			index++
		}
	}
	return path
}

func inspectTofuPlanSource(ctx context.Context, workspace string) (tofuPlanSource, error) {
	repository := gate.Repo{Dir: "."}
	checkout, err := repository.Snapshot(ctx)
	if err != nil {
		slog.Error("tofu.source.snapshot_failed", "err", err)
		return tofuPlanSource{}, fmt.Errorf("inspect the saved plan configuration: %w", err)
	}
	selected, err := tofuSourceWorkspace(ctx, workspace)
	if err != nil {
		return tofuPlanSource{}, err
	}
	status := tofuSourceCommitted
	if checkout.Dirty {
		status = tofuSourcePreview
	}
	return tofuPlanSource{Status: status, Commit: checkout.Commit, Workspace: selected}, nil
}

// The cache key binds source metadata to the exact plan bytes without writing
// generated metadata into the configuration checkout.
func tofuPlanSourcePath(path string) (string, error) {
	if err := requireRegularPlan(path); err != nil {
		return "", err
	}
	digest, err := planDigest(path)
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		slog.Error("tofu.source.cache_unavailable", "err", err)
		return "", fmt.Errorf("find the saved plan source cache: %w", err)
	}
	return filepath.Join(cache, "configsctl", "plan-source", fmt.Sprintf("%x.json", digest)), nil
}

func recordTofuPlanSource(ctx context.Context, workspace, path string, before tofuPlanSource) error {
	after, err := inspectTofuPlanSource(ctx, workspace)
	if err != nil {
		return errors.Join(err, removeTofuPlanSource(path))
	}
	if before != after {
		if err := removeTofuPlanSource(path); err != nil {
			return err
		}
		return errors.New("the configuration changed during planning; regenerate the saved plan")
	}
	if after.Status == tofuSourcePreview {
		slog.Warn("saved plan is preview only; configuration has uncommitted changes")
	}
	metadata, err := tofuPlanSourcePath(path)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("encode the saved plan source: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(metadata), 0o700); err != nil {
		return fmt.Errorf("create the saved plan source cache: %w", err)
	}
	if err := os.WriteFile(metadata, encoded, 0o600); err != nil {
		return fmt.Errorf("record the saved plan source: %w", err)
	}
	return nil
}

func removeTofuPlanSource(path string) error {
	metadata, err := tofuPlanSourcePath(path)
	if err != nil {
		return err
	}
	// A newly generated plan may not have a cached source record yet.
	if err := os.Remove(metadata); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("tofu.source.plan_record_remove_failed", "err", err)
		return fmt.Errorf("remove the stale saved plan source record: %w", err)
	}
	return nil
}

func requireTofuPlanSource(ctx context.Context, workspace, path string) error {
	metadata, err := tofuPlanSourcePath(path)
	if err != nil {
		return err
	}
	encoded, err := os.ReadFile(metadata)
	if err != nil {
		slog.Error("tofu.source.plan_record_unavailable", "err", err)
		return fmt.Errorf("regenerate the saved plan with configsctl tofu plan; its source record is unavailable: %w", err)
	}
	var source tofuPlanSource
	if err := json.Unmarshal(encoded, &source); err != nil {
		slog.Error("tofu.source.plan_record_invalid", "err", err)
		return fmt.Errorf("read the saved plan source record: %w", err)
	}
	current, err := inspectTofuPlanSource(ctx, workspace)
	if err != nil {
		return err
	}
	if source.Status != tofuSourceCommitted || current.Status != tofuSourceCommitted || source != current {
		return errors.New("regenerate the saved plan from this clean merged configuration commit; its recorded source does not match")
	}
	return nil
}
