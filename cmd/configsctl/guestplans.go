package main

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

type guestPlanOwner struct {
	workspace string
	module    string
}

func (t tofuSettings) guestPlanModules() map[string]string {
	if t.GuestModules != nil {
		return t.GuestModules
	}
	if t.GuestModule != "" {
		return map[string]string{guestWorkspaceName: t.GuestModule}
	}
	return nil
}

func validateGuestPlans(tofu tofuSettings) error {
	if tofu.GuestModules == nil {
		return nil
	}
	if tofu.GuestModule != "" {
		return errors.New("tofu.guest_module and tofu.guest_modules cannot both be set")
	}
	if len(tofu.GuestModules) == 0 {
		return errors.New("tofu.guest_modules is empty")
	}
	_, err := tofu.guestPlanOwners()
	return err
}

// guestPlanOwners binds inventory hosts to their explicit workspace and module.
// It refuses duplicate ownership before running a plan or opening credentials.
func (t tofuSettings) guestPlanOwners() (map[string]guestPlanOwner, error) {
	owners := make(map[string]guestPlanOwner)
	modules := t.guestPlanModules()
	for _, workspace := range slices.Sorted(maps.Keys(modules)) {
		if err := validateWorkspaceSelector(workspace); err != nil {
			return nil, err
		}
		module := modules[workspace]
		if module == "" {
			return nil, fmt.Errorf("tofu.guest_modules.%s has an empty module address", workspace)
		}
		traversal, diagnostics := hclsyntax.ParseTraversalAbs([]byte(module), "guest module address", hcl.InitialPos)
		if diagnostics.HasErrors() || len(traversal) < 2 || traversal.RootName() != "module" || !strings.HasPrefix(module, "module.") {
			return nil, fmt.Errorf("tofu.guest_modules.%s requires a valid module address", workspace)
		}
		key := tofuWorkspaceKey(filepath.Join(t.WorkspacesDir, workspace))
		hosts := t.LockTargets[key].GuestHosts
		if len(hosts) == 0 {
			return nil, fmt.Errorf("tofu.guest_modules.%s requires tofu.lock_targets.%s.guest_hosts", workspace, key)
		}
		for _, guest := range slices.Sorted(maps.Keys(hosts)) {
			host := hosts[guest]
			if previous, found := owners[host]; found {
				return nil, fmt.Errorf("guest host %s has multiple workspace owners: %s and %s", host, previous.workspace, workspace)
			}
			owners[host] = guestPlanOwner{workspace: workspace, module: module}
		}
	}
	return owners, nil
}

func guestWorkspaceRequest(tofu tofuSettings, guest gatedGuest, passwordFile string) (guestPlanRequest, error) {
	dir, found, err := selectedWorkspaceDir(tofu.WorkspacesDir, guest.workspace)
	if err != nil {
		return guestPlanRequest{}, err
	}
	if !found {
		return guestPlanRequest{}, fmt.Errorf("deployment workspace %s for host %s does not exist", guest.workspace, guest.host)
	}
	secretEnv, err := tofuSecretEnv(tofu, dir, defaultVaultFile, passwordFile)
	if err != nil {
		slog.Error("deploy.gate.workspace_credentials_failed", "workspace", guest.workspace, "err", err)
		return guestPlanRequest{}, err
	}
	return guestPlanRequest{workspaceDir: dir, env: secretEnv, module: guest.module, output: nil}, nil
}
