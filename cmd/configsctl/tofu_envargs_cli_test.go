package main_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runTofuEnvironment(t *testing.T, tree configsTree, input string, environment []string, args ...string) commandResult {
	t.Helper()
	temporary := filepath.Join(tree.root, "tmp")
	if err := os.MkdirAll(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), configsctlBinary, args...)
	command.Dir = tree.root
	command.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + tree.home, "TMPDIR=" + temporary,
		"XDG_CONFIG_HOME=" + filepath.Join(tree.home, ".config"),
	}, environment...)
	command.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatal(err)
		}
		result.exitCode = exited.ExitCode()
	}
	return result
}

func TestTofuEnvironmentApprovalDoesNotAffectEmulatedPlan(t *testing.T) {
	tree := independentUnlockedTree(t)
	args := tofuArgs("suburban/tack", "apply")
	result := runTofuEnvironment(t, tree, "", []string{
		"TF_CLI_ARGS_apply=-auto-approve -input=false",
		"TF_CLI_ARGS_plan=-invalid-emulated-plan-option",
	}, args...)
	requireSuccess(t, result, args...)
	requireFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
	if strings.Contains(result.stdout, "Enter a value:") {
		t.Fatal("environment approval still requested confirmation")
	}
}

func TestTofuEnvironmentPreservesNativeApprovalPrecedence(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(stringBool(explicit), func(t *testing.T) {
			tree := independentUnlockedTree(t)
			args := tofuArgs("suburban/tack", "apply")
			if explicit {
				args = append(args, "-auto-approve")
			}
			result := runTofuEnvironment(t, tree, "", []string{
				"TF_CLI_ARGS=-auto-approve=false -input=false",
				"TF_CLI_ARGS_apply=-auto-approve=true",
			}, args...)
			if explicit {
				requireSuccess(t, result, args...)
				return
			}
			if result.exitCode == 0 || !strings.Contains(result.stderr, "requires -auto-approve") {
				t.Fatalf("the apply did not preserve native environment precedence: %s", result.stderr)
			}
			requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
		})
	}
}

func stringBool(value bool) string {
	if value {
		return "explicit"
	}
	return "environment"
}

func TestTofuEnvironmentPreservesQuotedVariablesAndExplicitValues(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(stringBool(explicit), func(t *testing.T) {
			tree := independentUnlockedTree(t)
			writeFixtureFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "main.tf"), workspaceFixture(t, "environment-args.tf"))
			args := tofuArgs("suburban/tack", "apply", "-auto-approve", "-input=false")
			want := "global literal $HOME `uname`"
			if explicit {
				want = "explicit value with spaces"
				args = append(args, "-var=message="+want)
			}
			result := runTofuEnvironment(t, tree, "", []string{
				"TF_CLI_ARGS=-var 'message=global literal $HOME `uname`'",
				"TF_CLI_ARGS_apply=-var 'message=command value'",
			}, args...)
			requireSuccess(t, result, args...)
			if got := readOutput(t, tree, "suburban/tack", "message"); got != want {
				t.Fatalf("the applied variable was %q, want %q", got, want)
			}
		})
	}
}

func TestTofuEnvironmentSupportsMultiwordCommandSuffix(t *testing.T) {
	tree := independentUnlockedTree(t)
	args := tofuArgs("suburban/tack", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	show := tofuArgs("suburban/tack", "state", "list")
	result := runTofuEnvironment(t, tree, "", []string{"TF_CLI_ARGS_state_list=-id=not-present"}, show...)
	requireSuccess(t, result, show...)
	if strings.TrimSpace(result.stdout) != "" {
		t.Fatal("the state list command ignored its command-specific environment arguments")
	}
}

func TestTofuEnvironmentRejectsMalformedArgumentsWithoutValues(t *testing.T) {
	const marker = "synthetic-malformed-environment-argument"
	for _, variable := range []string{"TF_CLI_ARGS", "TF_CLI_ARGS_apply"} {
		t.Run(variable, func(t *testing.T) {
			tree := independentUnlockedTree(t)
			args := tofuArgs("suburban/tack", "apply", "-auto-approve")
			result := runTofuEnvironment(t, tree, "", []string{variable + "=-var 'message=" + marker}, args...)
			if result.exitCode == 0 || !strings.Contains(result.stderr, "check quoting and escaping") || !strings.Contains(result.stderr, variable) {
				t.Fatal("the malformed environment arguments did not fail with their variable name")
			}
			if strings.Contains(result.stdout+result.stderr, marker) {
				t.Fatal("the malformed environment argument value appeared in output")
			}
			requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
		})
	}
}

func writeMigrationPause(t *testing.T, tree configsTree) {
	t.Helper()
	configuration := filepath.Join(tree.home, ".config")
	if runtime.GOOS == "darwin" {
		configuration = filepath.Join(tree.home, "Library", "Application Support")
	}
	writeFixtureFile(t, filepath.Join(configuration, "configsctl", "state-migration", "pause.json"), "synthetic-retained-pause")
}

func TestTofuMigrationPauseRejectsInitCopyFlags(t *testing.T) {
	for _, flag := range []string{"-force-copy", "-migrate-state"} {
		for _, fromEnvironment := range []bool{false, true} {
			t.Run(flag+"/"+stringBool(fromEnvironment), func(t *testing.T) {
				tree := newConfigsTree(t)
				writeMigrationPause(t, tree)
				args := tofuArgs("alpha", "init", "-input=false")
				var environment []string
				if fromEnvironment {
					environment = []string{"TF_CLI_ARGS_init=" + flag}
				} else {
					args = append(args, flag)
				}
				result := runTofuEnvironment(t, tree, "", environment, args...)
				if result.exitCode == 0 || !strings.Contains(result.stderr, "writes remain paused") {
					t.Fatalf("the init copy flag bypassed the migration pause: %s", result.stderr)
				}
				requireNoFile(t, filepath.Join(tree.workspaces, "alpha", ".terraform"))
			})
		}
	}
}

func TestTofuMigrationPauseRejectsCredentialPlanBeforeAuthentication(t *testing.T) {
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settingsContent+readCredentialFixture(t, "credentials.yml"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "main.tf"), readCredentialFixture(t, "credentials.tf"))
	writeMigrationPause(t, tree)
	args := tofuArgs("alpha", "plan", "-input=false")
	result := runTofuEnvironment(t, tree, "", nil, args...)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "writes remain paused") {
		t.Fatalf("credential preparation bypassed the migration pause: %s", result.stderr)
	}
	for _, fragment := range []string{"hostlock.acquired", "tofu.command_started", "iDRAC credential"} {
		if strings.Contains(result.stderr, fragment) {
			t.Fatalf("the paused credential plan attempted an operation: %s", result.stderr)
		}
	}
}

func TestTofuMigrationPauseRejectsTaintAndUntaint(t *testing.T) {
	for _, operation := range []string{"taint", "untaint"} {
		t.Run(operation, func(t *testing.T) {
			tree := independentUnlockedTree(t)
			root := filepath.Join(tree.workspaces, "suburban", "tack")
			writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "environment-args.tf"))
			apply := tofuArgs("suburban/tack", "apply", "-auto-approve", "-input=false")
			requireSuccess(t, runConfigsctl(t, tree, apply...), apply...)
			if operation == "untaint" {
				taint := tofuArgs("suburban/tack", "taint", "terraform_data.argument")
				requireSuccess(t, runConfigsctl(t, tree, taint...), taint...)
			}
			state := filepath.Join(root, "tack.tfstate")
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			writeMigrationPause(t, tree)
			args := tofuArgs("suburban/tack", operation, "terraform_data.argument")
			refused := runConfigsctl(t, tree, args...)
			if refused.exitCode == 0 || !strings.Contains(refused.stderr, "writes remain paused") || strings.Contains(refused.stderr, "tofu.command_started") {
				t.Fatalf("the native state marker command bypassed the migration pause: %s", refused.stderr)
			}
			after, err := os.ReadFile(state)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("the paused state marker command changed backend bytes")
			}
		})
	}
}
