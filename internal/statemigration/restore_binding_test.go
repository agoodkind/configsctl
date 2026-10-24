package statemigration_test

import (
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/migrationpause"
)

func TestVerifyRestoredRequiresTheOriginalConfigurationBinding(t *testing.T) {
	engine, _ := restorationFixture(t)
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	original := engine.RestoreWorkspaces
	engine.RestoreWorkspaces = nil
	if err := engine.VerifyRestored(t.Context()); err == nil || !strings.Contains(err.Error(), "lacks its reviewed original configuration") {
		t.Fatalf("restoration verification accepted a missing original binding: %v", err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("missing original plan verification released the write pause")
	}
	engine.RestoreWorkspaces = original
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
}
