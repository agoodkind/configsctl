// Package oracle routes the Jinja forms the Go engine cannot read to the jinja2
// reference parser in lint_ansible_ast.py, which is embedded in this package. The
// Go engine parses most expressions itself; a few Ansible-Jinja forms, such as a
// parenthesized conditional piped into a filter, fail there and are routed here so
// a violation the Go engine could not classify is still enforced. The parser runs
// as a python3 subprocess because no Go Jinja parser reads these forms.
package oracle

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// script is the jinja2 oracle. It ships inside the binary, so the linter does not
// depend on any file in the working directory to classify a routed form.
//
//go:embed lint_ansible_ast.py
var script []byte

// scriptPattern names the temporary copy python3 runs.
const scriptPattern = "configsctl-oracle-*.py"

// routeTimeout bounds the subprocess, which parses a small batch of expressions.
const routeTimeout = 30 * time.Second

// Form is one expression to classify, with the runtime names (register, set_fact,
// and loop values) that a defensive read is allowed to reference.
type Form struct {
	Expr    string   `json:"expr"`
	Runtime []string `json:"runtime"`
}

// Violation is one banned construct the oracle resolved to an input variable.
type Violation struct {
	Kind string `json:"kind"`
	Root string `json:"root"`
}

// Result is the oracle verdict for one form, aligned to the input by index.
// Parsed reports whether jinja2 read the form; Violations holds the enforced
// constructs.
type Result struct {
	Parsed     bool        `json:"parsed"`
	Violations []Violation `json:"violations"`
}

// Route classifies the forms with the jinja2 oracle and returns one result per
// form in input order. An empty input returns no results and no error. A missing
// interpreter, an unwritable temporary script, or a malformed response is an
// error the caller surfaces, since a routed form cannot be silently passed.
func Route(forms []Form) ([]Result, error) {
	if len(forms) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(forms)
	if err != nil {
		slog.Error("marshal oracle forms failed", "err", err)
		return nil, fmt.Errorf("marshal oracle forms: %w", err)
	}
	scriptPath, err := writeScript()
	if err != nil {
		return nil, err
	}
	defer removeScript(scriptPath)
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", scriptPath, "--route")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		slog.Error("oracle route failed", "script", scriptPath, "stderr", stderr.String(), "err", runErr)
		return nil, fmt.Errorf("python oracle route: %w", runErr)
	}
	var results []Result
	if decodeErr := json.Unmarshal(stdout.Bytes(), &results); decodeErr != nil {
		slog.Error("decode oracle response failed", "err", decodeErr)
		return nil, fmt.Errorf("decode oracle response: %w", decodeErr)
	}
	return results, nil
}

// writeScript copies the embedded oracle to a new file in the host temp
// directory and returns its path. [os.CreateTemp] creates the file at 0600 under a
// name no other run shares.
func writeScript() (string, error) {
	file, err := os.CreateTemp("", scriptPattern)
	if err != nil {
		slog.Error("create oracle script failed", "err", err)
		return "", fmt.Errorf("create oracle script: %w", err)
	}
	if _, err := file.Write(script); err != nil {
		_ = file.Close()
		removeScript(file.Name())
		slog.Error("write oracle script failed", "path", file.Name(), "err", err)
		return "", fmt.Errorf("write oracle script: %w", err)
	}
	if err := file.Close(); err != nil {
		removeScript(file.Name())
		slog.Error("close oracle script failed", "path", file.Name(), "err", err)
		return "", fmt.Errorf("close oracle script: %w", err)
	}
	return file.Name(), nil
}

// removeScript deletes the temporary oracle copy. A failed removal leaves a
// small file in the host temp directory, which is logged rather than returned
// because the lint result it served is already valid.
func removeScript(path string) {
	if err := os.Remove(path); err != nil {
		slog.Warn("remove oracle script failed", "path", path, "err", err)
	}
}
