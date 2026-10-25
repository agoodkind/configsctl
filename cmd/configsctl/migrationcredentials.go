package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/dellcredential"
	"goodkind.io/configsctl/internal/hostlock"
	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/statemigration"
	"goodkind.io/configsctl/internal/vault"
)

type migrationCredentialScope string

const (
	migrationCredentialCutover  migrationCredentialScope = "cutover"
	migrationCredentialOriginal migrationCredentialScope = "original"
	migrationCredentialRestored migrationCredentialScope = "restored"
)

type migrationCredentialBinding struct {
	root        string
	selector    string
	settings    tofuCredentialSettings
	workspace   statemigration.Workspace
	scope       migrationCredentialScope
	certificate [sha256.Size]byte
}

type migrationDeployment struct {
	settings   tofuCredentialSettings
	credential dellcredential.Credential
	rotator    string
}

type migrationCredentials struct {
	deployments []migrationDeployment
	hosts       []hostlock.Host
}

func (credentials migrationCredentials) patterns() []redact.Pattern {
	var patterns []redact.Pattern
	for _, deployment := range credentials.deployments {
		patterns = append(patterns,
			redact.Pattern{Label: deployment.settings.PasswordVariable, Value: []byte(deployment.credential.Password)},
			redact.Pattern{Label: deployment.settings.Dell.RotatorPasswordKey, Value: []byte(deployment.rotator)})
	}
	return patterns
}

func prepareMigrationCredentials(ctx context.Context, engine *statemigration.Engine, passwordFile string) (migrationCredentials, error) {
	slog.InfoContext(ctx, "migration.credential.configuration_check_started")
	bindings, err := migrationCredentialBindings(engine)
	if err != nil {
		return migrationCredentials{}, err
	}
	if err := matchMigrationCredentialBindings(engine.Manifest, bindings); err != nil {
		return migrationCredentials{}, err
	}
	result := migrationCredentials{deployments: nil, hosts: nil}
	accounts := make(map[string]int)
	for _, binding := range bindings {
		key := binding.settings.Dell.Endpoint + "\x00" + binding.settings.Dell.AccountID
		index, exists := accounts[key]
		rotator, err := vault.Secret(binding.settings.Dell.RotatorPasswordKey, filepath.Join(binding.root, defaultVaultFile), passwordFile)
		if err != nil || rotator == "" {
			return migrationCredentials{}, fmt.Errorf("read the required nonempty vault key %s for credential host %s", binding.settings.Dell.RotatorPasswordKey, binding.settings.LockHost)
		}
		if !exists {
			credential, err := binding.settings.Dell.GenerateCredential()
			if err != nil {
				slog.ErrorContext(ctx, "migration.credential.generation_failed", "host", binding.settings.LockHost, "err", fmt.Sprintf("%T", err))
				return migrationCredentials{}, fmt.Errorf("generate the protected migration credential: %w", err)
			}
			index = len(result.deployments)
			accounts[key] = index
			result.deployments = append(result.deployments, migrationDeployment{binding.settings, credential, rotator})
		} else if result.deployments[index].rotator != rotator {
			return migrationCredentials{}, errors.New("the reviewed configurations disagree on the account rotator credential")
		}
		workspace := binding.workspace
		workspace.Environment = migrationCredentialEnvironment(workspace.Environment, binding.settings, result.deployments[index].credential)
		workspaces, err := migrationBindingWorkspaces(engine, binding.scope)
		if err != nil {
			return migrationCredentials{}, err
		}
		workspaces[binding.selector] = workspace
	}
	if err := stripMigrationRotators(engine, bindings); err != nil {
		return migrationCredentials{}, err
	}
	for _, binding := range bindings {
		workspaces, err := migrationBindingWorkspaces(engine, binding.scope)
		if err != nil {
			return migrationCredentials{}, err
		}
		if err := requireMigrationInputs(ctx, workspaces[binding.selector]); err != nil {
			return migrationCredentials{}, err
		}
	}
	result.hosts, err = migrationCredentialHosts(ctx, bindings)
	if err != nil {
		return migrationCredentials{}, err
	}
	return result, nil
}

func migrationCredentialBindings(engine *statemigration.Engine) ([]migrationCredentialBinding, error) {
	var bindings []migrationCredentialBinding
	roots := map[migrationCredentialScope]string{migrationCredentialCutover: engine.Manifest.ConfigurationDirectory, migrationCredentialOriginal: engine.Manifest.RestoreConfigurationDirectory}
	if len(engine.RestoreWorkspaces) != 0 {
		root, err := filepath.Abs(".")
		if err != nil {
			slog.Error("migration.credential.caller_resolve_failed", "err", fmt.Sprintf("%T", err))
			return nil, fmt.Errorf("resolve the original migration caller: %w", err)
		}
		roots[migrationCredentialRestored] = root
	}
	for _, scope := range []migrationCredentialScope{migrationCredentialCutover, migrationCredentialOriginal, migrationCredentialRestored} {
		root, exists := roots[scope]
		if !exists {
			continue
		}
		loaded, err := loadSettings(filepath.Join(root, settingsFile))
		if err != nil {
			return nil, err
		}
		workspaces, err := migrationBindingWorkspaces(engine, scope)
		if err != nil {
			return nil, err
		}
		for _, selector := range slices.Sorted(slices.Values(migrationWorkspaceSelectors(workspaces))) {
			workspace := workspaces[selector]
			name := tofuWorkspaceKey(filepath.Join(loaded.Tofu.WorkspacesDir, selector))
			configured, exists := loaded.Tofu.Credentials[name]
			if !exists {
				continue
			}
			binding, err := newMigrationCredentialBinding(root, selector, scope, configured, workspace)
			if err != nil {
				return nil, err
			}
			bindings = append(bindings, binding)
		}
	}
	return bindings, nil
}

func migrationWorkspaceSelectors(workspaces map[string]statemigration.Workspace) []string {
	selectors := make([]string, 0, len(workspaces))
	for selector := range workspaces {
		selectors = append(selectors, selector)
	}
	return selectors
}

func migrationBindingWorkspaces(engine *statemigration.Engine, scope migrationCredentialScope) (map[string]statemigration.Workspace, error) {
	switch scope {
	case migrationCredentialOriginal:
		return engine.OriginalWorkspaces, nil
	case migrationCredentialRestored:
		return engine.RestoreWorkspaces, nil
	case migrationCredentialCutover:
		return engine.Workspaces, nil
	default:
		return nil, errors.New("the migration credential scope is unsupported")
	}
}

func newMigrationCredentialBinding(root, selector string, scope migrationCredentialScope, configured tofuCredentialSettings, workspace statemigration.Workspace) (migrationCredentialBinding, error) {
	binding := migrationCredentialBinding{root: root, selector: selector, settings: configured, workspace: workspace, scope: scope, certificate: [sha256.Size]byte{}}
	if configured.UsernameVariable == configured.Dell.RotatorPasswordKey || configured.PasswordVariable == configured.Dell.RotatorPasswordKey {
		return binding, errors.New("the provider credential variables must differ from the rotator vault key")
	}
	adapter := *configured.Dell
	adapter.ReadPaths = slices.Clone(adapter.ReadPaths)
	if adapter.CAFile != "" {
		if !filepath.IsAbs(adapter.CAFile) {
			adapter.CAFile = filepath.Join(root, adapter.CAFile)
		}
		certificate, err := os.ReadFile(adapter.CAFile)
		if err != nil {
			return binding, errors.New("read the reviewed Dell CA certificate before credential activation")
		}
		if !x509.NewCertPool().AppendCertsFromPEM(certificate) {
			return binding, errors.New("the reviewed Dell CA certificate must contain PEM certificates")
		}
		binding.certificate = sha256.Sum256(certificate)
	}
	binding.settings.Dell = &adapter
	if err := binding.settings.validateWorkspace(workspace.Directory, workspace.Environment); err != nil {
		return binding, err
	}
	for _, assignment := range workspace.Environment {
		key, value, _ := strings.Cut(assignment, "=")
		if value != "" && (strings.HasPrefix(key, "TF_LOG") || strings.HasPrefix(key, "TOFU_LOG") || key == "TF_TEMP_LOG_PATH" || key == "TOFU_CPU_PROFILE") {
			if (key == "TF_LOG" || key == "TOFU_LOG") && strings.EqualFold(value, "OFF") {
				continue
			}
			return binding, errors.New("migration provider credentials require native logging and profiling overrides to be disabled")
		}
	}
	return binding, nil
}

func sameMigrationCredentialBinding(left, right migrationCredentialBinding) bool {
	a, b := left.settings, right.settings
	return a.LockHost == b.LockHost && a.UsernameVariable == b.UsernameVariable && a.PasswordVariable == b.PasswordVariable &&
		a.Dell.Endpoint == b.Dell.Endpoint && a.Dell.AccountID == b.Dell.AccountID && a.Dell.Username == b.Dell.Username &&
		a.Dell.RotatorUser == b.Dell.RotatorUser && a.Dell.RotatorPasswordKey == b.Dell.RotatorPasswordKey &&
		a.Dell.ServerName == b.Dell.ServerName && left.certificate == right.certificate && slices.Equal(a.Dell.ReadPaths, b.Dell.ReadPaths)
}

func matchMigrationCredentialBindings(manifest statemigration.Manifest, bindings []migrationCredentialBinding) error {
	byScope := make(map[string]migrationCredentialBinding)
	accounts := make(map[string]migrationCredentialBinding)
	for _, binding := range bindings {
		byScope[string(binding.scope)+"\x00"+binding.selector] = binding
		key := binding.settings.Dell.Endpoint + "\x00" + binding.settings.Dell.AccountID
		if previous, exists := accounts[key]; exists && !sameMigrationCredentialBinding(previous, binding) {
			return errors.New("the reviewed configurations disagree on Dell account, host, or provider credential identities")
		}
		accounts[key] = binding
	}
	for _, move := range manifest.Moves {
		original, originalExists := byScope["original\x00"+move.Source]
		cutover, cutoverExists := byScope["cutover\x00"+move.Destination]
		if originalExists != cutoverExists || (originalExists && !sameMigrationCredentialBinding(original, cutover)) {
			return errors.New("a reviewed transfer does not preserve its exact configured provider credential adapter")
		}
	}
	return nil
}

func migrationCredentialEnvironment(environment []string, configured tofuCredentialSettings, credential dellcredential.Credential) []string {
	keys := []string{tofuVariablePrefix + configured.UsernameVariable, tofuVariablePrefix + configured.PasswordVariable, tofuVariablePrefix + configured.Dell.RotatorPasswordKey, configured.Dell.RotatorPasswordKey}
	result := make([]string, 0, len(environment)+2)
	for _, assignment := range environment {
		key, _, _ := strings.Cut(assignment, "=")
		if !slices.Contains(keys, key) {
			result = append(result, assignment)
		}
	}
	return append(result, keys[0]+"="+credential.Username, keys[1]+"="+credential.Password)
}

func migrationWithoutRotators(environment []string, settings tofuSettings) []string {
	var clean []string
	for _, assignment := range environment {
		key, _, _ := strings.Cut(assignment, "=")
		rotator := false
		for _, configured := range settings.Credentials {
			name := configured.Dell.RotatorPasswordKey
			if key == name || key == tofuVariablePrefix+name {
				rotator = true
			}
			if exported, exists := strings.CutPrefix(name, settings.EnvKeyPrefix); exists && exported != "" && key == exported {
				rotator = true
			}
		}
		if !rotator {
			clean = append(clean, assignment)
		}
	}
	return clean
}

func stripMigrationRotators(engine *statemigration.Engine, bindings []migrationCredentialBinding) error {
	for _, scope := range []migrationCredentialScope{migrationCredentialCutover, migrationCredentialOriginal, migrationCredentialRestored} {
		workspaces, err := migrationBindingWorkspaces(engine, scope)
		if err != nil {
			return err
		}
		for selector, workspace := range workspaces {
			var clean []string
			for _, assignment := range workspace.Environment {
				key, _, _ := strings.Cut(assignment, "=")
				rotator := false
				for _, binding := range bindings {
					if key == binding.settings.Dell.RotatorPasswordKey || key == tofuVariablePrefix+binding.settings.Dell.RotatorPasswordKey {
						rotator = true
					}
				}
				if !rotator {
					clean = append(clean, assignment)
				}
			}
			workspace.Environment = clean
			workspaces[selector] = workspace
		}
	}
	return nil
}

func migrationCredentialHosts(ctx context.Context, bindings []migrationCredentialBinding) ([]hostlock.Host, error) {
	var result []hostlock.Host
	inventories := make(map[string]ansible.Inventory)
	byName := make(map[string]hostlock.Host)
	byEndpoint := make(map[string]bool)
	for _, binding := range bindings {
		inventory, exists := inventories[binding.root]
		if !exists {
			var err error
			inventory, err = ansible.LoadInventory(ctx, binding.root)
			if err != nil {
				slog.ErrorContext(ctx, "migration.credential.inventory_failed", "path", binding.root, "err", fmt.Sprintf("%T", err))
				return nil, fmt.Errorf("resolve the reviewed credential lock inventory: %w", err)
			}
			inventories[binding.root] = inventory
		}
		if err := requireLockTargets(inventory, []string{binding.settings.LockHost}); err != nil {
			return nil, err
		}
		targets, err := hostlock.Targets(inventory, []string{binding.settings.LockHost})
		if err != nil {
			return nil, fmt.Errorf("select migration credential host lock targets: %w", err)
		}
		target := targets[0]
		if previous, exists := byName[target.Name]; exists {
			if previous != target {
				return nil, errors.New("the reviewed configurations disagree on the exact credential host lock target")
			}
			continue
		}
		byName[target.Name] = target
		endpoint := target.User + "\x00" + target.Address + "\x00" + target.Dir
		if byEndpoint[endpoint] {
			continue
		}
		byEndpoint[endpoint] = true
		result = append(result, target)
	}
	slices.SortFunc(result, func(a, b hostlock.Host) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}
