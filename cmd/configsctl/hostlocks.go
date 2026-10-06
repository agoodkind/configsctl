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

const (
	hypervisorGroup   = "proxmox_servers"
	tofuLockOnlyGroup = "tofu_lock_hosts"
)

func allTofuLockHosts(inv ansible.Inventory) []string {
	names := slices.Concat(inv.GroupHosts(hypervisorGroup), inv.GroupHosts(tofuLockOnlyGroup))
	slices.Sort(names)
	return slices.Compact(names)
}

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

type tofuHostSelection struct {
	names   []string
	args    []string
	cleanup func()
}

func lockTofuHosts(
	ctx context.Context, request tofuLockRequest,
) (locked context.Context, args []string, release func(), err error) {
	inv, err := ansible.LoadInventory(ctx, "")
	if err != nil {
		slog.Error("tofu.lock.inventory_failed", "err", err)
		return nil, nil, nil, fmt.Errorf("load the inventory for hypervisor locks: %w", err)
	}
	selection, err := selectTofuLockHosts(ctx, inv, request)
	if err != nil {
		return nil, nil, nil, err
	}
	targets := hostlock.Targets(inv, selection.names)
	if err := requireLockOnlyTargets(inv, selection.names, targets); err != nil {
		selection.cleanup()
		return nil, nil, nil, err
	}
	lockedCtx, releaseLocks, err := takeLocks(ctx, targets)
	if err != nil {
		selection.cleanup()
		return nil, nil, nil, err
	}
	return lockedCtx, selection.args, func() {
		releaseLocks()
		selection.cleanup()
	}, nil
}

func requireLockOnlyTargets(inv ansible.Inventory, names []string, targets []hostlock.Host) error {
	targeted := map[string]bool{}
	for _, target := range targets {
		targeted[target.Name] = true
	}
	lockOnlyHosts := inv.GroupHosts(tofuLockOnlyGroup)
	for _, name := range names {
		if slices.Contains(lockOnlyHosts, name) && !targeted[name] {
			err := fmt.Errorf("host %s from inventory group %s has no ssh lock target", name, tofuLockOnlyGroup)
			slog.Error("tofu.lock.lock_only_host_untargeted", "host", name, "group", tofuLockOnlyGroup, "err", err)
			return err
		}
	}
	return nil
}

func selectTofuLockHosts(ctx context.Context, inv ansible.Inventory, request tofuLockRequest) (tofuHostSelection, error) {
	everyHost := tofuHostSelection{names: allTofuLockHosts(inv), args: request.args, cleanup: func() {}}
	if tofuSubcommand(request.args) != tofuApply {
		return everyHost, nil
	}
	workspace := tofuWorkspaceKey(request.workspaceDir)
	targets, declared := request.lockTargets[workspace]
	if !declared {
		return everyHost, nil
	}
	planFile := savedPlanCandidate(request.args)
	if planFile == "" {
		slog.Info("tofu.lock.all_hypervisors", "workspace", workspace, "reason", "the apply has no saved plan")
		return everyHost, nil
	}
	if err := requireRegularPlan(workspacePath(request.workspaceDir, planFile)); err != nil {
		return tofuHostSelection{}, err
	}
	copyDir, copyPath, err := copySavedPlan(request.workspaceDir, planFile)
	if err != nil {
		return tofuHostSelection{}, err
	}
	cleanup := func() { removePlanCopy(copyDir) }
	slog.Info("tofu.lock.plan_copied", "workspace", workspace, "plan", planFile, "copy", copyPath)
	args := slices.Clone(request.args)
	args[len(args)-1] = copyPath
	decision, err := planDecision(ctx, inv, request, targets, copyPath)
	if err != nil {
		cleanup()
		return tofuHostSelection{}, err
	}
	names := decision.Hosts
	if decision.AllHosts {
		names = everyHost.names
	}
	return tofuHostSelection{names: names, args: args, cleanup: cleanup}, nil
}

func planDecision(
	ctx context.Context, inv ansible.Inventory, request tofuLockRequest, targets tofulock.Targets, planPath string,
) (tofulock.Decision, error) {
	workspace := tofuWorkspaceKey(request.workspaceDir)
	plan, err := showTofuPlan(ctx, request, planPath)
	if err != nil {
		return tofulock.Decision{}, err
	}
	decision := tofulock.Decide(plan, targets)
	if decision.AllHosts {
		slog.Info("tofu.lock.all_hypervisors", "workspace", workspace, "reason", decision.Reason)
		return decision, nil
	}
	slog.Info("tofu.lock.plan_hosts", "workspace", workspace, "plan", planPath, "hosts", decision.Hosts)
	return decision, requireLockTargets(inv, decision.Hosts)
}

func requireRegularPlan(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		slog.Error("tofu.lock.plan_stat_failed", "plan", path, "err", err)
		return fmt.Errorf("read the saved plan %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		err := fmt.Errorf("the saved plan %s is not a regular file", path)
		slog.Error("tofu.lock.plan_not_regular", "plan", path, "err", err)
		return err
	}
	return nil
}

func copySavedPlan(workspaceDir, planFile string) (string, string, error) {
	dir, err := os.MkdirTemp("", "configsctl-plan-*")
	if err != nil {
		slog.Error("tofu.lock.plan_copy_dir_failed", "err", err)
		return "", "", fmt.Errorf("create a directory for the saved plan copy: %w", err)
	}
	copyPath := filepath.Join(dir, filepath.Base(planFile))
	if err := copyPrivateFile(workspacePath(workspaceDir, planFile), copyPath); err != nil {
		removePlanCopy(dir)
		return "", "", err
	}
	return dir, copyPath, nil
}

func copyPrivateFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		slog.Error("tofu.lock.plan_read_failed", "plan", source, "err", err)
		return fmt.Errorf("read the saved plan %s: %w", source, err)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		slog.Error("tofu.lock.plan_copy_failed", "copy", destination, "err", err)
		return fmt.Errorf("create the saved plan copy %s: %w", destination, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		slog.Error("tofu.lock.plan_copy_failed", "copy", destination, "err", err)
		return fmt.Errorf("write the saved plan copy %s: %w", destination, err)
	}
	if err := file.Close(); err != nil {
		slog.Error("tofu.lock.plan_copy_failed", "copy", destination, "err", err)
		return fmt.Errorf("close the saved plan copy %s: %w", destination, err)
	}
	return nil
}

func removePlanCopy(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("tofu.lock.plan_copy_remove_failed", "dir", dir, "err", err)
	}
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
