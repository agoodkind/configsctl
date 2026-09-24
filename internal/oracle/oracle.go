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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// script is the jinja2 oracle. It ships inside the binary, so the linter does not
// depend on any file in the working directory to classify a routed form.
//
//go:embed lint_ansible_ast.py
var script []byte

// scriptDirPattern names the private directory that holds the temporary copy.
const scriptDirPattern = "configsctl-oracle-*"

// scriptName is the file name of the copy python3 runs.
const scriptName = "lint_ansible_ast.py"

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
	scriptDir, scriptPath, err := writeScript()
	if err != nil {
		return nil, err
	}
	defer removeScriptDir(scriptDir)
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	cmd, err := pythonWithJinja(ctx, scriptPath)
	if err != nil {
		return nil, err
	}
	// -I runs python3 in isolated mode: the script's directory, the user
	// site-packages, and every PYTHON* environment variable stay off the import
	// path, so a module another user plants in the temp directory or names in
	// PYTHONPATH is never imported.
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

// pythonWithJinja returns a command for a Python that imports jinja2 in isolated
// mode. On macOS, it also checks the standard Homebrew Python locations.
func pythonWithJinja(ctx context.Context, scriptPath string) (*exec.Cmd, error) {
	if err := exec.CommandContext(ctx, "python3", "-I", "-c", "import jinja2").Run(); err == nil {
		slog.Debug("oracle Python selected", "python", "python3")
		return exec.CommandContext(ctx, "python3", "-I", scriptPath, "--route"), nil
	}
	if runtime.GOOS == "darwin" {
		if err := exec.CommandContext(ctx, "/opt/homebrew/bin/python3", "-I", "-c", "import jinja2").Run(); err == nil {
			slog.Debug("oracle Python selected", "python", "/opt/homebrew/bin/python3")
			return exec.CommandContext(ctx, "/opt/homebrew/bin/python3", "-I", scriptPath, "--route"), nil
		}
		if err := exec.CommandContext(ctx, "/usr/local/bin/python3", "-I", "-c", "import jinja2").Run(); err == nil {
			slog.Debug("oracle Python selected", "python", "/usr/local/bin/python3")
			return exec.CommandContext(ctx, "/usr/local/bin/python3", "-I", scriptPath, "--route"), nil
		}
	}
	err := errors.New("no supported python3 can import jinja2 in isolated mode")
	slog.Error("oracle Python unavailable", "err", err)
	return nil, err
}

// writeScript copies the embedded oracle into a new directory in the host temp
// directory and returns that directory and the script path. [os.MkdirTemp]
// creates the directory at 0700 under a name no other run shares, so no other
// user can place a file beside the script.
func writeScript() (string, string, error) {
	dir, err := os.MkdirTemp("", scriptDirPattern)
	if err != nil {
		slog.Error("create oracle script dir failed", "err", err)
		return "", "", fmt.Errorf("create oracle script dir: %w", err)
	}
	path := filepath.Join(dir, scriptName)
	if err := os.WriteFile(path, script, 0o600); err != nil {
		removeScriptDir(dir)
		slog.Error("write oracle script failed", "path", path, "err", err)
		return "", "", fmt.Errorf("write oracle script: %w", err)
	}
	return dir, path, nil
}

// removeScriptDir deletes the private directory and the oracle copy inside it. A
// failed removal leaves a small directory in the host temp directory, which is
// logged rather than returned because the lint result it served is already valid.
func removeScriptDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("remove oracle script dir failed", "dir", dir, "err", err)
	}
}
