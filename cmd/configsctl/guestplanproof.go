package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

type guestPlanDocument struct {
	FormatVersion string `json:"format_version"`
	PlannedValues struct {
		RootModule guestPlanModule `json:"root_module"`
	} `json:"planned_values"`
}

type guestPlanModule struct {
	Address      string            `json:"address"`
	ChildModules []guestPlanModule `json:"child_modules"`
}

// requirePlannedGuest rejects an empty target plan that did not inspect the
// selected guest module. OpenTofu can return success for a nonexistent target.
func requirePlannedGuest(ctx context.Context, request guestPlanRequest, path, target string) error {
	showRequest := tofuLockRequest{workspaceDir: request.workspaceDir, env: request.env, args: nil, lockTargets: nil, requiredHosts: nil}
	encoded, err := showTofuPlan(ctx, showRequest, path)
	if err != nil {
		return err
	}
	var document guestPlanDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		slog.Error("deploy.gate.plan_decode_failed", "workspace", request.workspaceDir, "err", err)
		return fmt.Errorf("decode selected guest plan: %w", err)
	}
	if !strings.HasPrefix(document.FormatVersion, "1.") {
		return fmt.Errorf("selected guest plan has unsupported format %q", document.FormatVersion)
	}
	if !document.PlannedValues.RootModule.contains(target) {
		return fmt.Errorf("selected guest module %s is absent from the plan; check its module address and inventory service_name", target)
	}
	return nil
}

func (m guestPlanModule) contains(target string) bool {
	if m.Address == target {
		return true
	}
	return slices.ContainsFunc(m.ChildModules, func(child guestPlanModule) bool { return child.contains(target) })
}
