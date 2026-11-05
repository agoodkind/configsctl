package main_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHostLockServerRejectsCallerDirectoryBeforeVaultSetup(t *testing.T) {
	command := exec.CommandContext(t.Context(), configsctlBinary, "host-lock", "serve", "--principal", "planning")
	command.Dir = t.TempDir()
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	command.Stdin = strings.NewReader(`{"protocol_version":1,"operation":"acquire","run_id":"attempt","lease_seconds":30,"directory":"/tmp"}`)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "supported JSON fields") {
		t.Fatalf("host-lock serve did not reject the caller directory before vault setup: %v: %s", err, output)
	}
}
