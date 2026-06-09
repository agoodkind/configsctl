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
	"sync"

	"goodkind.io/configs/internal/ansible"
	"goodkind.io/configs/internal/baseline"
	"goodkind.io/configs/internal/lint"
	"goodkind.io/configs/internal/redact"
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
	command := args[0]
	handler, ok := handlers[command]
	if !ok {
		return fmt.Errorf("unknown command: %q", command)
	}

	patterns, loadErr := loadSecretPatterns()
	if loadErr != nil {
		var short *shortSecretError
		isShort := errors.As(loadErr, &short)
		switch {
		case isShort && command == "set-secrets":
			patterns = nil // set-secrets is exempt; it prints only key names.
		case isShort:
			fmt.Fprintf(os.Stderr, "configs: refusing to run: vault key %q has a value shorter than %d characters; rotate it via 'configs set-secrets'\n", short.key, redact.MinLen)
			return short
		default:
			return loadErr
		}
	}

	restore, err := installRedaction(patterns)
	if err != nil {
		return err
	}
	defer restore()

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

func runKeys(args []string) error {
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "--fingerprint" {
		details, err := vault.KeyDetails(defaultVaultFile, passwordFile)
		if err != nil {
			return errors.New("list vault key details failed")
		}
		printKeyDetails(details)
		return nil
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

// printKeyDetails writes each key's name, value byte length, and keyed
// fingerprint in aligned columns. It prints no secret values.
func printKeyDetails(details []vault.KeyDetail) {
	nameWidth := len("NAME")
	for _, detail := range details {
		nameWidth = max(nameWidth, len(detail.Name))
	}
	fmt.Printf("%-*s  %6s  %s\n", nameWidth, "NAME", "LENGTH", "FP")
	for _, detail := range details {
		fmt.Printf("%-*s  %6d  %s\n", nameWidth, detail.Name, detail.Length, detail.Fingerprint)
	}
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
	dir, path, err := writeSecretFile(value)
	if err != nil {
		return err
	}
	fmt.Printf("Secret %q written to: %s\n", args[0], path)
	fmt.Println("WARNING: this file holds the secret in plaintext on disk.")
	fmt.Println("Do not cat, paste, log, or commit it. Delete it after use:")
	fmt.Printf("  rm -rf %s\n", dir)
	return nil
}

// writeSecretFile creates a 0700 temp dir and writes value to a file inside it
// via [os.CreateTemp], which owns the path and creates the file at 0600. Writing
// through the returned handle keeps the filesystem path entirely tool-owned. It
// returns the dir (for the operator's cleanup line) and the file path.
func writeSecretFile(value string) (dir, path string, err error) {
	dir, err = os.MkdirTemp("", "configs-secret-*")
	if err != nil {
		slog.Error("create secret temp dir failed", "err", err)
		return "", "", fmt.Errorf("create secret temp dir: %w", err)
	}
	if err = os.Chmod(dir, 0o700); err != nil {
		slog.Error("chmod secret temp dir failed", "err", err)
		return "", "", fmt.Errorf("chmod secret temp dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "secret-*")
	if err != nil {
		slog.Error("create secret file failed", "err", err)
		return "", "", fmt.Errorf("create secret file: %w", err)
	}
	if _, err = f.WriteString(value); err != nil {
		_ = f.Close()
		slog.Error("write secret file failed", "err", err)
		return "", "", fmt.Errorf("write secret file: %w", err)
	}
	if err = f.Close(); err != nil {
		slog.Error("close secret file failed", "err", err)
		return "", "", fmt.Errorf("close secret file: %w", err)
	}
	return dir, f.Name(), nil
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

// shortSecretError marks a fail-closed validation failure. It names the key,
// never the value.
type shortSecretError struct{ key string }

func (e *shortSecretError) Error() string {
	return fmt.Sprintf("vault key %q value shorter than %d chars", e.key, redact.MinLen)
}

// vaultInputs returns the vault and password-file paths and whether both are
// present. When the home dir cannot be resolved or either file is absent,
// available is false and the caller runs without redaction patterns, because
// nothing decrypts anywhere in that case.
func vaultInputs() (vaultPath, passwordFile string, available bool) {
	passwordFile, err := vaultPassPath()
	if err != nil {
		return "", "", false
	}
	if _, statErr := os.Stat(defaultVaultFile); statErr != nil {
		return "", "", false
	}
	if _, statErr := os.Stat(passwordFile); statErr != nil {
		return "", "", false
	}
	return defaultVaultFile, passwordFile, true
}

// loadSecretPatterns reads the vault and builds redaction patterns. An absent
// vault or password file yields no patterns and no error, because nothing
// decrypts anywhere in that case. A too-short secret returns *shortSecretError.
func loadSecretPatterns() ([]redact.Pattern, error) {
	vaultPath, passwordFile, available := vaultInputs()
	if !available {
		return nil, nil
	}
	values, err := vault.Values(vaultPath, passwordFile)
	if err != nil {
		slog.Error("vault values load failed", "err", err)
		return nil, fmt.Errorf("load vault values: %w", err)
	}
	patterns := make([]redact.Pattern, 0, len(values))
	for name, value := range values {
		if value == "" {
			continue
		}
		patterns = append(patterns, redact.Pattern{Value: []byte(value), Label: name})
	}
	if badKey, ok := redact.Validate(patterns); !ok {
		return nil, &shortSecretError{key: badKey}
	}
	return patterns, nil
}

// lockedWriter serializes writes to a shared real descriptor under a mutex, so
// the stdout and stderr redactors never interleave a partial write.
type lockedWriter struct {
	mu  *sync.Mutex
	dst *os.File
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.dst.Write(p)
	if err != nil {
		return n, fmt.Errorf("locked write: %w", err)
	}
	return n, nil
}

// installRedaction routes [os.Stdout] and [os.Stderr] through redactors that
// share one mutex, so writes to the two real streams never interleave mid-write.
// With no patterns it is a no-op and leaves the real descriptors in place. It
// returns a restore func to call on exit. A pipe-creation failure is fatal: with
// secrets present and no way to filter, the tool must not run, so the error
// propagates and no command dispatches.
func installRedaction(patterns []redact.Pattern) (func(), error) {
	realStdout, realStderr := os.Stdout, os.Stderr
	if len(patterns) == 0 {
		return func() {}, nil
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		slog.Error("stdout pipe failed", "err", err)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		slog.Error("stderr pipe failed", "err", err)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	drain := func(src *os.File, dst *os.File) {
		defer wg.Done()
		w := redact.New(&lockedWriter{mu: &mu, dst: dst}, patterns)
		_, _ = io.Copy(w, src)
		_ = w.Close()
	}
	launch := func(src *os.File, dst *os.File) {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("redaction drain panicked", "err", fmt.Errorf("%v", r))
				}
			}()
			drain(src, dst)
		}()
	}
	wg.Add(2)
	launch(outR, realStdout)
	launch(errR, realStderr)

	os.Stdout = outW
	os.Stderr = errW

	restore := func() {
		os.Stdout = realStdout
		os.Stderr = realStderr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
	}
	return restore, nil
}
