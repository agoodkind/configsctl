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
)

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

// InventoryDump prints the resolved inventory as YAML. Secret values in the
// output are redacted by the global redaction layer installed in main, so this
// streams ansible-inventory directly.
func InventoryDump() error {
	cmd := exec.CommandContext(context.Background(), "ansible-inventory", "--list", "--yaml")
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Error("ansible-inventory failed", "err", err)
		return fmt.Errorf("ansible-inventory: %w", err)
	}
	return nil
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
