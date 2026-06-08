// Package ansible shells out to the ansible CLIs for the operations that have no
// Go equivalent: running a playbook, syntax-checking it, and dumping the
// inventory whose custom plugin only ansible can resolve.
package ansible

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"goodkind.io/configs/internal/lint"
	"gopkg.in/yaml.v3"
)

// vaultVarPrefix is the name prefix every secret variable carries, because all
// secrets live in group_vars/all/vault.yml as vault_* keys. The inventory dump
// redacts any variable with this prefix.
const vaultVarPrefix = "vault_"

// redactedPlaceholder replaces a secret value in the redacted inventory dump.
const redactedPlaceholder = "<redacted>"

// ansibleDir is the ansible working tree, relative to the repository root that
// the binary runs from. The ansible CLIs run with this as their directory so the
// local ansible.cfg applies.
const ansibleDir = "ansible"

// DeployOptions configures a playbook run.
type DeployOptions struct {
	Playbook  string
	Limit     string
	Check     bool
	Diff      bool
	ExtraVars []string
	FullLint  bool
}

var (
	importRE = regexp.MustCompile(
		`(?:ansible\.builtin\.)?` +
			`(?:import_playbook|include_playbook|import_tasks|include_tasks)\s*:\s*` +
			`([^\s{}'"]+)`,
	)
	templateRE = regexp.MustCompile(`([^\s"'{}]*\.j2)`)
)

// Deploy lints the playbook's reachable files, then runs ansible-playbook. A
// blocking lint finding refuses the deploy.
func Deploy(opts DeployOptions) error {
	gatePaths := ScopeFiles(opts.Playbook)
	if opts.FullLint {
		gatePaths = lint.Discover(".")
	}
	result, err := lint.Gate(gatePaths)
	if err != nil {
		slog.Error("lint gate failed", "playbook", opts.Playbook, "err", err)
		return fmt.Errorf("lint gate failed: %w", err)
	}
	for _, divergence := range result.Divergences {
		fmt.Fprintln(os.Stderr, divergence)
	}
	if result.Blocked {
		for _, finding := range result.NewFindings {
			fmt.Fprintln(os.Stdout, finding)
		}
		return fmt.Errorf("deploy blocked: %d input-default violation(s)", len(result.NewFindings))
	}
	return runPlaybook(opts)
}

// SyntaxCheck validates a playbook's structure without connecting to a host.
func SyntaxCheck(playbook string) error {
	args := []string{
		"--syntax-check", "--vault-password-file", vaultPassPath(), playbookArg(playbook),
	}
	return runStreaming("ansible-playbook", args)
}

// InventoryDump prints the resolved inventory as YAML with every vault-sourced
// secret value redacted, so the structural dump never exposes decrypted
// credentials.
func InventoryDump() error {
	cmd := exec.CommandContext(context.Background(), "ansible-inventory", "--list", "--yaml")
	cmd.Dir = ansibleDir
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		slog.Error("ansible-inventory failed", "err", err)
		return fmt.Errorf("ansible-inventory: %w", err)
	}
	redacted, err := redactInventorySecrets(raw)
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(redacted); err != nil {
		return fmt.Errorf("write inventory: %w", err)
	}
	return nil
}

// redactInventorySecrets parses the ansible-inventory YAML and replaces the
// value of every vault-sourced variable with redactedPlaceholder.
func redactInventorySecrets(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		slog.Error("parse inventory yaml failed", "err", err)
		return nil, fmt.Errorf("parse inventory yaml: %w", err)
	}
	redactVaultNodes(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		slog.Error("marshal redacted inventory failed", "err", err)
		return nil, fmt.Errorf("marshal redacted inventory: %w", err)
	}
	return out, nil
}

// redactVaultNodes walks the YAML tree and, for every mapping entry whose key
// starts with vaultVarPrefix, collapses the value node to a redacted scalar.
// Collapsing the node handles multi-line and structured secret values, not just
// single-line scalars.
func redactVaultNodes(node *yaml.Node) {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			if strings.HasPrefix(key.Value, vaultVarPrefix) {
				value.Kind = yaml.ScalarNode
				value.Tag = "!!str"
				value.Value = redactedPlaceholder
				value.Content = nil
				value.Alias = nil
				continue
			}
			redactVaultNodes(value)
		}
		return
	}
	for _, child := range node.Content {
		redactVaultNodes(child)
	}
}

func runPlaybook(opts DeployOptions) error {
	args := []string{"--vault-password-file", vaultPassPath(), playbookArg(opts.Playbook)}
	if opts.Limit != "" {
		args = append(args, "--limit", opts.Limit)
	}
	if opts.Check {
		args = append(args, "--check")
	}
	if opts.Diff {
		args = append(args, "--diff")
	}
	for _, extra := range opts.ExtraVars {
		args = append(args, "--extra-vars", extra)
	}
	return runStreaming("ansible-playbook", args)
}

func runStreaming(name string, args []string) error {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = ansibleDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Error("ansible command failed", "command", name, "err", err)
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// ScopeFiles returns the files a deploy reads: the playbook, its transitive
// imports and includes, and the .j2 templates they render. Paths are relative to
// the repository root so the linter can read them.
func ScopeFiles(playbook string) []string {
	worklist := []string{scopePlaybookPath(playbook)}
	seen := map[string]struct{}{}
	found := map[string]struct{}{}
	for len(worklist) > 0 {
		current := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		if _, done := seen[current]; done {
			continue
		}
		seen[current] = struct{}{}
		data, err := os.ReadFile(current)
		if err != nil {
			continue
		}
		if hasYAMLSuffix(current) {
			found[current] = struct{}{}
		}
		worklist = append(worklist, importsIn(current, string(data))...)
		addTemplates(string(data), found)
	}
	return sortedKeys(found)
}

func importsIn(current, text string) []string {
	var targets []string
	for _, match := range importRE.FindAllStringSubmatch(text, -1) {
		targets = append(targets, filepath.Join(filepath.Dir(current), match[1]))
	}
	return targets
}

func addTemplates(text string, found map[string]struct{}) {
	for _, match := range templateRE.FindAllStringSubmatch(text, -1) {
		token := strings.TrimLeft(match[1], "/")
		if token == "" {
			continue
		}
		template := filepath.Clean(token)
		if info, err := os.Stat(template); err == nil && !info.IsDir() {
			found[template] = struct{}{}
		}
	}
}

func scopePlaybookPath(playbook string) string {
	if hasYAMLSuffix(playbook) {
		return playbook
	}
	return filepath.Join(ansibleDir, "playbooks", playbook+".yml")
}

func playbookArg(playbook string) string {
	if hasYAMLSuffix(playbook) {
		return playbook
	}
	return filepath.Join("playbooks", playbook+".yml")
}

func hasYAMLSuffix(path string) bool {
	return strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")
}

func vaultPassPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".vault.pass"
	}
	return filepath.Join(home, ".config", "ansible", "vault.pass")
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
