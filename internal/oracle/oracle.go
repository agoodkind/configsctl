// Package oracle routes the Jinja forms the Go engine cannot read to the jinja2
// reference parser in scripts/lint_ansible_ast.py. The Go engine parses most
// expressions itself; a few Ansible-Jinja forms, such as a parenthesized
// conditional piped into a filter, fail there and are routed here so a violation
// the Go engine could not classify is still enforced. The parser runs as a
// python3 subprocess because no Go Jinja parser reads these forms.
package oracle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// scriptRelPath is the oracle location relative to the repository root, which is
// the working directory the linter runs from.
const scriptRelPath = "scripts/lint_ansible_ast.py"

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
// interpreter, a missing script, or a malformed response is an error the caller
// surfaces, since a routed form cannot be silently passed.
func Route(forms []Form) ([]Result, error) {
	if len(forms) == 0 {
		return nil, nil
	}
	script, err := locateScript()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(forms)
	if err != nil {
		slog.Error("marshal oracle forms failed", "err", err)
		return nil, fmt.Errorf("marshal oracle forms: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", script, "--route")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		slog.Error("oracle route failed", "script", script, "stderr", stderr.String(), "err", runErr)
		return nil, fmt.Errorf("python oracle route: %w", runErr)
	}
	var results []Result
	if decodeErr := json.Unmarshal(stdout.Bytes(), &results); decodeErr != nil {
		slog.Error("decode oracle response failed", "err", decodeErr)
		return nil, fmt.Errorf("decode oracle response: %w", decodeErr)
	}
	return results, nil
}

// locateScript returns the oracle script path, honoring the CONFIGS_ORACLE
// override so a test can point at a copy.
func locateScript() (string, error) {
	if override := os.Getenv("CONFIGS_ORACLE"); override != "" {
		return override, nil
	}
	if _, err := os.Stat(scriptRelPath); err != nil {
		slog.Error("locate oracle script failed", "path", scriptRelPath, "err", err)
		return "", fmt.Errorf("locate oracle script %q: %w", scriptRelPath, err)
	}
	return scriptRelPath, nil
}
