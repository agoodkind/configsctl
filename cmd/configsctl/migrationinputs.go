package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	"goodkind.io/configsctl/internal/statemigration"
)

func requireMigrationInputs(ctx context.Context, workspace statemigration.Workspace) error {
	provided, err := migrationAutomaticInputs(ctx, workspace.Directory)
	if err != nil {
		return err
	}
	for _, assignment := range workspace.Environment {
		key, _, found := strings.Cut(assignment, "=")
		if name, variable := strings.CutPrefix(key, tofuVariablePrefix); variable && found {
			provided[name] = true
		}
	}
	blocks, err := tofuModuleBlocks(workspace.Directory, tofuVariableSchema)
	if err != nil {
		return err
	}
	schema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "default", Required: false}}}
	for _, block := range blocks {
		content, _, diagnostics := block.Body.PartialContent(schema)
		if diagnostics.HasErrors() {
			return errors.New("read migration input default and lifetime declarations")
		}
		if _, defaulted := content.Attributes["default"]; defaulted {
			continue
		}
		name := block.Labels[0]
		if !provided[name] {
			return fmt.Errorf("required migration input %s is missing in %s; supply TF_VAR_%s or a reviewed automatic variable file; migration plans ignore TF_CLI_ARGS", name, workspace.Directory, name)
		}
	}
	return nil
}

func migrationAutomaticInputs(ctx context.Context, directory string) (map[string]bool, error) {
	slog.Info("migration.input_files.inspection_started", "workspace", directory)
	provided := make(map[string]bool)
	var paths []string
	for _, name := range []string{"terraform.tfvars", "terraform.tfvars.json"} {
		path := filepath.Join(directory, name)
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("inspect the migration automatic variable files")
		}
	}
	for _, pattern := range []string{"*.auto.tfvars", "*.auto.tfvars.json"} {
		matches, err := filepath.Glob(filepath.Join(directory, pattern))
		if err != nil {
			return nil, errors.New("list the migration automatic variable files")
		}
		paths = append(paths, matches...)
	}
	for _, path := range paths {
		command := exec.CommandContext(ctx, "git", "ls-files", "-z", "--")
		command.Dir = directory
		tracked, err := command.Output()
		if err != nil {
			slog.ErrorContext(ctx, "migration.input_files.tracked_read_failed", "workspace", directory, "err", fmt.Sprintf("%T", err))
			return nil, fmt.Errorf("inspect the reviewed migration variable files: %w", err)
		}
		if !slices.Contains(strings.Split(string(tracked), "\x00"), filepath.Base(path)) {
			return nil, errors.New("migration automatic variable files must belong to the reviewed configuration commit; supply other values with TF_VAR variables")
		}
		parser := hclparse.NewParser()
		parse := parser.ParseHCLFile
		if strings.HasSuffix(path, ".json") {
			parse = parser.ParseJSONFile
		}
		file, diagnostics := parse(path)
		if diagnostics.HasErrors() {
			return nil, errors.New("parse the reviewed migration automatic variable file; values remain private")
		}
		attributes, diagnostics := file.Body.JustAttributes()
		if diagnostics.HasErrors() {
			return nil, errors.New("migration automatic variable files must contain native variable assignments")
		}
		for name := range attributes {
			provided[name] = true
		}
	}
	return provided, nil
}
