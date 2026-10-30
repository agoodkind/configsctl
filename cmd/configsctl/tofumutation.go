package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/runid"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
	"goodkind.io/configsctl/internal/vault"
)

const mutationPlanName = "mutation.tfplan"

var mutationApplyOnlyFlags = map[string]bool{"backup": true, "state-out": true, "suppress-forget-errors": true}

var mutationSharedFlags = map[string]bool{
	"lock": true, "lock-timeout": true, "input": true, "parallelism": true,
	"state": true, "no-color": true, "compact-warnings": true, "concise": true,
	"consolidate-warnings": true, "consolidate-errors": true, "show-sensitive": true,
	"json": true, "json-into": true, "deprecation": true,
}

func tofuMutates(args []string) bool {
	command := tofuargs.Subcommand(args)
	return command == tofuargs.Apply || command == tofuargs.Destroy
}

func needsIndependentPlan(independent bool, args []string) bool {
	return independent && tofuMutates(args) && savedPlanCandidate(args) == ""
}

func runIndependentMutation(env cmdEnv, request tofuLockRequest, credential tofuCredentialSettings, usesCredential bool, passwordFile string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if tofuargs.BooleanArgument(request.args, "help", false) || tofuargs.BooleanArgument(request.args, "h", false) {
		return executeTofu(ctx, env, request.workspaceDir, request.args, request.args, request.env)
	}
	targets, err := mutationLockTargets(ctx, request)
	if err != nil {
		return err
	}
	if usesCredential {
		return runCredentialMutation(ctx, env, request, targets, credential, passwordFile)
	}
	return applyIndependentPlan(ctx, env, request, targets)
}

func mutationLockTargets(ctx context.Context, request tofuLockRequest) (tofulock.Targets, error) {
	targets, declared := request.lockTargets[tofuWorkspaceKey(request.workspaceDir)]
	if !declared {
		return tofulock.Targets{}, fmt.Errorf("workspace %s has no declared lock ownership", request.workspaceDir)
	}
	inv, err := ansible.LoadInventory(ctx, "")
	if err != nil {
		slog.Error("tofu.mutation.inventory_failed", "err", err)
		return tofulock.Targets{}, fmt.Errorf("load inventory for mutation ownership: %w", err)
	}
	return guestLockTargets(inv, request, targets)
}

func runCredentialMutation(ctx context.Context, env cmdEnv, request tofuLockRequest, targets tofulock.Targets, credential tofuCredentialSettings, passwordFile string) error {
	if os.Getenv(runid.EnvVar) == "" {
		identifier, err := runid.New()
		if err != nil {
			slog.Error("tofu.mutation.run_id_failed", "err", err)
			return fmt.Errorf("create the mutation run identity: %w", err)
		}
		if err := os.Setenv(runid.EnvVar, identifier); err != nil {
			slog.Error("tofu.mutation.run_id_failed", "err", err)
			return fmt.Errorf("set the mutation run identity: %w", err)
		}
		defer func() { _ = os.Unsetenv(runid.EnvVar) }()
	}
	locked, release, err := lockCredentialHost(ctx, credential.LockHost)
	if err != nil {
		return err
	}
	defer release()
	var patterns []redact.Pattern
	request.env, patterns, err = prepareMutationCredential(locked, credential, request.env, passwordFile)
	if err != nil {
		return err
	}
	env.secrets = append(env.secrets, patterns...)
	restore, err := installRedaction(patterns)
	if err != nil {
		return err
	}
	defer restore()
	return applyIndependentPlan(locked, env, request, targets)
}

func applyIndependentPlan(ctx context.Context, env cmdEnv, request tofuLockRequest, targets tofulock.Targets) error {
	dir, err := os.MkdirTemp("", "configsctl-mutation-*")
	if err != nil {
		slog.Error("tofu.mutation.plan_directory_failed", "err", err)
		return fmt.Errorf("create a private mutation plan directory: %w", err)
	}
	defer removePlanCopy(dir)
	planPath := filepath.Join(dir, mutationPlanName)
	planArgs, applyArgs, err := mutationArguments(request.args, planPath)
	if err != nil {
		return err
	}
	planErr := executeTofu(ctx, env, request.workspaceDir, request.args, planArgs, request.env)
	changed := false
	if planErr != nil {
		var status *exitCodeError
		if !errors.As(planErr, &status) || status.code != tofuPendingExitCode {
			return planErr
		}
		changed = true
	}
	plan, err := showTofuPlan(ctx, request, planPath)
	if err != nil {
		return err
	}
	decision := tofulock.DecideIndependent(plan, targets)
	if decision.AllHosts {
		return fmt.Errorf("workspace %s has unknown lock ownership: %s", request.workspaceDir, decision.Reason)
	}
	if changed && !tofuargs.AutoApprove(request.args) {
		if err := confirmMutation(ctx, request.args, request.env); err != nil {
			return err
		}
	}
	displayArgs := request.args
	request.args = applyArgs
	locked, commandArgs, release, err := lockTofuHosts(ctx, request)
	if err != nil {
		return err
	}
	defer release()
	return executeTofu(locked, env, request.workspaceDir, displayArgs, commandArgs, request.env)
}

func (t tofuSettings) lockGuestModule(workspaceDir string) string {
	for selector, module := range t.GuestModules {
		if tofuWorkspaceKey(filepath.Join(t.WorkspacesDir, selector)) == tofuWorkspaceKey(workspaceDir) {
			return module
		}
	}
	return ""
}

func prepareMutationCredential(ctx context.Context, credential tofuCredentialSettings, environment []string, passwordFile string) ([]string, []redact.Pattern, error) {
	password, err := vault.Secret(credential.Dell.RotatorPasswordKey, defaultVaultFile, passwordFile)
	if err != nil {
		slog.Error("tofu.credential.vault_failed", "err", err)
		return nil, nil, fmt.Errorf("read rotator credential: %w", err)
	}
	return credential.prepare(ctx, password, environment)
}

func mutationArguments(args []string, planPath string) ([]string, []string, error) {
	plan := []string{string(tofuargs.Plan)}
	apply := []string{string(tofuargs.Apply)}
	if tofuargs.Subcommand(args) == tofuargs.Destroy {
		plan = append(plan, "-destroy")
	}
	for index := 1; index < len(args); index++ {
		argument := args[index]
		name, _, _ := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if name == "auto-approve" {
			if _, value, found := strings.Cut(argument, "="); found {
				if _, err := strconv.ParseBool(value); err != nil {
					slog.Error("tofu.mutation.approval_flag_invalid", "err", err)
					return nil, nil, fmt.Errorf("invalid -auto-approve value: %w", err)
				}
			}
			continue
		}
		arguments := []string{argument}
		if tofuargs.ConsumesValue(argument) {
			index++
			if index >= len(args) {
				return nil, nil, fmt.Errorf("OpenTofu option %s requires a value", argument)
			}
			arguments = append(arguments, args[index])
		}
		switch {
		case mutationApplyOnlyFlags[name]:
			apply = append(apply, arguments...)
		case mutationSharedFlags[name]:
			plan = append(plan, arguments...)
			apply = append(apply, arguments...)
		default:
			plan = append(plan, arguments...)
		}
	}
	plan = append(plan, "-detailed-exitcode", "-out="+planPath)
	apply = append(apply, planPath)
	return plan, apply, nil
}

func confirmMutation(ctx context.Context, args, environment []string) error {
	input := environmentValue(environment, "TF_INPUT")
	if !tofuargs.BooleanArgument(args, "input", input != "false" && input != "0") || tofuargs.BooleanArgument(args, "json", false) {
		return errors.New("OpenTofu requires -auto-approve for a noninteractive apply with pending changes")
	}
	fmt.Fprint(os.Stdout, "\nApprove these planned actions by entering 'yes'.\n\n  Enter a value: ")
	type response struct {
		text string
		err  error
	}
	answers := make(chan response, 1)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				err := fmt.Errorf("read confirmation: %v", failure)
				slog.Error("tofu.mutation.confirmation_panicked", "err", err)
				answers <- response{text: "", err: err}
			}
		}()
		text, err := bufio.NewReader(os.Stdin).ReadString('\n')
		answers <- response{text: text, err: err}
	}()
	select {
	case <-ctx.Done():
		slog.Error("tofu.mutation.confirmation_stopped", "err", ctx.Err())
		return fmt.Errorf("OpenTofu confirmation stopped: %w", ctx.Err())
	case answer := <-answers:
		if answer.err != nil {
			slog.Error("tofu.mutation.confirmation_failed", "err", answer.err)
			return fmt.Errorf("read OpenTofu confirmation: %w", answer.err)
		}
		if strings.TrimSpace(answer.text) != "yes" {
			return errors.New("OpenTofu apply canceled")
		}
		return nil
	}
}
