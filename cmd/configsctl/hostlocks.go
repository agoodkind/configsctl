package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/gate"
	"goodkind.io/configsctl/internal/hostlock"
	"goodkind.io/configsctl/internal/runid"
	"goodkind.io/configsctl/internal/tofulock"
)

// Destroy locks every host in hypervisorGroup even when tofu.lock_targets
// declares hosts for the workspace.
const hypervisorGroup = "proxmox_servers"

// Check-mode deployments do not acquire host locks.
// Lock loss cancels the local command before lock release. Remote Ansible
// modules may continue running.
func lockedDeploy(opts ansible.DeployOptions) error {
	ctx := context.Background()
	if !opts.Check {
		lockedCtx, release, err := lockPlayHosts(ctx, opts)
		if err != nil {
			return err
		}
		defer release()
		ctx = lockedCtx
	}
	if err := ansible.Deploy(ctx, opts); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		slog.Error("deploy.failed", "playbook", opts.Playbook, "err", err)
		return fmt.Errorf("deploy %s: %w", opts.Playbook, err)
	}
	return nil
}

func lockPlayHosts(ctx context.Context, opts ansible.DeployOptions) (context.Context, func(), error) {
	loaded, err := loadSettings(settingsFile)
	if err != nil {
		return nil, nil, err
	}
	inv, err := ansible.LoadInventory(ctx, "")
	if err != nil {
		slog.Error("deploy.lock.inventory_failed", "err", err)
		return nil, nil, fmt.Errorf("load the inventory for host locks: %w", err)
	}
	names, err := ansible.PlayHosts(ctx, opts)
	if err != nil {
		slog.Error("deploy.lock.hosts_failed", "playbook", opts.Playbook, "err", err)
		return nil, nil, fmt.Errorf("list the hosts of %s: %w", opts.Playbook, err)
	}
	variables := loaded.Deploy.LockHostVariables[ansible.PlaybookName(opts.Playbook)]
	hosts, err := deployLockTargets(inv, names, variables)
	if err != nil {
		return nil, nil, err
	}
	return takeLocks(ctx, hosts)
}

func deployLockTargets(inv ansible.Inventory, playHosts, variables []string) ([]hostlock.Host, error) {
	extraHosts, err := inv.VariableHosts(playHosts, variables)
	if err != nil {
		slog.Error("deploy.lock.variable_read_failed", "variables", variables, "err", err)
		return nil, fmt.Errorf("read the lock host variables %v: %w", variables, err)
	}
	targeted := map[string]bool{}
	for _, target := range hostlock.Targets(inv, extraHosts) {
		targeted[target.Name] = true
	}
	for _, name := range extraHosts {
		if !targeted[name] {
			err := fmt.Errorf("host %s from a lock host variable has no ssh lock target", name)
			slog.Error("deploy.lock.variable_host_untargeted", "host", name, "err", err)
			return nil, err
		}
	}
	names := slices.Concat(playHosts, extraHosts)
	slices.Sort(names)
	return hostlock.Targets(inv, slices.Compact(names)), nil
}

type tofuLockRequest struct {
	workspaceDir string
	env          []string
	args         []string
	lockTargets  map[string]tofulock.Targets
}

func lockTofuHosts(ctx context.Context, request tofuLockRequest) (context.Context, func(), error) {
	inv, err := ansible.LoadInventory(ctx, "")
	if err != nil {
		slog.Error("tofu.lock.inventory_failed", "err", err)
		return nil, nil, fmt.Errorf("load the inventory for hypervisor locks: %w", err)
	}
	names, err := tofuLockHostNames(ctx, inv, request)
	if err != nil {
		return nil, nil, err
	}
	return takeLocks(ctx, hostlock.Targets(inv, names))
}

func tofuLockHostNames(ctx context.Context, inv ansible.Inventory, request tofuLockRequest) ([]string, error) {
	hypervisors := inv.GroupHosts(hypervisorGroup)
	if tofuSubcommand(request.args) != tofuApply {
		return hypervisors, nil
	}
	workspace := tofuWorkspaceKey(request.workspaceDir)
	targets, declared := request.lockTargets[workspace]
	if !declared {
		return hypervisors, nil
	}
	planFile := savedPlanFile(request.workspaceDir, request.args)
	if planFile == "" {
		slog.Info("tofu.lock.all_hypervisors", "workspace", workspace, "reason", "the apply has no saved plan")
		return hypervisors, nil
	}
	plan, err := showTofuPlan(ctx, request, planFile)
	if err != nil {
		return nil, err
	}
	decision := tofulock.Decide(plan, targets)
	if decision.AllHosts {
		slog.Info("tofu.lock.all_hypervisors", "workspace", workspace, "reason", decision.Reason)
		return hypervisors, nil
	}
	slog.Info("tofu.lock.plan_hosts", "workspace", workspace, "plan", planFile, "hosts", decision.Hosts)
	return decision.Hosts, requireLockTargets(inv, decision.Hosts)
}

func savedPlanFile(workspaceDir string, args []string) string {
	if len(args) == 0 || tofuSubcommand(args[:len(args)-1]) != tofuApply {
		return ""
	}
	candidate := args[len(args)-1]
	if strings.HasPrefix(candidate, "-") {
		return ""
	}
	path := candidate
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspaceDir, path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return candidate
}

func showTofuPlan(ctx context.Context, request tofuLockRequest, planFile string) ([]byte, error) {
	args, err := sanitizeTofuArgs([]string{"show", "-json", "-plan=" + planFile})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = request.workspaceDir
	cmd.Env = request.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Error("tofu.lock.show_failed", "dir", request.workspaceDir, "plan", planFile,
			"stderr", strings.TrimSpace(stderr.String()), "err", err)
		return nil, fmt.Errorf("read the saved plan %s: %w", planFile, err)
	}
	return stdout.Bytes(), nil
}

func requireLockTargets(inv ansible.Inventory, names []string) error {
	inventoryHosts := inv.GroupHosts("all")
	targeted := map[string]bool{}
	for _, target := range hostlock.Targets(inv, names) {
		targeted[target.Name] = true
	}
	for _, name := range names {
		_, hasVariables := inv.Hosts[name]
		if !hasVariables && !slices.Contains(inventoryHosts, name) {
			err := fmt.Errorf("host %s from tofu.lock_targets is not an inventory host", name)
			slog.Error("tofu.lock.host_unknown", "host", name, "err", err)
			return err
		}
		if !targeted[name] {
			err := fmt.Errorf("host %s from tofu.lock_targets has no ssh lock target", name)
			slog.Error("tofu.lock.host_untargeted", "host", name, "err", err)
			return err
		}
	}
	return nil
}

// Run locked work with the returned context and call release after it returns.
// Lock loss cancels that context with hostlock.ErrLost.
func takeLocks(ctx context.Context, hosts []hostlock.Host) (locked context.Context, release func(), err error) {
	run, err := runid.Current()
	if err != nil {
		slog.Error("lock.run_id_failed", "err", err)
		return nil, nil, fmt.Errorf("create the run id for host locks: %w", err)
	}
	controller, err := controllerName()
	if err != nil {
		return nil, nil, err
	}
	set, err := hostlock.AcquireAll(ctx, hosts, run, controller)
	if err != nil {
		slog.Error("lock.acquire_failed", "run", run, "err", err)
		return nil, nil, fmt.Errorf("lock the target hosts: %w", err)
	}
	lockedCtx, cancel := context.WithCancelCause(ctx)
	stop := set.KeepRenewed(ctx, cancel)
	return lockedCtx, func() {
		stop()
		cancel(nil)
		set.ReleaseAll(context.WithoutCancel(ctx))
	}, nil
}

// controllerName returns the controller name from the run environment, or the
// host name of this machine.
func controllerName() (string, error) {
	if name := os.Getenv(gate.ControllerEnvVar); name != "" {
		return name, nil
	}
	name, err := os.Hostname()
	if err != nil {
		slog.Error("lock.hostname_unavailable", "err", err)
		return "", fmt.Errorf("read the host name for the lock owner: %w", err)
	}
	return name, nil
}
