package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/gate"
	"goodkind.io/configsctl/internal/hostlock"
	"goodkind.io/configsctl/internal/runid"
)

// hypervisorGroup is the inventory group of every Proxmox hypervisor. An
// OpenTofu apply locks each host in it.
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
	return takeLocks(ctx, hostlock.Targets(inv, names))
}

func lockHypervisors(ctx context.Context) (context.Context, func(), error) {
	inv, err := ansible.LoadInventory(ctx, "")
	if err != nil {
		slog.Error("tofu.lock.inventory_failed", "err", err)
		return nil, nil, fmt.Errorf("load the inventory for hypervisor locks: %w", err)
	}
	return takeLocks(ctx, hostlock.Targets(inv, inv.GroupHosts(hypervisorGroup)))
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
