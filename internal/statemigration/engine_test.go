package statemigration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/migrationpause"
	"goodkind.io/configsctl/internal/statemigration"
)

func migrationFixture(t *testing.T) (statemigration.Engine, statemigration.StateInfo) {
	t.Helper()
	source := workspaceFixture(t, "source")
	destination := workspaceFixture(t, "destination")
	nativeCommand(t, source, "apply", "-auto-approve", "-input=false", "-no-color")
	private := privateFixtureDir(t)
	before, err := statemigration.Pull(context.Background(), source, filepath.Join(private, "reviewed.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	engine := statemigration.Engine{
		Manifest: statemigration.Manifest{
			Version: 1, Commit: commit, RestoreCommit: strings.Repeat("b", 40), ConfigurationDirectory: source.Directory,
			Workspaces: []statemigration.WorkspaceSpec{
				{Selector: "legacy", Backend: "local:legacy", Before: before},
				{Selector: "application", Backend: "local:application"},
			},
			Moves: []statemigration.Transfer{
				{Source: "legacy", Address: "terraform_data.original", Destination: "application", NewAddress: "terraform_data.renamed"},
			},
		},
		CurrentCommit:     commit,
		Directory:         filepath.Join(private, "migration"),
		PauseFile:         filepath.Join(private, "pause.json"),
		Workspaces:        map[string]statemigration.Workspace{"legacy": source, "application": destination},
		RestoreWorkspaces: map[string]statemigration.Workspace{"legacy": source},
	}
	return engine, before
}

func TestCompleteMigrationKeepsNativeIdentityAndReleasesPauseAfterPlans(t *testing.T) {
	ctx := context.Background()
	engine, before := migrationFixture(t)
	logPath := filepath.Join(privateFixtureDir(t), "native.log")
	baseline := []byte("synthetic native log baseline\n")
	if err := os.WriteFile(logPath, baseline, 0o600); err != nil {
		t.Fatal(err)
	}
	for selector, workspace := range engine.Workspaces {
		workspace.Environment = append(workspace.Environment, "TF_LOG=TRACE", "TF_LOG_CORE=TRACE", "TF_LOG_PROVIDER=TRACE", "TF_LOG_PATH="+logPath, "TF_TEMP_LOG_PATH="+logPath)
		engine.Workspaces[selector] = workspace
	}
	if err := engine.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err != nil {
		t.Fatal("preparation paused ordinary deployment")
	}
	if err := engine.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("ordinary deployment passed during an unverified migration")
	}
	if err := engine.Verify(ctx); err == nil {
		t.Fatal("verification released the pause while the legacy root still declared the transferred resource")
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("a failed independent plan removed the durable write pause")
	}
	retired, err := os.ReadFile(filepath.Join("testdata", "retired.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine.Workspaces["legacy"].Directory, "main.tf"), retired, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := engine.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil || !bytes.Equal(logged, baseline) {
		t.Fatal("migration inherited native logging settings or modified the external log")
	}
	private := privateFixtureDir(t)
	after, err := statemigration.Pull(ctx, engine.Workspaces["application"], filepath.Join(private, "after.tfstate"))
	if err != nil || after.Records["terraform_data.renamed"] != before.Records["terraform_data.original"] {
		t.Fatalf("the complete migration changed instance identity or provider state: %v", err)
	}
	if err := filepath.WalkDir(engine.Directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(syntheticMarker)) {
			t.Fatal("the retained migration files contain the synthetic plaintext marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDependentInstancesKeepIdentityAfterNativeDependencyInvalidation(t *testing.T) {
	ctx := context.Background()
	engine, _ := migrationFixture(t)
	source := workspaceFixture(t, "dependent")
	destination := workspaceFixture(t, "dependent_destination")
	nativeCommand(t, source, "apply", "-auto-approve", "-input=false", "-no-color")
	before, err := statemigration.Pull(ctx, source, filepath.Join(privateFixtureDir(t), "dependent.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Dependencies["terraform_data.child"]) == 0 {
		t.Fatal("the real native fixture did not record its dependency")
	}
	engine.Workspaces["legacy"] = source
	engine.Workspaces["application"] = destination
	engine.Manifest.Workspaces[0].Before = before
	engine.Manifest.Moves = []statemigration.Transfer{
		{Source: "legacy", Address: "terraform_data.parent", Destination: "application", NewAddress: "terraform_data.new_parent"},
		{Source: "legacy", Address: "terraform_data.child", Destination: "application", NewAddress: "terraform_data.new_child"},
	}
	if err := engine.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	retired, err := os.ReadFile(filepath.Join("testdata", "retired.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Directory, "main.tf"), retired, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := engine.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := statemigration.Pull(ctx, destination, filepath.Join(privateFixtureDir(t), "dependent-after.tfstate"))
	if err != nil || before.Records["terraform_data.parent"] != after.Records["terraform_data.new_parent"] ||
		before.Records["terraform_data.child"] != after.Records["terraform_data.new_child"] {
		t.Fatalf("the dependent native transfer changed resource identities or provider records: %v", err)
	}
}

func TestMigrationRefusesAChangedSourceAndRetainsPause(t *testing.T) {
	ctx := context.Background()
	engine, _ := migrationFixture(t)
	if err := engine.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	source := engine.Workspaces["legacy"]
	source.Environment = append(source.Environment, "TF_VAR_fixture_value=synthetic-concurrent-change")
	nativeCommand(t, source, "apply", "-auto-approve", "-input=false", "-no-color")
	if err := engine.Execute(ctx); err == nil {
		t.Fatal("migration overwrote a source update made after preparation")
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("migration released its pause after detecting a stale source")
	}
	private := privateFixtureDir(t)
	if _, err := statemigration.Pull(ctx, engine.Workspaces["application"], filepath.Join(private, "destination.tfstate")); err == nil {
		t.Fatal("a stale source migration wrote the destination backend")
	}
}

func TestManifestRejectsDuplicateAndIncompleteOwnership(t *testing.T) {
	engine, _ := migrationFixture(t)
	duplicate := engine.Manifest
	duplicate.Moves = append(duplicate.Moves, duplicate.Moves[0])
	encoded, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statemigration.DecodeManifest(encoded); err == nil {
		t.Fatal("the manifest accepted duplicate instance ownership")
	}
	incomplete := engine.Manifest
	incomplete.Workspaces[0].Before.Records["terraform_data.omitted"] = "synthetic-digest"
	encoded, err = json.Marshal(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statemigration.DecodeManifest(encoded); err == nil {
		t.Fatal("the manifest accepted an omitted existing instance")
	}
}
