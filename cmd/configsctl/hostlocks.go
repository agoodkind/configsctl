package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/gate"
	"goodkind.io/configsctl/internal/runid"
	"goodkind.io/configsctl/internal/tofulock"
)

type tofuLockRequest struct {
	workspaceDir          string
	env                   []string
	args                  []string
	lockTargets           map[string]tofulock.Targets
	requiredHosts         []string
	independentWorkspaces bool
	guestModule           string
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

// Run locked work with the returned context and call release after it returns.
// Lock loss cancels that context with hostlock.ErrLost.
func takeLocks(ctx context.Context, hosts []hostlock.Host) (locked context.Context, release func(), err error) {
	set, run, err := acquireLockSet(ctx, hosts)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Setenv(hostlock.StateEnvVar, set.StateDir()); err != nil {
		set.ReleaseAll(context.WithoutCancel(ctx))
		slog.Error("lock.state_export_failed", "run", run, "err", err)
		return nil, nil, fmt.Errorf("export %s to the child processes: %w", hostlock.StateEnvVar, err)
	}
	lockedCtx, release := keepLocksRenewed(ctx, set)
	return lockedCtx, release, nil
}

// acquireLockSet locks every host for the current run and returns the Set and
// the run id. It does not export hostlock.StateEnvVar.
func acquireLockSet(ctx context.Context, hosts []hostlock.Host) (*hostlock.Set, string, error) {
	run, err := runid.Current()
	if err != nil {
		slog.Error("lock.run_id_failed", "err", err)
		return nil, "", fmt.Errorf("create the run id for host locks: %w", err)
	}
	controller, err := controllerName()
	if err != nil {
		return nil, "", err
	}
	set, err := hostlock.AcquireAll(ctx, hosts, run, controller, hostlock.DefaultLease())
	if err != nil {
		slog.Error("lock.acquire_failed", "run", run, "err", err)
		return nil, "", fmt.Errorf("lock the target hosts: %w", err)
	}
	return set, run, nil
}

// keepLocksRenewed renews the locks of set until the returned release function
// runs. Lock loss cancels the returned context.
func keepLocksRenewed(ctx context.Context, set *hostlock.Set) (context.Context, func()) {
	lockedCtx, cancel := context.WithCancelCause(ctx)
	stop := set.KeepRenewed(ctx, cancel)
	return lockedCtx, func() {
		stop()
		cancel(nil)
		set.ReleaseAll(context.WithoutCancel(ctx))
	}
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
