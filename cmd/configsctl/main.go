// Command configsctl runs OpenTofu and manages encrypted Vault secrets.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"goodkind.io/configsctl/internal/dockerbridge"
	"goodkind.io/configsctl/internal/hclsources"
	"goodkind.io/configsctl/internal/lint"
	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofurun"
	"goodkind.io/configsctl/internal/vault"
	"goodkind.io/configsctl/internal/version"
)

// defaultVaultFile is the vault path relative to the repository root, which is
// the working directory the binary runs from.
const defaultVaultFile = vault.File

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 0 || args[0] != "lint-hcl-sources":
		err = run(args)
	case len(args) != 2:
		err = errors.New("usage: configsctl lint-hcl-sources <request.json>")
	default:
		request, readErr := os.ReadFile(args[1])
		if readErr != nil {
			err = fmt.Errorf("read HCL source request: %w", readErr)
		} else {
			err = hclsources.CheckRequest(request)
		}
	}
	if err != nil {
		slog.Error("configsctl failed", "err", err)
		var exitErr *exitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		os.Exit(1)
	}
}

// cmdEnv carries process-wide state a subcommand may need. Deploy uses the
// secret patterns to build a redactor over its own log file, which sits outside
// the stdout and stderr redactors installed for the process.
type cmdEnv struct {
	secrets       []redact.Pattern
	dockerBridges map[string]dockerbridge.Bridge
}

// handlers dispatches a subcommand name to its implementation.
var handlers = map[string]func(cmdEnv, []string) error{
	"lint":           runLint,
	"keys":           runKeys,
	"secret":         runSecret,
	"set-secrets":    runSetSecrets,
	"rename-secret":  runRenameSecret,
	"delete-secrets": runDeleteSecrets,
	"tofu":           runTofu,
	"migrate-state":  runStateMigration,
	"version":        runVersion,
	"gate":           runGate,
	"gate-finish":    runGateFinish,
	"lock":           runLock,
}

// runVersion prints the link-time build identity and the hash of the running
// binary as one line.
func runVersion(_ cmdEnv, _ []string) error {
	fmt.Println(version.BuildVersionString())
	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: configsctl <command>")
	}
	command := args[0]
	if command == "host-lock" {
		return runHostLockServer(args[1:])
	}
	if command == "ci" {
		return runCI(args[1:])
	}
	if command == "providers" {
		return runProviders(args[1:])
	}
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
			fmt.Fprintf(os.Stderr, "configsctl: refusing to run: vault key %q has a value shorter than %d characters; rotate it via 'configsctl set-secrets'\n", short.key, redact.MinLen)
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

	return handler(cmdEnv{secrets: patterns}, args[1:])
}

func runLint(_ cmdEnv, paths []string) error {
	if len(paths) == 0 {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		loaded, err := loadNativeSettings(settingsFile)
		if err != nil {
			return err
		}
		return runNativeLint(ctx, loaded.Tofu)
	}
	findings, divergences, err := lint.Run(paths)
	if err != nil {
		slog.Error("lint failed", "err", err)
		return fmt.Errorf("lint failed: %w", err)
	}
	for _, finding := range findings {
		fmt.Println(finding)
	}
	for _, divergence := range divergences {
		fmt.Fprintln(os.Stderr, divergence)
	}
	if len(findings) > 0 {
		return fmt.Errorf("%d input-default violation(s)", len(findings))
	}
	return nil
}

func runKeys(_ cmdEnv, args []string) error {
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

func runSecret(_ cmdEnv, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: configsctl secret <key>")
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

func runSetSecrets(_ cmdEnv, _ []string) error {
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

const tofuUsage = "usage: configsctl tofu [<workspace>] <tofu args...>"

func runTofu(env cmdEnv, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) == 0 {
		return errors.New(tofuUsage)
	}
	ctx, cleanupSource, err := prepareInstalledTofuSource(ctx)
	if err != nil {
		return err
	}
	defer cleanupSource()
	return runTofuWithSource(ctx, env, args)
}

func runTofuWithSource(ctx context.Context, env cmdEnv, args []string) error {
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	loaded, workspaceDir, tofuArgs, err := loadTofuWorkspace(ctx, args)
	if err != nil {
		return err
	}
	safeArgs, childEnv, err := prepareTofuInvocation(loaded.Tofu, workspaceDir, passwordFile, tofuArgs)
	if err != nil {
		return err
	}
	env, safeArgs, restoreOperator, err := prepareTofuRedaction(cmdEnv{secrets: env.secrets, dockerBridges: loaded.Tofu.dockerBridges}, loaded.Tofu, workspaceDir, safeArgs, childEnv)
	if err != nil {
		return err
	}
	defer restoreOperator()
	credential, configured := loaded.Tofu.Credentials[tofuWorkspaceKey(workspaceDir)]
	usesCredential := configured && credentialCommand(safeArgs)
	if err := requireExplicitLockPath(loaded.Tofu, workspaceDir, safeArgs); err != nil {
		return err
	}
	if err := requireMergedTofuSource(ctx, workspaceDir, safeArgs, usesCredential); err != nil {
		return err
	}
	if handled, helpErr := runEndpointHelp(ctx, env, loaded.Tofu, workspaceDir, safeArgs, childEnv); handled {
		return helpErr
	}
	releaseMigration, err := leaseTofuMigration(safeArgs, usesCredential)
	if err != nil {
		return err
	}
	defer releaseMigration()
	slog.Info("tofu run", "dir", workspaceDir, "args", strings.Join(safeArgs, " "))
	requiredHosts, err := requiredCredentialHosts(credential, usesCredential, sourceExecutionPath(ctx, workspaceDir), childEnv)
	if err != nil {
		return err
	}
	if usesCredential {
		if err := requireInstalledModuleCache(ctx, workspaceDir, environmentValue(childEnv, "TF_DATA_DIR"), safeArgs); err != nil {
			return err
		}
	}
	if tofuMutates(safeArgs) {
		request := tofuLockRequest{
			workspaceDir: workspaceDir, env: childEnv, args: safeArgs,
			lockTargets: loaded.Tofu.LockTargets, requiredHosts: requiredHosts, independentWorkspaces: true,
			guestModule: loaded.Tofu.lockGuestModule(workspaceDir),
		}
		return runIndependentMutation(ctx, env, loaded.Tofu, request, passwordFile)
	}
	commandArgs := safeArgs
	release := func() {}
	if usesCredential {
		ctx, release, err = lockCredentialHost(ctx, credential.LockHost, loaded.Tofu.LockTargets[tofuWorkspaceKey(workspaceDir)])
	}
	if err != nil {
		return err
	}
	defer release()
	if err := requireCurrentInstalledSource(ctx, workspaceDir); err != nil {
		return err
	}
	if usesCredential {
		rotatorPassword, err := vault.Secret(credential.Dell.RotatorPasswordKey, defaultVaultFile, passwordFile)
		if err != nil {
			slog.Error("tofu.credential.vault_failed", "err", err)
			return fmt.Errorf("read rotator credential: %w", err)
		}
		var patterns []redact.Pattern
		childEnv, patterns, err = credential.prepare(ctx, workspaceDir, rotatorPassword, childEnv)
		if err != nil {
			return err
		}
		env.secrets = append(env.secrets, patterns...)
		restore, err := installRedaction(patterns)
		if err != nil {
			return err
		}
		defer restore()
	}
	return executeTofu(ctx, env, workspaceDir, safeArgs, commandArgs, childEnv)
}

func executeTofu(ctx context.Context, env cmdEnv, workspaceDir string, safeArgs, commandArgs, childEnv []string) error {
	if err := requireInstalledModuleCache(ctx, workspaceDir, environmentValue(childEnv, "TF_DATA_DIR"), commandArgs); err != nil {
		return err
	}
	planPath := tofuPlanOutput(commandArgs)
	var source tofuPlanSource
	if planPath != "" {
		var err error
		source, err = inspectTofuPlanSource(ctx, workspaceDir)
		if err != nil {
			return err
		}
	}
	request := tofurun.Request{
		Root: sourceExecutionPath(ctx, workspaceDir), Args: commandArgs, Environment: childEnv, DisplayArgs: safeArgs, Patterns: env.secrets,
	}
	bridges := env.dockerBridges
	if bridged, _ := dockerBridgeAccess(commandArgs); !bridged || tofuMutates(commandArgs) {
		bridges = nil
	}
	return runInDockerBridgeDirectory(ctx, bridges, childEnv, func(bridgeContext context.Context, bridgeArguments func([]string) []string) error {
		request.Args = bridgeArguments(commandArgs)
		return finishTofu(bridgeContext, request, workspaceDir, planPath, safeArgs, source)
	})
}

func finishTofu(ctx context.Context, request tofurun.Request, workspaceDir, planPath string, safeArgs []string, source tofuPlanSource) error {
	commandArgs := request.Args
	result, err := tofurun.Run(ctx, request)

	if errors.Is(err, tofurun.ErrIncomplete) {
		return tofuFailure(ctx, err, result.LogPath)
	}
	if err == nil && planPath != "" && (result.ExitCode == 0 || result.ExitCode == tofuPendingExitCode) {
		if err := recordTofuPlanSource(ctx, workspaceDir, workspacePath(workspaceDir, planPath), source); err != nil {
			return err
		}
	}
	pendingPlan := tofuargs.Subcommand(commandArgs) == tofuargs.Plan &&
		tofuargs.Subcommand(safeArgs) != tofuargs.Plan && result.ExitCode == tofuPendingExitCode
	if result.ExitCode != 0 && !pendingPlan {
		return tofuFailure(ctx, &exitCodeError{code: result.ExitCode}, result.LogPath)
	}
	if err != nil {
		slog.Error("tofu.run_failed", "workspace", workspaceDir, "err", err)
		return fmt.Errorf("run OpenTofu in %s: %w", workspaceDir, err)
	}
	if pendingPlan {
		return &exitCodeError{code: tofuPendingExitCode}
	}
	return nil
}

const tofuCrashExitCode = 11

func tofuFailure(ctx context.Context, runErr error, logPath string) error {
	cause := context.Cause(ctx)
	var exitErr *exitCodeError
	exited := errors.As(runErr, &exitErr)
	if cause == nil && exited && exitErr.code == tofuCrashExitCode {
		return tofuCrashError(runErr, logPath)
	}
	slog.Error("tofu command failed", "err", runErr)
	if logPath != "" {
		fmt.Fprintf(os.Stderr, "The tofu output is in %s\n", logPath)
	}
	if cause != nil {
		return fmt.Errorf("tofu stopped: %w", cause)
	}
	if exited {
		return exitErr
	}
	return errors.New("tofu failed")
}

func tofuCrashError(runErr error, logPath string) error {
	crash := &exitCodeError{code: tofuCrashExitCode}
	if logPath == "" {
		slog.Error("tofu.crashed", "exit_code", tofuCrashExitCode, "err", runErr)
		return fmt.Errorf("OpenTofu crashed: %w", crash)
	}
	slog.Error("tofu.crashed", "exit_code", tofuCrashExitCode, "log", logPath, "err", runErr)
	return fmt.Errorf("OpenTofu crashed; the tofu output is in %s: %w", logPath, crash)
}

// exitCodeError carries a child process exit status to main, which exits with
// the same code. Tofu distinguishes status 2 (pending changes under
// -detailed-exitcode) from status 1 (error), and that distinction survives.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("tofu exited with status %d", e.code)
}

// tofuArgPattern accepts printable characters only. Arguments go to tofu as
// an argv vector, never through a shell, so this exists to reject control
// characters and to give the taint analysis a validation boundary.
var tofuArgPattern = regexp.MustCompile(`^[\x20-\x7E]+$`)

// sanitizeTofuArgs validates each argument and returns the validated copies.
func sanitizeTofuArgs(args []string) ([]string, error) {
	safe := make([]string, 0, len(args))
	for _, arg := range args {
		match := tofuArgPattern.FindString(arg)
		if match != arg {
			return nil, fmt.Errorf("tofu argument %q contains non-printable characters", arg)
		}
		safe = append(safe, match)
	}
	return safe, nil
}

func runRenameSecret(_ cmdEnv, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: configsctl rename-secret <old-key> <new-key>")
	}
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	if err := vault.RenameSecret(args[0], args[1], defaultVaultFile, passwordFile); err != nil {
		slog.Error("vault secret rename failed", "old", args[0], "new", args[1], "err", err)
		return fmt.Errorf("rename vault secret %q to %q: %w", args[0], args[1], err)
	}
	fmt.Printf("renamed: %s -> %s\n", args[0], args[1])
	return nil
}

func runDeleteSecrets(_ cmdEnv, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: configsctl delete-secrets <key> [<key>]")
	}
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	if err := vault.DeleteSecrets(args, defaultVaultFile, passwordFile); err != nil {
		slog.Error("vault secret delete failed", "keys", strings.Join(args, " "), "err", err)
		return fmt.Errorf("delete vault secrets: %w", err)
	}
	fmt.Printf("deleted: %s\n", strings.Join(args, ", "))
	return nil
}

func vaultPassPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve home directory failed")
	}
	return filepath.Join(home, ".config", "ansible", "vault.pass"), nil
}

// shortSecretError marks a fail-closed validation failure. It names the key,
// never the value.
type shortSecretError struct{ key string }

func (e *shortSecretError) Error() string {
	return fmt.Sprintf("vault key %q value shorter than %d chars", e.key, redact.MinLen)
}

// vaultInputs returns the password-file path and whether it and the vault are
// both present. When the home dir cannot be resolved or either file is absent,
// available is false and the caller runs without redaction patterns.
func vaultInputs() (passwordFile string, available bool) {
	passwordFile, err := vaultPassPath()
	if err != nil {
		return "", false
	}
	if _, statErr := os.Stat(defaultVaultFile); statErr != nil {
		return "", false
	}
	if _, statErr := os.Stat(passwordFile); statErr != nil {
		return "", false
	}
	return passwordFile, true
}

// loadSecretPatterns reads the vault and builds redaction patterns. An absent
// vault or password file yields no patterns and no error. A too-short secret
// returns *shortSecretError.
func loadSecretPatterns() ([]redact.Pattern, error) {
	passwordFile, available := vaultInputs()
	if !available {
		return nil, nil
	}
	patterns, err := vault.Patterns(".", passwordFile)
	if err != nil {
		slog.Error("vault values load failed", "err", err)
		return nil, fmt.Errorf("load vault values: %w", err)
	}
	if badKey, ok := redact.Validate(patterns); !ok {
		return nil, &shortSecretError{key: badKey}
	}
	return patterns, nil
}

// lockedWriter serializes writes to dst using mu.
type lockedWriter struct {
	mu  *sync.Mutex
	dst io.Writer
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

// installRedaction filters [os.Stdout] and [os.Stderr] and returns a function
// that restores their original descriptors after both redactors finish.
func installRedaction(patterns []redact.Pattern) (func(), error) {
	realStdout, realStderr := os.Stdout, os.Stderr
	realLogOutput := log.Writer()
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
	log.SetOutput(errW)

	restore := func() {
		os.Stdout = realStdout
		os.Stderr = realStderr
		log.SetOutput(realLogOutput)
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
	}
	return restore, nil
}
