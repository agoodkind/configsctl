package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const sourceFixtureIgnore = ".terraform/\n.terraform.lock.hcl\n*.tfstate*\n*.tfplan\n*.plan\ntmp/\n"

func sourceGit(t *testing.T, tree configsTree, arguments ...string) string {
	t.Helper()
	settings := []string{"-c", "user.name=Configsctl Test", "-c", "user.email=configsctl-test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	command := exec.CommandContext(t.Context(), "git", append(settings, arguments...)...)
	command.Dir = tree.root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the temporary source repository command %s failed: %v\n%s", arguments[0], err, output)
	}
	return strings.TrimSpace(string(output))
}

func publishConfigsTree(t *testing.T, tree configsTree) {
	t.Helper()
	origin := filepath.Join(filepath.Dir(tree.root), filepath.Base(tree.root)+"-origin.git")
	if _, err := os.Stat(origin); os.IsNotExist(err) {
		sourceGit(t, tree, "init", "--bare", "--initial-branch=main", origin)
	} else if err != nil {
		t.Fatal(err)
	}
	remotes := strings.Fields(sourceGit(t, tree, "remote"))
	if slices.Contains(remotes, "origin") {
		sourceGit(t, tree, "remote", "set-url", "origin", origin)
	} else {
		sourceGit(t, tree, "remote", "add", "origin", origin)
	}
	if sourceGit(t, tree, "status", "--porcelain") != "" {
		sourceGit(t, tree, "add", "--all")
		sourceGit(t, tree, "commit", "-m", "Record synthetic configuration")
	}
	sourceGit(t, tree, "push", "--quiet", "--force", "origin", "HEAD:main")
}

func initializedSourceTree(t *testing.T) configsTree {
	t.Helper()
	requireTofu(t)
	tree := newConfigsTree(t)
	arguments := tofuArgs("alpha", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
	return tree
}

func sourceFeature(t *testing.T, tree configsTree) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "feature.tf"), "output \"feature\" { value = \"unmerged\" }\n")
	sourceGit(t, tree, "add", "workspaces/alpha/feature.tf")
	sourceGit(t, tree, "commit", "-m", "Add unmerged synthetic configuration")
}

func requireSourceRefusal(t *testing.T, tree configsTree, result commandResult, fragment string) {
	t.Helper()
	if result.exitCode == 0 || !strings.Contains(result.stderr, fragment) {
		t.Fatalf("expected source refusal %q, got exit %d: %s", fragment, result.exitCode, result.stderr)
	}
	if strings.Contains(result.stderr, "command_started") || strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the refused source started an operation: %s", result.stderr)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "alpha", "alpha.tfstate"))
}

func TestTofuSourceRefusesUnmergedWrites(t *testing.T) {
	commands := [][]string{
		{"apply", "-auto-approve", "-input=false"},
		{"destroy", "-auto-approve", "-input=false"},
		{"refresh", "-input=false"},
		{"import", "terraform_data.item", "existing"},
		{"state", "rm", "terraform_data.item"},
		{"force-unlock", "-force", "existing"},
	}
	for _, command := range commands {
		t.Run(command[0], func(t *testing.T) {
			tree := initializedSourceTree(t)
			sourceFeature(t, tree)
			result := runConfigsctl(t, tree, tofuArgs("alpha", command...)...)
			requireSourceRefusal(t, tree, result, "is not on origin/main")
		})
	}
}

func TestTofuSourceRefusesDirtyWrites(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("tracked=%t", tracked), func(t *testing.T) {
			tree := initializedSourceTree(t)
			path := filepath.Join(tree.workspaces, "alpha", "untracked.tf")
			if tracked {
				path = filepath.Join(tree.workspaces, "alpha", "main.tf")
			}
			writeFixtureFile(t, path, "output \"dirty\" { value = true }\n")
			result := runConfigsctl(t, tree, tofuArgs("alpha", "apply", "-auto-approve", "-input=false")...)
			requireSourceRefusal(t, tree, result, "commit or remove configuration changes")
		})
	}
}

func TestTofuSourceRejectsUnmergedPlanAfterCheckoutChanges(t *testing.T) {
	for _, option := range []string{"-input=false", "--"} {
		t.Run(option, func(t *testing.T) {
			tree := initializedSourceTree(t)
			merged := sourceGit(t, tree, "rev-parse", "HEAD")
			sourceFeature(t, tree)
			plan := filepath.Join(t.TempDir(), "unmerged.tfplan")
			arguments := tofuArgs("alpha", "plan", "-input=false", "-out="+plan)
			requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
			sourceGit(t, tree, "checkout", "--detach", merged)
			result := runConfigsctl(t, tree, tofuArgs("alpha", "apply", option, plan)...)
			requireSourceRefusal(t, tree, result, "recorded source does not match")
		})
	}
}

func TestTofuSourceAllowsExactMergedSavedPlan(t *testing.T) {
	tree := initializedSourceTree(t)
	plan := filepath.Join(t.TempDir(), "merged.tfplan")
	arguments := tofuArgs("alpha", "plan", "-input=false", "-out="+plan)
	requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
	apply := tofuArgs("alpha", "apply", "-input=false", plan)
	requireSuccess(t, runConfigsctl(t, tree, apply...), apply...)
	requireOutputLength(t, tree, "alpha", "alpha_length", vaultAlphaValue)
}

func TestTofuSourceRejectsChangedPlanBytes(t *testing.T) {
	tree := initializedSourceTree(t)
	plan := filepath.Join(t.TempDir(), "changed.tfplan")
	arguments := tofuArgs("alpha", "plan", "-input=false", "-out="+plan)
	requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
	file, err := os.OpenFile(plan, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("changed"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result := runConfigsctl(t, tree, tofuArgs("alpha", "apply", "-input=false", plan)...)
	requireSourceRefusal(t, tree, result, "source record is unavailable")
}

func TestTofuSourceRejectsDirtyPreviewAfterCleanup(t *testing.T) {
	tree := initializedSourceTree(t)
	path := filepath.Join(tree.workspaces, "alpha", "preview.tf")
	writeFixtureFile(t, path, "output \"preview\" { value = true }\n")
	plan := filepath.Join(t.TempDir(), "preview.tfplan")
	arguments := tofuArgs("alpha", "plan", "-input=false", "-out="+plan)
	requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result := runConfigsctl(t, tree, tofuArgs("alpha", "apply", "-input=false", plan)...)
	requireSourceRefusal(t, tree, result, "recorded source does not match")
}

func TestTofuSourceRefusesUnavailableOrigin(t *testing.T) {
	tree := initializedSourceTree(t)
	sourceGit(t, tree, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	result := runConfigsctl(t, tree, tofuArgs("alpha", "apply", "-auto-approve", "-input=false")...)
	requireSourceRefusal(t, tree, result, "refresh origin/main")
}

func TestTofuSourceAllowsUnmergedReadOnlyCommands(t *testing.T) {
	tree := initializedSourceTree(t)
	sourceFeature(t, tree)
	for _, command := range []string{"plan", "validate", "version"} {
		arguments := tofuArgs("alpha", command)
		requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
	}
}

func TestTofuSourceAllowsReadOnlyStateWithGlobalOptions(t *testing.T) {
	tree := initializedSourceTree(t)
	applyWorkspace(t, tree, "alpha")
	sourceFeature(t, tree)
	arguments := tofuArgs("alpha", "-chdir=.", "state", "list")
	requireSuccess(t, runConfigsctl(t, tree, arguments...), arguments...)
}

func TestTofuSourceRefusesUnmergedCredentialPlan(t *testing.T) {
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+readCredentialFixture(t, "credentials.yml"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "main.tf"), readCredentialFixture(t, "credentials.tf"))
	publishConfigsTree(t, tree)
	sourceFeature(t, tree)
	result := runConfigsctl(t, tree, tofuArgs("alpha", "plan", "-input=false")...)
	requireSourceRefusal(t, tree, result, "is not on origin/main")
}
