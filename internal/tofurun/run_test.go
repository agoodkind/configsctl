package tofurun_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/tofurun"
)

const (
	credentialValue    = "example password for local OpenTofu"
	credentialLabel    = "fixture_idrac_password"
	credentialVariable = "TF_VAR_credential"
	echoModule         = `variable "credential" {
  type = string
}

output "credential" {
  value = var.credential
}
`
)

// TestRunRedactsCallerPatternsInTheRunLog runs real OpenTofu against a local
// module and checks that the run log replaces an explicitly supplied credential.
func TestRunRedactsCallerPatternsInTheRunLog(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(echoModule), 0o600); err != nil {
		t.Fatalf("write the module: %v", err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	environment := append(os.Environ(), "TF_IN_AUTOMATION=1", credentialVariable+"="+credentialValue)
	ctx := t.Context()

	initRequest := tofurun.Request{
		Root: root, Args: []string{"init", "-input=false"}, Environment: environment,
		DisplayArgs: nil, Patterns: nil,
	}
	initialized, err := tofurun.Run(ctx, initRequest)
	if err != nil || initialized.ExitCode != 0 {
		t.Fatalf("tofu init: exit code %d, err %v", initialized.ExitCode, err)
	}

	patterns := []redact.Pattern{{Value: []byte(credentialValue), Label: credentialLabel}}
	planRequest := tofurun.Request{
		Root: root, Args: []string{"plan", "-input=false", "-no-color"}, Environment: environment,
		DisplayArgs: nil, Patterns: patterns,
	}
	planned, err := tofurun.Run(ctx, planRequest)
	if err != nil || planned.ExitCode != 0 {
		t.Fatalf("tofu plan: exit code %d, err %v", planned.ExitCode, err)
	}
	if planned.LogPath == "" {
		t.Fatal("tofu plan wrote to the terminal, want a run log")
	}
	body, err := os.ReadFile(planned.LogPath)
	if err != nil {
		t.Fatalf("read the run log: %v", err)
	}
	logText := string(body)
	if strings.Contains(logText, credentialValue) {
		t.Fatal("the run log contains the caller's credential value")
	}
	if !strings.Contains(logText, "<redacted:"+credentialLabel+">") {
		t.Fatalf("the run log lacks the redaction marker for %s:\n%s", credentialLabel, logText)
	}
}
