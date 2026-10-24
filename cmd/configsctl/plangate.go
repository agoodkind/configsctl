package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"goodkind.io/configsctl/internal/procgroup"
	"goodkind.io/configsctl/internal/tofulock"
)

const (
	guestWorkspaceName      = "guest"
	serviceNameVariable     = "service_name"
	tofuPendingExitCode     = 2
	gatePlanFileName        = "gate.tfplan"
	gatePlanDirPattern      = "configsctl-gate-*"
	gateSkipReasonUnmanaged = "the hosts are not values of guest_hosts"
)

// planGateError combines guest plan rejections into one deploy refusal.
// The deploy command reports the plan log path for this error type.
type planGateError struct {
	rejections []error
}

func (e *planGateError) Error() string {
	messages := make([]string, 0, len(e.rejections))
	for _, rejection := range e.rejections {
		messages = append(messages, rejection.Error())
	}
	return "the guest plan gate refused the deploy: " + strings.Join(messages, "; ")
}

type gatedGuest struct {
	host        string
	serviceName string
	workspace   string
	module      string
}

type guestPlanRequest struct {
	workspaceDir string
	env          []string
	module       string
	output       io.Writer
}

// gateGuestPlans runs targeted plans for selected playbook hosts listed as guest_hosts values.
// The function returns a planGateError for failed plans or pending changes.
func gateGuestPlans(ctx context.Context, targets deployTargets, output io.Writer) error {
	tofu := targets.settings.Tofu
	if len(tofu.guestPlanModules()) == 0 {
		slog.Info("deploy.gate.disabled", "reason", "guest deployment workspaces are not configured")
		return nil
	}
	guests, err := gatedGuests(targets)
	if err != nil {
		return err
	}
	if len(guests) == 0 {
		return nil
	}
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	if output == nil {
		output = os.Stdout
	}
	var rejections []error
	for _, guest := range guests {
		request, err := guestWorkspaceRequest(tofu, guest, passwordFile)
		if err != nil {
			return err
		}
		request.env = append(os.Environ(), request.env...)
		request.output = output
		if err := gateGuestPlan(ctx, request, guest); err != nil {
			slog.Error("deploy.gate.rejected", "guest", guest.serviceName, "host", guest.host, "err", err)
			rejections = append(rejections, err)
		}
	}
	if len(rejections) > 0 {
		return &planGateError{rejections: rejections}
	}
	return nil
}

func gatedGuests(targets deployTargets) ([]gatedGuest, error) {
	owners, err := targets.settings.Tofu.guestPlanOwners()
	if err != nil {
		return nil, err
	}
	enrolledHosts := slices.Collect(maps.Values(targets.settings.Tofu.guestHosts()))
	var guests []gatedGuest
	var skipped []string
	for _, host := range targets.playHosts {
		if !slices.Contains(enrolledHosts, host) {
			skipped = append(skipped, host)
			continue
		}
		owner, found := owners[host]
		if !found {
			return nil, fmt.Errorf("enrolled host %s does not have a deployment workspace in tofu.guest_modules", host)
		}
		serviceName, err := targets.inventory.HostString(host, serviceNameVariable)
		if err != nil {
			slog.Error("deploy.gate.service_name_failed", "host", host, "err", err)
			return nil, fmt.Errorf("read the guest name of %s for the plan gate: %w", host, err)
		}
		guests = append(guests, gatedGuest{host: host, serviceName: serviceName, workspace: owner.workspace, module: owner.module})
	}
	if len(skipped) > 0 {
		slog.Info("deploy.gate.hosts_skipped", "hosts", skipped, "reason", gateSkipReasonUnmanaged)
	}
	return guests, nil
}

func gateGuestPlan(ctx context.Context, request guestPlanRequest, guest gatedGuest) error {
	planDir, err := os.MkdirTemp("", gatePlanDirPattern)
	if err != nil {
		slog.Error("deploy.gate.plan_dir_failed", "err", err)
		return fmt.Errorf("create a directory for the plan of guest %s: %w", guest.serviceName, err)
	}
	defer removeGatePlan(planDir)
	planPath := filepath.Join(planDir, gatePlanFileName)
	target := request.module + "[" + strconv.Quote(guest.serviceName) + "]"
	args, err := sanitizeTofuArgs([]string{
		string(tofuPlan), "-detailed-exitcode", "-input=false", "-target=" + target, "-out=" + planPath,
	})
	if err != nil {
		return err
	}
	slog.Info("deploy.gate.plan", "dir", request.workspaceDir, "guest", guest.serviceName, "host", guest.host,
		"args", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = request.workspaceDir
	cmd.Env = request.env
	var stderr bytes.Buffer
	cmd.Stdout = request.output
	cmd.Stderr = io.MultiWriter(request.output, &stderr)
	runErr := procgroup.RunGroup(cmd)
	if runErr == nil {
		if err := requirePlannedGuest(ctx, request, planPath, target); err != nil {
			return err
		}
		slog.Info("deploy.gate.passed", "guest", guest.serviceName, "host", guest.host)
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == tofuPendingExitCode {
		return pendingChangesError(ctx, request, guest, planPath)
	}
	slog.Error("deploy.gate.plan_failed", "guest", guest.serviceName, "host", guest.host,
		"stderr", strings.TrimSpace(stderr.String()), "err", runErr)
	return fmt.Errorf("the plan of guest %s (host %s) failed: %w", guest.serviceName, guest.host, runErr)
}

func pendingChangesError(ctx context.Context, request guestPlanRequest, guest gatedGuest, planPath string) error {
	showRequest := tofuLockRequest{workspaceDir: request.workspaceDir, env: request.env, args: nil, lockTargets: nil}
	plan, err := showTofuPlan(ctx, showRequest, planPath)
	if err != nil {
		return err
	}
	addresses, err := tofulock.PendingAddresses(plan)
	if err != nil {
		slog.Error("deploy.gate.plan_read_failed", "guest", guest.serviceName, "host", guest.host, "err", err)
		return fmt.Errorf("read the plan of guest %s (host %s): %w", guest.serviceName, guest.host, err)
	}
	return fmt.Errorf("guest %s (host %s) has %d pending resource changes: %s",
		guest.serviceName, guest.host, len(addresses), strings.Join(addresses, ", "))
}

func removeGatePlan(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("deploy.gate.plan_remove_failed", "dir", dir, "err", err)
	}
}
