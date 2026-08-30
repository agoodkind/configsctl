package ansible

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The app reaches the ledger over a multi-host connection string. Stopping one
// data guest black-holes its socket rather than refusing it, so with no bound
// on each name the driver waits out the kernel's TCP retry on the first name
// and never reaches a surviving node. These tests render the real template
// against the real group_vars and assert the deploy writes that bound.

type renderedComposeOverride struct {
	Services struct {
		App struct {
			Environment struct {
				DatabaseURL string `yaml:"DATABASE_URL"`
			} `yaml:"environment"`
		} `yaml:"app"`
	} `yaml:"services"`
}

func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(workingDirectory, "..", "..", ".."))
}

// tackConnectTimeoutSeconds reads the bound from the group_vars file that owns
// it, so the assertions below are tied to that one durable home.
func tackConnectTimeoutSeconds(t *testing.T, repositoryRoot string) int {
	t.Helper()
	path := filepath.Join(
		repositoryRoot, "ansible", "inventory", "group_vars", "tack_all.yml",
	)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	var groupVars struct {
		ConnectTimeoutSeconds *int `yaml:"tack_database_connect_timeout_seconds"`
	}
	if err := yaml.Unmarshal(contents, &groupVars); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if groupVars.ConnectTimeoutSeconds == nil {
		t.Fatalf("%s does not set tack_database_connect_timeout_seconds", path)
	}
	if *groupVars.ConnectTimeoutSeconds <= 0 {
		t.Fatalf(
			"tack_database_connect_timeout_seconds = %d, want a positive whole number of seconds",
			*groupVars.ConnectTimeoutSeconds,
		)
	}
	return *groupVars.ConnectTimeoutSeconds
}

// renderTackOverride renders the compose override for a repointed owner guest,
// the shape that carries the app's multi-host connection string.
func renderTackOverride(t *testing.T, repositoryRoot string) renderedComposeOverride {
	t.Helper()
	ansiblePlaybook, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Fatalf("ansible-playbook is required: %v", err)
	}

	outputFile := filepath.Join(t.TempDir(), "docker-compose.override.yml")
	vaultPasswordFile := filepath.Join(t.TempDir(), "vault-password")
	if err := os.WriteFile(vaultPasswordFile, []byte("unused\n"), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", vaultPasswordFile, err)
	}

	extraVariables, err := json.Marshal(map[string]any{
		"group_vars_file": filepath.Join(
			repositoryRoot, "ansible", "inventory", "group_vars", "tack_all.yml",
		),
		"template_file": filepath.Join(
			repositoryRoot, "tack", "docker-compose.override.yml.j2",
		),
		"output_file": outputFile,
		// A repointed owner guest: the app dials the three data nodes.
		"tack_provision_owner":            true,
		"tack_ledger_consumers_repointed": true,
		"tack_ledger_legacy_node_present": false,
		"tack_ledger_node_name":           "yugabyte",
		"tack_ledger_join_target":         "",
		"tack_store_host":                 "3d06:bad:b01:10::20",
		"tack_ledger_node_addresses": map[string]string{
			"yb1": "3d06:bad:b01:10::21",
			"yb2": "3d06:bad:b01:10::22",
			"yb3": "3d06:bad:b01:10::23",
		},
	})
	if err != nil {
		t.Fatalf("encode extra variables: %v", err)
	}

	commandContext, cancel := context.WithTimeout(t.Context(), ansiblePlaybookTimeout)
	defer cancel()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	command := exec.CommandContext(
		commandContext,
		ansiblePlaybook,
		"--inventory",
		"localhost,",
		filepath.Join(workingDirectory, "testdata", "render_tack_override.yml"),
		"--extra-vars",
		string(extraVariables),
	)
	command.Dir = filepath.Join(repositoryRoot, "ansible")
	command.Env = append(
		os.Environ(),
		"ANSIBLE_VAULT_PASSWORD_FILE="+vaultPasswordFile,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		if commandContext.Err() != nil {
			t.Fatalf(
				"render compose override exceeded %s: %v\n%s",
				ansiblePlaybookTimeout, commandContext.Err(), output,
			)
		}
		t.Fatalf("render compose override: %v\n%s", err, output)
	}

	contents, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", outputFile, err)
	}
	var rendered renderedComposeOverride
	if err := yaml.Unmarshal(contents, &rendered); err != nil {
		t.Fatalf("decode rendered override: %v\n%s", err, contents)
	}
	return rendered
}

func TestTackOverrideRendersTheLedgerConnectBound(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	connectTimeoutSeconds := tackConnectTimeoutSeconds(t, repositoryRoot)
	rendered := renderTackOverride(t, repositoryRoot)

	databaseURL := rendered.Services.App.Environment.DatabaseURL
	if databaseURL == "" {
		t.Fatal("rendered override carries no app DATABASE_URL")
	}
	if !strings.Contains(databaseURL, "yb1:5433,yb2:5433,yb3:5433") {
		t.Errorf("app DATABASE_URL = %q, want the three ledger names", databaseURL)
	}
	wantBound := "connect_timeout=" + strconv.Itoa(connectTimeoutSeconds)
	if !strings.Contains(databaseURL, wantBound) {
		t.Errorf(
			"app DATABASE_URL = %q, want the per-name connect bound %q; without it the "+
				"driver waits out the kernel's TCP retry on a lost guest",
			databaseURL, wantBound,
		)
	}
}
