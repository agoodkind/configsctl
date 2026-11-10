package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/dellcredential"
	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
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
	switch tofuargs.Subcommand(args) {
	case tofuargs.Apply, tofuargs.Destroy:
		return true
	case tofuargs.Plan, tofuargs.Refresh, "import":
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

func (s tofuCredentialSettings) prepare(ctx context.Context, workspace, password string, childEnv []string) ([]string, []redact.Pattern, error) {
	var credential dellcredential.Credential
	var err error
	if source := installedSourceFrom(ctx); source != nil {
		request, requestErr := source.moduleCacheRequest(workspace, environmentValue(childEnv, "TF_DATA_DIR"), false)
		if requestErr != nil {
			return nil, nil, requestErr
		}
		credential, err = source.verified.PrepareCredential(ctx, request, *s.Dell, password)
	} else {
		credential, err = s.Dell.Prepare(ctx, password)
	}
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

func lockCredentialHost(ctx context.Context, name string, targets tofulock.Targets) (context.Context, func(), error) {
	hosts, err := targets.LockHosts(tofulock.Decision{AllHosts: false, Reason: "", Hosts: []string{name}, GuestLocks: nil})
	if err != nil {
		slog.Error("tofu.credential.lock_targets_failed", "host", name, "err", err)
		return nil, nil, fmt.Errorf("select the credential lock host: %w", err)
	}
	return takeLocks(ctx, hosts)
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
