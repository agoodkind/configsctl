package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/vault"
)

const (
	mutationPlanName    = "mutation.tfplan"
	tofuPendingExitCode = 2
	guestWorkspaceName  = "guest"
)

func tofuMutates(args []string) bool {
	command := tofuargs.Subcommand(args)
	return command == tofuargs.Apply || command == tofuargs.Destroy
}

func runIndependentMutation(ctx context.Context, env cmdEnv, settings tofuSettings, request tofuLockRequest, passwordFile string) error {
	if tofuargs.BooleanArgument(request.args, "help", false) || tofuargs.BooleanArgument(request.args, "h", false) {
		return executeTofu(ctx, env, request.workspaceDir, request.args, request.args, request.env)
	}
	targets, declared := explicitLockTargets(request)
	if !declared {
		return fmt.Errorf("workspace %s requires native lock ownership", request.workspaceDir)
	}
	if !strictExplicitSettings(settings, targets) {
		return runNativeMutation(ctx, settings, request, targets, passwordFile)
	}
	return runExplicitMutation(ctx, settings, request, targets, passwordFile)
}

func (t tofuSettings) lockGuestModule(workspaceDir string) string {
	for selector, module := range t.GuestModules {
		if tofuWorkspaceKey(filepath.Join(t.WorkspacesDir, selector)) == tofuWorkspaceKey(workspaceDir) {
			return module
		}
	}
	return ""
}

func prepareMutationCredential(ctx context.Context, credential tofuCredentialSettings, workspace string, environment []string, passwordFile string) ([]string, []redact.Pattern, error) {
	password, err := vault.Secret(credential.Dell.RotatorPasswordKey, defaultVaultFile, passwordFile)
	if err != nil {
		slog.Error("tofu.credential.vault_failed", "err", err)
		return nil, nil, fmt.Errorf("read rotator credential: %w", err)
	}
	return credential.prepare(ctx, workspace, password, environment)
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
