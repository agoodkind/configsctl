// Command configs is the control tool for the configs repository. It runs
// Ansible deploys, manages vault secrets, and lints Ansible input variables.
package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/configs/internal/ansible"
	"goodkind.io/configs/internal/baseline"
	"goodkind.io/configs/internal/lint"
	"goodkind.io/configs/internal/vault"
)

// defaultVaultFile is the vault path relative to the repository root, which is
// the working directory the binary runs from.
const defaultVaultFile = "ansible/inventory/group_vars/all/vault.yml"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("configs failed", "err", err)
		os.Exit(1)
	}
}

// handlers dispatches a subcommand name to its implementation.
var handlers = map[string]func([]string) error{
	"lint":           runLint,
	"baseline":       runBaseline,
	"keys":           runKeys,
	"secret":         runSecret,
	"set-secrets":    runSetSecrets,
	"deploy":         runDeploy,
	"syntax-check":   runSyntaxCheck,
	"inventory-dump": runInventoryDump,
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: configs <command>")
	}
	handler, ok := handlers[args[0]]
	if !ok {
		return fmt.Errorf("unknown command: %q", args[0])
	}
	return handler(args[1:])
}

func runLint(paths []string) error {
	if len(paths) == 0 {
		paths = lint.Discover(".")
	}
	result, err := lint.Gate(paths)
	if err != nil {
		slog.Error("lint gate failed", "err", err)
		return fmt.Errorf("lint gate failed: %w", err)
	}
	for _, line := range result.NewFindings {
		fmt.Println(line)
	}
	for _, line := range result.Divergences {
		fmt.Fprintln(os.Stderr, line)
	}
	if result.BypassToken != "" {
		fmt.Printf("*** lint non-blocking via BYPASS_LINT=%s\n", result.BypassToken)
	}
	if result.Blocked {
		return fmt.Errorf("%d input-default violation(s)", len(result.NewFindings))
	}
	return nil
}

func runBaseline(args []string) error {
	value := modeFlag(args)
	mode, err := baseline.ParseMode(value)
	if err != nil {
		return fmt.Errorf("invalid baseline mode %q", value)
	}
	if lint.WriteBaseline(lint.Discover("."), mode) {
		fmt.Println("baseline updated")
	}
	return nil
}

func runKeys(_ []string) error {
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	keys, err := vault.Keys(defaultVaultFile, passwordFile)
	if err != nil {
		return errors.New("list vault keys failed")
	}
	for _, key := range keys {
		fmt.Println(key)
	}
	return nil
}

func runSecret(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: configs secret <key>")
	}
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	value, err := vault.Secret(args[0], defaultVaultFile, passwordFile)
	if err != nil {
		return fmt.Errorf("read vault secret %q", args[0])
	}
	fmt.Print(value)
	return nil
}

func runSetSecrets(_ []string) error {
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return errors.New("read stdin failed")
	}
	added, updated, err := vault.SetSecrets(string(stdin), defaultVaultFile, passwordFile)
	if err != nil {
		return errors.New("merge vault secrets failed")
	}
	if len(added) > 0 {
		fmt.Printf("added: %s\n", strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		fmt.Printf("updated: %s\n", strings.Join(updated, ", "))
	}
	return nil
}

func runDeploy(args []string) error {
	opts, err := parseDeploy(args)
	if err != nil {
		return err
	}
	if err := ansible.Deploy(opts); err != nil {
		return errors.New("deploy failed")
	}
	return nil
}

func runSyntaxCheck(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: configs syntax-check <playbook>")
	}
	if err := ansible.SyntaxCheck(args[0]); err != nil {
		return errors.New("syntax-check failed")
	}
	return nil
}

func runInventoryDump(_ []string) error {
	if err := ansible.InventoryDump(); err != nil {
		return errors.New("inventory-dump failed")
	}
	return nil
}

func parseDeploy(args []string) (ansible.DeployOptions, error) {
	var opts ansible.DeployOptions
	index := 0
	for index < len(args) {
		consumed, err := applyDeployArg(&opts, args, index)
		if err != nil {
			return opts, err
		}
		index += consumed
	}
	if opts.Playbook == "" {
		return opts, errors.New("usage: configs deploy <playbook> [flags]")
	}
	return opts, nil
}

// applyDeployArg applies one deploy argument and returns how many tokens it
// consumed.
func applyDeployArg(opts *ansible.DeployOptions, args []string, index int) (int, error) {
	arg := args[index]
	switch {
	case arg == "--check":
		opts.Check = true
	case arg == "--diff":
		opts.Diff = true
	case arg == "--full-lint":
		opts.FullLint = true
	case arg == "--limit" && index+1 < len(args):
		opts.Limit = args[index+1]
		return 2, nil
	case strings.HasPrefix(arg, "--limit="):
		opts.Limit = strings.TrimPrefix(arg, "--limit=")
	case arg == "--extra-var" && index+1 < len(args):
		opts.ExtraVars = append(opts.ExtraVars, args[index+1])
		return 2, nil
	case strings.HasPrefix(arg, "--extra-var="):
		opts.ExtraVars = append(opts.ExtraVars, strings.TrimPrefix(arg, "--extra-var="))
	case !strings.HasPrefix(arg, "-"):
		opts.Playbook = arg
	default:
		return 0, fmt.Errorf("unknown deploy argument: %q", arg)
	}
	return 1, nil
}

func vaultPassPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve home directory failed")
	}
	return filepath.Join(home, ".config", "ansible", "vault.pass"), nil
}

// modeFlag reads the value of a --mode flag, accepting --mode=value and
// --mode value forms. It returns empty when the flag is absent.
func modeFlag(args []string) string {
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--mode="); ok {
			return value
		}
		if arg == "--mode" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
