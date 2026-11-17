// Package sourcelint checks active OpenTofu source with upstream TFLint.
package sourcelint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"goodkind.io/configsctl/internal/tofufiles"
)

const parserSummaryParagraphs = 2

var (
	errSourceType         = errors.New("source lint requires regular source files")
	errDiagnosticLocation = errors.New("TFLint diagnostic does not identify a copied source file")
)

// Run checks a private copy without changing configuration or provider caches.
//
// Active OpenTofu files replace matching Terraform files in the copy. TFLint
// checks each directory independently without initializing child modules.
//
// Example:
//
//	err := sourcelint.Run(ctx, "opentofu")
func Run(ctx context.Context, root string) error {
	slog.Info("tofu.source_lint.started", "directory", root)
	binary, err := exec.LookPath("tflint")
	if err != nil {
		slog.Error("tofu.source_lint.binary_missing", "err", err)

		return fmt.Errorf("install TFLint v0.66.0 and add tflint to PATH: %w", err)
	}

	directory, err := os.MkdirTemp("", "configsctl-source-lint-*")
	if err != nil {
		slog.Error("tofu.source_lint.directory_failed", "err", err)

		return fmt.Errorf("create source lint directory: %w", err)
	}
	defer os.RemoveAll(directory)
	if err := copySources(root, directory); err != nil {
		return err
	}

	customErr := checkSourceRules(directory, root)

	config := filepath.Join(directory, ".tflint.hcl")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		slog.Error("tofu.source_lint.config_failed", "err", err)

		return fmt.Errorf("write source lint configuration: %w", err)
	}

	command := exec.CommandContext(
		ctx,
		binary,
		"--config="+config,
		"--recursive",
		"--call-module-type=none",
		"--no-color",
		"--format=json",
		"--only=terraform_unused_declarations",
		"--only=terraform_typed_variables",
		"--only=terraform_naming_convention",
		"--only=terraform_required_providers",
	)
	command.Dir = directory
	command.Stderr = os.Stderr
	output, runErr := command.Output()
	if len(output) > 0 {
		if err := printDiagnostics(output, directory, root); err != nil {
			return err
		}
	}

	if runErr != nil {
		slog.Error("tofu.source_lint.failed", "err", runErr)

		return errors.Join(
			customErr,
			fmt.Errorf("TFLint source checks failed for %s: %w", root, runErr),
		)
	}

	return customErr
}

type sourceRange struct {
	Filename string `json:"filename"`
	Start    struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"start"`
}

type diagnostic struct {
	Message string `json:"message"`
	Summary string `json:"summary"`
	Rule    struct {
		Name string `json:"name"`
	} `json:"rule"`
	Range sourceRange `json:"range"`
}

func printDiagnostics(output []byte, directory, root string) error {
	var report struct {
		Issues []diagnostic `json:"issues"`
		Errors []diagnostic `json:"errors"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		slog.Error("tofu.source_lint.diagnostics_failed", "err", err)

		return fmt.Errorf("decode TFLint diagnostics: %w", err)
	}

	for _, issue := range append(report.Issues, report.Errors...) {
		if issue.Range.Filename == "" {
			if err := printParserError(issue.Message, directory, root); err != nil {
				return err
			}

			continue
		}

		filename, err := originalFilename(issue.Range.Filename, directory, root)
		if err != nil {
			return err
		}

		fmt.Fprintf(
			os.Stderr,
			"%s:%d:%d: %s (%s)\n",
			filename,
			issue.Range.Start.Line,
			issue.Range.Start.Column,
			issue.Message,
			issue.Rule.Name,
		)
	}

	return nil
}

var diagnosticPath = regexp.MustCompile(`[^\s;:]+\.(?:tf|tofu)(?:\.json)?`)

func printParserError(message, directory, root string) error {
	paragraphs := strings.Split(message, "\n\n")
	message = strings.Join(paragraphs[:min(parserSummaryParagraphs, len(paragraphs))], "\n")
	var locationErr error
	message = diagnosticPath.ReplaceAllStringFunc(
		message,
		func(path string) string {
			filename, err := originalFilename(path, directory, root)
			if err != nil {
				locationErr = err
			}

			return filename
		},
	)
	if locationErr != nil {
		return locationErr
	}

	fmt.Fprintln(os.Stderr, "TFLint: "+message)

	return nil
}

func originalFilename(path, directory, root string) (string, error) {
	copyRoot, err := filepath.EvalSymlinks(directory)
	if err != nil {
		slog.Error("tofu.source_lint.copy_path_failed", "err", err)

		return "", fmt.Errorf("resolve source lint copy directory: %w", err)
	}

	copyPath := path
	if !filepath.IsAbs(copyPath) {
		copyPath = filepath.Join(directory, copyPath)
	}

	copyPath, err = filepath.EvalSymlinks(copyPath)
	if err != nil {
		slog.Error("tofu.source_lint.diagnostic_path_failed", "err", err)

		return "", fmt.Errorf("resolve copied diagnostic source: %w", err)
	}

	relative, err := filepath.Rel(copyRoot, copyPath)
	if err != nil || !filepath.IsLocal(relative) || relative == "." {
		slog.Error(
			"tofu.source_lint.location_failed",
			"err",
			errDiagnosticLocation,
			"filename",
			path,
		)

		return "", errDiagnosticLocation
	}

	filename := filepath.Join(root, relative)
	candidate := ""
	if stem, matched := strings.CutSuffix(filename, ".tf"); matched {
		candidate = stem + ".tofu"
	}

	if stem, matched := strings.CutSuffix(filename, ".tf.json"); matched {
		candidate = stem + ".tofu.json"
	}

	if candidate != "" {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			slog.Error("tofu.source_lint.location_read_failed", "err", err)

			return "", fmt.Errorf("read diagnostic source: %w", err)
		}
	}

	return filename, nil
}

func copySources(root, destination string) error {
	input, err := os.OpenRoot(root)
	if err != nil {
		slog.Error("tofu.source_lint.input_failed", "err", err)

		return fmt.Errorf("open source lint input: %w", err)
	}
	defer input.Close()
	output, err := os.OpenRoot(destination)
	if err != nil {
		slog.Error("tofu.source_lint.output_failed", "err", err)

		return fmt.Errorf("open source lint output: %w", err)
	}
	defer output.Close()
	if err := copyDirectory(input, output, "."); err != nil {
		slog.Error("tofu.source_lint.copy_failed", "err", err)

		return fmt.Errorf("copy active OpenTofu source: %w", err)
	}

	return nil
}

func copyDirectory(input, output *os.Root, directory string) error {
	entries, err := fs.ReadDir(input.FS(), directory)
	if err != nil {
		slog.Error("tofu.source_lint.discovery_failed", "err", err)

		return fmt.Errorf("read source lint directory: %w", err)
	}

	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names[entry.Name()] = true
		}
	}

	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			if err := copyDirectory(input, output, path); err != nil {
				return err
			}

			continue
		}

		if !tofufiles.Active(entry.Name(), names) {
			continue
		}

		if !entry.Type().IsRegular() {
			return errSourceType
		}

		if err := copyFile(input, output, path); err != nil {
			return err
		}
	}

	return nil
}

func copyFile(input, output *os.Root, source string) error {
	body, err := input.ReadFile(source)
	if err != nil {
		slog.Error("tofu.source_lint.read_failed", "path", source, "err", err)

		return fmt.Errorf("read source lint input %s: %w", source, err)
	}

	target := source
	if stem, matched := strings.CutSuffix(source, ".tofu"); matched {
		target = stem + ".tf"
	}

	if stem, matched := strings.CutSuffix(source, ".tofu.json"); matched {
		target = stem + ".tf.json"
	}

	if err := output.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		slog.Error("tofu.source_lint.mkdir_failed", "err", err)

		return fmt.Errorf("create source lint directory: %w", err)
	}

	if err := output.WriteFile(target, body, 0o600); err != nil {
		slog.Error("tofu.source_lint.write_failed", "err", err)

		return fmt.Errorf("copy source lint input %s: %w", source, err)
	}

	return nil
}
