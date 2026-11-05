package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"goodkind.io/configsctl/internal/gate"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofuplansource"
)

type tofuSourceStatus = string

const (
	tofuSourceCommitted tofuSourceStatus = "committed"
	tofuSourcePreview   tofuSourceStatus = "preview"
)

type tofuPlanSource = tofuplansource.Source

func requireMergedTofuSource(ctx context.Context, workspace string, args []string, changesCredential bool) error {
	if installedSourceFrom(ctx) != nil {
		if _, err := inspectTofuPlanSource(ctx, workspace); err != nil {
			return err
		}
		if candidate := savedPlanCandidate(args); candidate != "" {
			return requireTofuPlanSource(ctx, workspace, workspacePath(workspace, candidate))
		}
		return nil
	}
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
	if source := installedSourceFrom(ctx); source != nil {
		return installedWorkspace(source, workspace)
	}
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
	if source := installedSourceFrom(ctx); source != nil {
		identity, err := source.inspect(ctx)
		if err != nil {
			return tofuPlanSource{}, err
		}
		identity.Workspace, err = installedWorkspace(source, workspace)
		return identity, err
	}
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
	return tofuPlanSource{
		Status: status, Commit: checkout.Commit, Workspace: selected,
		Kind: "", Root: "", Artifact: "", Node: "", Environment: "", ManifestSHA256: "",
	}, nil
}

func recordTofuPlanSource(ctx context.Context, workspace, path string, before tofuPlanSource) error {
	after, err := inspectTofuPlanSource(ctx, workspace)
	if err != nil {
		return errors.Join(err, tofuplansource.Remove(path))
	}
	if before != after {
		if err := tofuplansource.Remove(path); err != nil {
			return fmt.Errorf("remove the stale plan source: %w", err)
		}
		return errors.New("the configuration changed during planning; regenerate the saved plan")
	}
	if after.Status == tofuSourcePreview {
		slog.Warn("saved plan is preview only; configuration has uncommitted changes")
	}
	if err := tofuplansource.Record(path, after); err != nil {
		return fmt.Errorf("record the saved OpenTofu plan source: %w", err)
	}
	return nil
}

func requireTofuPlanSource(ctx context.Context, workspace, path string) error {
	current, err := inspectTofuPlanSource(ctx, workspace)
	if err != nil {
		return err
	}
	if err := tofuplansource.Require(path, current); err != nil {
		slog.Error("tofu.source.plan_refused", "err", err)
		return fmt.Errorf("verify the saved OpenTofu plan source: %w", err)
	}
	return nil
}
