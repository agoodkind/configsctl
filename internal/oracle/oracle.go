// Package oracle checks Ansible Jinja expressions that the Go parser cannot parse,
// including parenthesized conditionals piped into filters. The embedded Python
// parser returns a parse result and any violations for each expression.
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

//go:embed lint_ansible_ast.py
var script []byte

const scriptDirPattern = "configsctl-oracle-*"

const scriptName = "lint_ansible_ast.py"

const routeTimeout = 30 * time.Second

// Form contains a Jinja expression and the runtime names it may reference.
type Form struct {
	Expr    string   `json:"expr"`
	Runtime []string `json:"runtime"`
}

// Violation identifies a banned construct and its input variable.
type Violation struct {
	Kind string `json:"kind"`
	Root string `json:"root"`
}

// Result reports whether Jinja2 parsed a form and lists its violations.
type Result struct {
	Parsed     bool        `json:"parsed"`
	Violations []Violation `json:"violations"`
}

// Route parses each form with Jinja2 and returns results in input order.
// It returns an error if the parser cannot run or produces an invalid response.
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

// pythonWithJinja returns a command for the first Python that imports Jinja2 in
// isolated mode. Isolated mode excludes the script directory, user packages,
// and PYTHONPATH from imports, preventing a substituted module from loading.
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

// writeScript stores the parser in a unique 0700 temporary directory.
// Other users cannot replace the script before Python starts.
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

// removeScriptDir logs cleanup failures without changing the lint result.
func removeScriptDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("remove oracle script dir failed", "dir", dir, "err", err)
	}
}
