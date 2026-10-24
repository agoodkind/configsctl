package statemigration_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/migrationpause"
)

func TestVerifyRestoredRechecksOriginalPlansAfterItsTerminalCheckpoint(t *testing.T) {
	engine, _ := restorationFixture(t)
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	pause, err := os.ReadFile(engine.PauseFile)
	if err != nil {
		t.Fatal(err)
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.PauseFile, pause, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := engine.RestoreWorkspaces["legacy"]
	originalEnvironment := workspace.Environment
	workspace.Environment = append(slices.Clone(originalEnvironment), "TF_VAR_fixture_value=synthetic-unapplied-original-change")
	engine.RestoreWorkspaces["legacy"] = workspace
	if err := engine.VerifyRestored(t.Context()); err == nil || !strings.Contains(err.Error(), "differs from the frozen original baseline") {
		t.Fatalf("terminal restoration retry skipped the original native plan: %v", err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("a changed original plan released the terminal restoration pause")
	}
	workspace.Environment = originalEnvironment
	engine.RestoreWorkspaces["legacy"] = workspace
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err != nil {
		t.Fatal("a fully rechecked terminal restoration retained its pause")
	}
	requirePrivateRecoveryFiles(t, engine)
}
