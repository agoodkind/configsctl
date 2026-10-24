package statemigration_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/statemigration"
)

func finalizeMigrationFixture(t *testing.T, engine *statemigration.Engine) {
	t.Helper()
	original := privateFixtureDir(t)
	cutover := privateFixtureDir(t)
	originals := make(map[string]statemigration.Workspace)
	workspaces := make(map[string]statemigration.Workspace)
	for _, spec := range engine.Manifest.Workspaces {
		workspace := engine.Workspaces[spec.Selector]
		if spec.Before.Lineage != "" {
			var exists bool
			workspace, exists = engine.OriginalWorkspaces[spec.Selector]
			if !exists {
				t.Fatalf("original fixture workspace %s lacks its configuration", spec.Selector)
			}
			originals[spec.Selector] = copyMigrationFixture(t, workspace, filepath.Join(original, "workspaces", spec.Selector))
		}
		configured := copyMigrationFixture(t, workspace, filepath.Join(cutover, "workspaces", spec.Selector))
		if spec.Before.Lineage != "" {
			configureFixtureCutover(t, configured.Directory, spec.Selector, engine.Manifest.Moves)
		}
		workspaces[spec.Selector] = configured
	}
	engine.Manifest.RestoreCommit = commitFixtureConfiguration(t, original)
	engine.Manifest.Commit = commitFixtureConfiguration(t, cutover)
	engine.Manifest.RestoreConfigurationDirectory = original
	engine.Manifest.ConfigurationDirectory = cutover
	engine.CurrentCommit = engine.Manifest.Commit
	engine.OriginalWorkspaces = originals
	engine.RestoreWorkspaces = originals
	engine.Workspaces = workspaces
	for _, workspace := range originals {
		nativeCommand(t, workspace, "init", "-reconfigure", "-input=false", "-no-color")
	}
}

func copyMigrationFixture(t *testing.T, source statemigration.Workspace, destination string) statemigration.Workspace {
	t.Helper()
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(source.Directory, "*.tf"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the native fixture lacks its HCL configuration: %v", err)
	}
	for _, path := range files {
		file := readFixtureHCL(t, path)
		for _, block := range file.Body().Blocks() {
			if block.Type() != "terraform" {
				continue
			}
			backend := block.Body().FirstMatchingBlock("backend", []string{"local"})
			if backend == nil {
				continue
			}
			backend.Body().SetAttributeValue("path", cty.StringVal(fixtureBackendPath(t, source)))
		}
		writeFixtureHCL(t, filepath.Join(destination, filepath.Base(path)), file)
	}
	return statemigration.Workspace{Directory: destination, Environment: source.Environment}
}

func fixtureBackendPath(t *testing.T, workspace statemigration.Workspace) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(workspace.Directory, "*.tf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		file := readFixtureHCL(t, path)
		for _, block := range file.Body().Blocks() {
			if block.Type() != "terraform" {
				continue
			}
			backend := block.Body().FirstMatchingBlock("backend", []string{"local"})
			if backend == nil {
				continue
			}
			attribute := backend.Body().GetAttribute("path")
			if attribute == nil {
				t.Fatal("the local fixture backend lacks its explicit path")
			}
			expression, diagnostics := hclsyntax.ParseExpression(attribute.Expr().BuildTokens(nil).Bytes(), path, hcl.InitialPos)
			if diagnostics.HasErrors() {
				t.Fatal("the fixture backend path is not a native expression")
			}
			value, diagnostics := expression.Value(nil)
			if diagnostics.HasErrors() || value.IsNull() || !value.IsKnown() || value.Type() != cty.String {
				t.Fatal("the fixture backend path must be a literal string")
			}
			state := value.AsString()
			if !filepath.IsAbs(state) {
				state = filepath.Join(workspace.Directory, state)
			}
			return state
		}
	}
	t.Fatal("the fixture does not declare a local backend")
	return ""
}

func configureFixtureCutover(t *testing.T, directory, selector string, moves []statemigration.Transfer) {
	t.Helper()
	var retained []statemigration.Transfer
	for _, move := range moves {
		if move.Destination == selector {
			if move.Source != selector {
				t.Fatal("the native fixture requires an explicit configuration for a mixed source and destination root")
			}
			retained = append(retained, move)
		}
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.tf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		file := readFixtureHCL(t, path)
		if len(retained) == 0 {
			for _, block := range file.Body().Blocks() {
				if block.Type() != "terraform" && block.Type() != "variable" {
					file.Body().RemoveBlock(block)
				}
			}
		} else {
			renameFixtureInstances(t, file, retained)
		}
		writeFixtureHCL(t, path, file)
	}
}

func renameFixtureInstances(t *testing.T, file *hclwrite.File, moves []statemigration.Transfer) {
	t.Helper()
	for index, move := range moves {
		if move.Address == move.NewAddress {
			continue
		}
		before := strings.Split(move.Address, ".")
		staged := append([]string(nil), before...)
		staged[len(staged)-1] = fmt.Sprintf("fixture_transfer_%d", index)
		renameFixtureReference(t, file.Body(), before, staged)
	}
	for index, move := range moves {
		if move.Address == move.NewAddress {
			continue
		}
		staged := strings.Split(move.Address, ".")
		staged[len(staged)-1] = fmt.Sprintf("fixture_transfer_%d", index)
		renameFixtureReference(t, file.Body(), staged, strings.Split(move.NewAddress, "."))
	}
}

func renameFixtureReference(t *testing.T, body *hclwrite.Body, before, after []string) {
	t.Helper()
	if len(before) != len(after) || len(before) < 2 || len(before) > 3 || strings.Contains(strings.Join(before, "."), "[") {
		t.Fatal("the native fixture rename requires a root resource or data source")
	}
	for _, attribute := range body.Attributes() {
		attribute.Expr().RenameVariablePrefix(before, after)
	}
	for _, block := range body.Blocks() {
		labels := block.Labels()
		if block.Type() == "resource" && len(before) == 2 && len(labels) == 2 && labels[0] == before[0] && labels[1] == before[1] {
			block.SetLabels(after)
		}
		if block.Type() == "data" && len(before) == 3 && before[0] == "data" && len(labels) == 2 && labels[0] == before[1] && labels[1] == before[2] {
			block.SetLabels(after[1:])
		}
		renameFixtureReference(t, block.Body(), before, after)
	}
}

func readFixtureHCL(t *testing.T, path string) *hclwrite.File {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, diagnostics := hclwrite.ParseConfig(data, path, hcl.InitialPos)
	if diagnostics.HasErrors() {
		t.Fatal("the native fixture contains invalid HCL")
	}
	return file
}

func writeFixtureHCL(t *testing.T, path string, file *hclwrite.File) {
	t.Helper()
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitFixtureConfiguration(t *testing.T, directory string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(directory, ".git")); os.IsNotExist(err) {
		fixtureGitCommand(t, directory, "init", "--initial-branch=main")
		ignore := []byte(".terraform/\n*.tfstate\n*.tfstate.*\n.terraform.lock.hcl\n")
		if err := os.WriteFile(filepath.Join(directory, ".gitignore"), ignore, 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	fixtureGitCommand(t, directory, "add", ".")
	fixtureGitCommand(t, directory, "commit", "-m", "Record native migration fixture")
	return strings.TrimSpace(fixtureGitCommand(t, directory, "rev-parse", "HEAD"))
}

func fixtureGitCommand(t *testing.T, directory string, args ...string) string {
	t.Helper()
	settings := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Configs native fixture", "-c", "user.email=configs-fixture@example.invalid"}
	command := exec.CommandContext(t.Context(), "git", append(settings, args...)...)
	command.Dir = directory
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		t.Fatalf("the native Git fixture command %s failed: %v", args[0], err)
	}
	return output.String()
}
