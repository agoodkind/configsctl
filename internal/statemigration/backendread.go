package statemigration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const nativeDiagnosticError = "error"

var errBackendInit = errors.New("OpenTofu could not initialize the private state backend")

// OpenTofu initializes the backend before it installs providers from state.
// Listing workspaces and pulling state do not require the provider binaries.
func initializeReadBackend(ctx context.Context, workspace Workspace, backendArgs []string) error {
	plugins := filepath.Join(workspace.Directory, "empty-plugins")
	if err := os.Mkdir(plugins, privateDirectoryMode); err != nil {
		slog.ErrorContext(ctx, "retirement.plugin_directory_failed", "err", err)

		return fmt.Errorf("create the private plugin directory: %w", err)
	}

	args := append(
		[]string{
			"init",
			"-json",
			"-get=false",
			"-lock=false",
			"-input=false",
			"-plugin-dir=" + plugins,
		},
		backendArgs...,
	)
	cmd := nativeProcess(ctx, workspace, "default", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err == nil {
		return nil
	}

	exit, ok := errors.AsType[*exec.ExitError](err)
	if ok && exit.ExitCode() == 1 && onlyMissingProviders(out.Bytes(), plugins) {
		slog.InfoContext(ctx, "retirement.backend_initialized_without_providers")

		return nil
	}

	slog.ErrorContext(ctx, "retirement.backend_init_failed", "err", migrationErrorType(err))

	return errBackendInit
}

func onlyMissingProviders(output []byte, plugins string) bool {
	decoder := json.NewDecoder(bytes.NewReader(output))
	configured, installing, missing := false, false, false

	for {
		var event struct {
			Level      string `json:"@level"`
			Module     string `json:"@module"`
			Message    string `json:"@message"`
			Type       string `json:"type"`
			Diagnostic struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Detail   string `json:"detail"`
			} `json:"diagnostic"`
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			return missing
		}

		if err != nil || event.Module != "tofu.ui" {
			return false
		}

		if event.Level == "info" &&
			strings.HasPrefix(event.Message, "Successfully configured the backend ") {
			configured = true
		}

		if configured && event.Level == "info" &&
			event.Message == "Initializing provider plugins..." {
			installing = true
		}

		if event.Level != nativeDiagnosticError &&
			event.Diagnostic.Severity != nativeDiagnosticError {
			continue
		}

		if !installing || event.Level != nativeDiagnosticError || event.Type != "diagnostic" ||
			event.Diagnostic.Severity != nativeDiagnosticError ||
			event.Diagnostic.Summary != "Failed to query available provider packages" ||
			!strings.HasSuffix(
				event.Diagnostic.Detail,
				"was not found in any of the search locations\n\n  - "+plugins,
			) {
			return false
		}

		missing = true
	}
}
