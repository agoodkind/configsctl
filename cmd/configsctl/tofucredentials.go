package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/dellcredential"
	"goodkind.io/configsctl/internal/hostlock"
	"goodkind.io/configsctl/internal/redact"
)

type tofuCredentialSettings struct {
	LockHost         string                 `yaml:"lock_host"`
	UsernameVariable string                 `yaml:"username_variable"`
	PasswordVariable string                 `yaml:"password_variable"`
	Dell             *dellcredential.Config `yaml:"dell"`
}

func validateTofuCredentials(settings map[string]tofuCredentialSettings) error {
	for workspace, credential := range settings {
		if workspace == "" || tofuWorkspaceKey(workspace) != workspace {
			return errors.New("tofu.credentials requires clean workspace paths")
		}
		if credential.LockHost == "" || credential.UsernameVariable == "" || credential.PasswordVariable == "" || credential.UsernameVariable == credential.PasswordVariable {
			return fmt.Errorf("tofu.credentials.%s requires lock_host and distinct credential variables", workspace)
		}
		if credential.Dell == nil {
			return fmt.Errorf("tofu.credentials.%s requires a Dell credential adapter", workspace)
		}
		if err := credential.Dell.Validate(); err != nil {
			slog.Error("tofu.credential.config_invalid", "workspace", workspace, "err", err)
			return fmt.Errorf("tofu.credentials.%s: %w", workspace, err)
		}
	}
	return nil
}

func credentialCommand(args []string) bool {
	switch tofuSubcommand(args) {
	case tofuApply, tofuDestroy:
		return true
	case tofuPlan, tofuRefresh, "import":
		return true
	default:
		return false
	}
}

func (s tofuCredentialSettings) requireEphemeral(workspace string) error {
	blocks, err := tofuModuleBlocks(workspace, tofuVariableSchema)
	if err != nil {
		return err
	}
	for _, name := range []string{s.UsernameVariable, s.PasswordVariable} {
		found := false
		for _, block := range blocks {
			if block.Labels[0] != name {
				continue
			}
			content, _, diagnostics := block.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "ephemeral", Required: true}, {Name: "sensitive", Required: true}}})
			if diagnostics.HasErrors() {
				return fmt.Errorf("credential variable %s requires ephemeral = true and sensitive = true", name)
			}
			for _, attribute := range content.Attributes {
				value, diagnostics := attribute.Expr.Value(nil)
				if diagnostics.HasErrors() || !value.RawEquals(cty.True) {
					return fmt.Errorf("credential variable %s requires ephemeral = true and sensitive = true", name)
				}
			}
			found = true
		}
		if !found {
			return fmt.Errorf("credential variable %s does not exist in %s", name, workspace)
		}
	}
	return nil
}

func (s tofuCredentialSettings) prepare(ctx context.Context, password string, childEnv []string) ([]string, []redact.Pattern, error) {
	credential, err := s.Dell.Prepare(ctx, password)
	if err != nil {
		slog.Error("tofu.credential.prepare_failed", "err", err)
		return nil, nil, fmt.Errorf("prepare deployment credential: %w", err)
	}
	usernameKey := tofuVariablePrefix + s.UsernameVariable
	passwordKey := tofuVariablePrefix + s.PasswordVariable
	rotatorKey := tofuVariablePrefix + s.Dell.RotatorPasswordKey
	result := make([]string, 0, len(childEnv)+2)
	for _, assignment := range childEnv {
		key, _, _ := strings.Cut(assignment, "=")
		if key == usernameKey || key == passwordKey || key == rotatorKey {
			continue
		}
		result = append(result, assignment)
	}
	result = append(result, usernameKey+"="+credential.Username, passwordKey+"="+credential.Password)
	return result, []redact.Pattern{{Label: s.PasswordVariable, Value: []byte(credential.Password)}}, nil
}

func (s tofuCredentialSettings) validateWorkspace(workspace string, childEnv []string) error {
	if err := s.requireEphemeral(workspace); err != nil {
		return err
	}
	if environmentValue(childEnv, "TF_LOG_PATH") != "" {
		return errors.New("TF_LOG_PATH cannot write provider credentials outside configsctl redaction")
	}
	return nil
}

func lockCredentialHost(ctx context.Context, name string) (context.Context, func(), error) {
	inv, err := ansible.LoadInventory(ctx, "")
	if err != nil {
		slog.Error("tofu.credential.inventory_failed", "err", err)
		return nil, nil, fmt.Errorf("read inventory for credential lock: %w", err)
	}
	if err := requireLockTargets(inv, []string{name}); err != nil {
		return nil, nil, err
	}
	targets, err := hostlock.Targets(inv, []string{name})
	if err != nil {
		return nil, nil, fmt.Errorf("select credential host lock targets: %w", err)
	}
	return takeLocks(ctx, targets)
}

func environmentValue(environment []string, name string) string {
	value := ""
	for _, assignment := range environment {
		key, candidate, _ := strings.Cut(assignment, "=")
		if key == name {
			value = candidate
		}
	}
	return value
}
